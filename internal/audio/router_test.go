package audio

import (
	ma "github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"testing"
)

type captureSender map[uint32][]ma.Delivery

func (s captureSender) SendAudio(id uint32, d ma.Delivery, _ *ma.EncodingCache) error {
	s[id] = append(s[id], d)
	return nil
}
func TestRouteContextsMergeAndGates(t *testing.T) {
	sends := captureSender{}
	cfg := RouterConfig{Sender: sends, GetChan: func(uint32) uint32 { return 10 }, GetUsersInChan: func(id uint32) []uint32 {
		if id == 10 {
			return []uint32{1, 2}
		}
		return []uint32{3}
	}, GetLinkedChans: func(uint32) []uint32 { return []uint32{11, 12} }, GetListenersInChan: func(uint32) []uint32 { return []uint32{2, 4, 5} }, GetListenerVolume: func(sid, cid uint32) float32 {
		if cid == 11 {
			return 2.5
		}
		return .5
	}, FilterRecipient: func(_, sid uint32) bool { return sid != 5 }, CanSenderSpeakInChan: func(_, cid uint32) bool { return cid != 12 }}
	router := NewRouterWithConfig(cfg)
	frame := ma.Frame{Codec: ma.CodecOpus, SenderSession: 999, OpusData: []byte{1}}
	if e := router.Route(1, frame); e != nil {
		t.Fatal(e)
	}
	for _, id := range []uint32{2, 3, 4} {
		if len(sends[id]) != 1 {
			t.Fatalf("接收者 %d 次数 %d", id, len(sends[id]))
		}
		if sends[id][0].SenderSession != 1 {
			t.Fatal("身份未覆盖")
		}
	}
	if sends[2][0].Context != ma.ContextNormal || sends[3][0].Context != ma.ContextNormal || sends[4][0].Context != ma.ContextListen || sends[2][0].VolumeAdjustment != 2.5 || sends[4][0].VolumeAdjustment != 2.5 {
		t.Fatal(sends)
	}
	if len(sends[1]) != 0 || len(sends[5]) != 0 {
		t.Fatal("发送者或 deaf 收到普通音频")
	}
	cfg.CanSenderSpeak = func(uint32) bool { return false }
	cfg.Sender = captureSender{}
	router = NewRouterWithConfig(cfg)
	_ = router.Route(1, frame)
	if len(cfg.Sender.(captureSender)) != 0 {
		t.Fatal("未执行静音门控")
	}
	frame.Target = 31
	_ = router.Route(1, frame)
	if cfg.Sender.(captureSender)[1][0].Context != ma.ContextNormal {
		t.Fatal("Loopback 上下文错误")
	}
}
