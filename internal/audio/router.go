package audio

import (
	"github.com/dchote/go-mumble-server/internal/cluster"
	ma "github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"log/slog"
	"sync"
	"sync/atomic"
)

type Recipient struct {
	SessionID uint32
	Session   cluster.SessionRef
	Delivery  ma.Delivery
}
type BatchSender interface {
	SendVoice(sender cluster.SessionRef, frame ma.Frame, recipients []Recipient) error
}
type RouterConfig struct {
	Sender               BatchSender
	GetChan              func(uint32) uint32
	GetUsersInChan       func(uint32) []uint32
	ResolveVoiceTarget   func(uint32, uint8, ma.Frame) []Recipient
	ResolveSession       func(uint32) (cluster.SessionRef, bool)
	GetLinkedChans       func(uint32) []uint32
	GetListenersInChan   func(uint32) []uint32
	GetListenerVolume    func(uint32, uint32) float32
	FilterRecipient      func(uint32, uint32) bool
	CanSenderSpeak       func(uint32) bool
	CanSenderSpeakInChan func(uint32, uint32) bool
	VoiceDebug           bool
}
type Router struct {
	mu     sync.RWMutex
	config RouterConfig
}

func NewRouterWithConfig(cfg RouterConfig) *Router { return &Router{config: cfg} }
func (r *Router) SetVoiceDebug(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.config.VoiceDebug = enabled
}

var routeLogCount atomic.Uint64

// Route 只处理音频事实，接收上下文由服务端路由生成。
func (r *Router) Route(sender uint32, frame ma.Frame) error {
	r.mu.RLock()
	cfg := r.config
	r.mu.RUnlock()
	ref := cluster.SessionRef{SessionID: sender}
	if cfg.ResolveSession != nil {
		var ok bool
		ref, ok = cfg.ResolveSession(sender)
		if !ok {
			return nil
		}
	}
	return r.RouteRef(ref, frame)
}

func (r *Router) RouteRef(senderRef cluster.SessionRef, frame ma.Frame) error {
	sender := senderRef.SessionID
	r.mu.RLock()
	cfg := r.config
	r.mu.RUnlock()
	if cfg.Sender == nil || frame.Target > 31 {
		return nil
	}
	frame.SenderSession = sender
	if frame.Target > 0 && frame.Target < 31 {
		if cfg.ResolveVoiceTarget != nil {
			return cfg.Sender.SendVoice(senderRef, frame, cfg.ResolveVoiceTarget(sender, uint8(frame.Target), frame))
		}
		return nil
	}
	if frame.Target == 31 {
		return cfg.Sender.SendVoice(senderRef, frame, []Recipient{{SessionID: sender, Session: senderRef, Delivery: ma.Delivery{Frame: frame}}})
	}
	if cfg.CanSenderSpeak != nil && !cfg.CanSenderSpeak(sender) {
		return nil
	}
	if cfg.GetChan == nil || cfg.GetUsersInChan == nil {
		return nil
	}
	recipients := make(map[uint32]ma.Delivery)
	add := func(sid uint32, context ma.Context, volume float32) {
		if sid == sender || (cfg.FilterRecipient != nil && !cfg.FilterRecipient(sender, sid)) {
			return
		}
		d := ma.Delivery{Frame: frame, Context: context, VolumeAdjustment: volume}
		if old, ok := recipients[sid]; ok {
			d = ma.MergeDelivery(old, d)
		}
		recipients[sid] = d
	}
	ch := cfg.GetChan(sender)
	channels := []uint32{ch}
	if cfg.GetLinkedChans != nil {
		channels = append(channels, cfg.GetLinkedChans(ch)...)
	}
	for _, cid := range channels {
		if cfg.CanSenderSpeakInChan != nil && !cfg.CanSenderSpeakInChan(sender, cid) {
			continue
		}
		for _, sid := range cfg.GetUsersInChan(cid) {
			add(sid, ma.ContextNormal, 1)
		}
		if cfg.GetListenersInChan != nil {
			for _, sid := range cfg.GetListenersInChan(cid) {
				volume := float32(1)
				if cfg.GetListenerVolume != nil {
					volume = cfg.GetListenerVolume(sid, cid)
				}
				add(sid, ma.ContextListen, volume)
			}
		}
	}
	canonical := make([]Recipient, 0, len(recipients))
	for sid, d := range recipients {
		ref := cluster.SessionRef{SessionID: sid}
		if cfg.ResolveSession != nil {
			var ok bool
			ref, ok = cfg.ResolveSession(sid)
			if !ok {
				continue
			}
		}
		canonical = append(canonical, Recipient{SessionID: sid, Session: ref, Delivery: d})
	}
	_ = cfg.Sender.SendVoice(senderRef, frame, canonical)
	n := routeLogCount.Add(1)
	if cfg.VoiceDebug && (n <= 5 || n%50 == 0) {
		slog.Info("audio route", "sender", sender, "recipients", len(recipients))
	}

	return nil
}
