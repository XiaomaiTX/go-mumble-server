package mumble

import (
	ma "github.com/dchote/go-mumble-server/pkg/mumble/audio"

	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/audio"
	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// voiceTargetSpec 保留频道语义；显式用户目标绑定连接身份，不能随 session 复用转移。
// 发布后不再修改，所有读取和更新均在 connMu 下进行。
type voiceTargetSpec struct {
	owner    cluster.SessionRef
	sessions map[uint32]cluster.SessionRef
	channels []messages.VoiceTargetTarget
}

type voiceRecipient struct {
	session  uint32
	ref      cluster.SessionRef
	delivery ma.Delivery
}

func (s *Server) storeVoiceTarget(c Peer, vt messages.VoiceTarget) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	sid := c.SessionID()
	owner, ok := s.users.Snapshot(sid)
	if !ok || owner.SessionGeneration != c.SessionGeneration() {
		return
	}
	ownerRef := cluster.SessionRef{SessionID: sid, Generation: owner.SessionGeneration}
	// Eligibility is registry lifecycle, never the local conn table: a remote
	// edge session (no local conn) can own voice targets too.
	if _, ok := s.registry.Snapshot(ownerRef); !ok {
		return
	}
	spec := voiceTargetSpec{owner: ownerRef, sessions: make(map[uint32]cluster.SessionRef)}
	for _, target := range vt.Targets {
		for _, id := range target.Session {
			if recipient, found := s.users.Snapshot(id); found {
				ref := cluster.SessionRef{SessionID: id, Generation: recipient.SessionGeneration}
				if _, ok := s.registry.Snapshot(ref); ok {
					spec.sessions[id] = ref
				}
			}
		}
		if target.HasChannelID {
			if _, ok := s.chans.GetChannel(target.ChannelID); ok {
				target.Session = nil
				spec.channels = append(spec.channels, target)
			}
		}
	}
	merged := make(map[uint8]voiceTargetSpec)
	if v, ok := s.voiceTargets.Load(sid); ok {
		for id, old := range v.(map[uint8]voiceTargetSpec) {
			merged[id] = old
		}
	}
	if len(spec.sessions) == 0 && len(spec.channels) == 0 {
		delete(merged, uint8(vt.ID))
	} else {
		merged[uint8(vt.ID)] = spec
	}
	if len(merged) == 0 {
		s.voiceTargets.Delete(sid)
	} else {
		s.voiceTargets.Store(sid, merged)
	}
}

// resolveVoiceTarget 每个语音包重新解析频道、组和权限，同时固定本次发送的连接。
func (s *Server) resolveVoiceTarget(sessionID uint32, targetID uint8) []voiceRecipient {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	v, ok := s.voiceTargets.Load(sessionID)
	if !ok {
		return nil
	}
	spec, ok := v.(map[uint8]voiceTargetSpec)[targetID]
	if !ok || !s.registry.Active(spec.owner) {
		return nil
	}
	speaker, ok := s.users.Snapshot(sessionID)
	if !ok || speaker.Mute || speaker.Suppress || speaker.SelfMute {
		return nil
	}
	var recipients []voiceRecipient
	seen := make(map[uint32]int)
	permitted := make(map[uint32]bool)
	checked := make(map[uint32]bool)
	canWhisper := func(cid uint32) bool {
		if !checked[cid] {
			checked[cid] = true
			if s.acl == nil {
				permitted[cid] = s.aclCheck(acl.SubjectOf(speaker), cid, mumble.PermissionWhisper)
			} else {
				permitted[cid] = s.acl.CheckCurrent(acl.SubjectOf(speaker), cid, mumble.PermissionWhisper)
			}
		}
		return permitted[cid]
	}
	add := func(u mumble.User, context ma.Context, volume float32) {
		if u.SessionID == sessionID || u.Deaf || u.SelfDeaf {
			return
		}
		ref := cluster.SessionRef{SessionID: u.SessionID, Generation: u.SessionGeneration}
		if !s.registry.Active(ref) {
			return
		}
		d := ma.Delivery{Context: context, VolumeAdjustment: volume}
		if index, ok := seen[u.SessionID]; ok {
			recipients[index].delivery = ma.MergeDelivery(recipients[index].delivery, d)
			return
		}
		seen[u.SessionID] = len(recipients)
		recipients = append(recipients, voiceRecipient{session: u.SessionID, ref: ref, delivery: d})
	}
	for sid, expected := range spec.sessions {
		if !s.registry.Active(expected) {
			continue
		}
		if u, ok := s.users.Snapshot(sid); ok && u.SessionGeneration == expected.Generation && canWhisper(u.ChannelID) {
			add(u, ma.ContextWhisper, 1)
		}
	}
	for _, target := range spec.channels {
		if _, ok := s.chans.GetChannel(target.ChannelID); !ok {
			continue
		}
		channels := map[uint32]bool{target.ChannelID: true}
		if target.Links {
			for _, id := range s.chans.ConnectedChannelIDs(target.ChannelID) {
				channels[id] = true
			}
		}
		if target.Children {
			for _, id := range s.chans.SubtreeIDs(target.ChannelID) {
				channels[id] = true
			}
		}
		for cid := range channels {
			if !canWhisper(cid) {
				continue
			}
			groupAllows := func(u mumble.User) bool {
				// 组过滤在被监听频道上解析，监听者与占用者同等对待（匹配 murmur）。
				if target.Group == "" {
					return true
				}
				return s.acl != nil && s.acl.InGroup(u, cid, target.Group)
			}
			for _, u := range s.users.SnapshotByChannel(cid) {
				if !groupAllows(u) {
					continue
				}
				add(u, ma.ContextShout, 1)
			}
			for _, sid := range s.listeners.SessionIDsIn(cid) {
				u, ok := s.users.Snapshot(sid)
				if !ok || !groupAllows(u) {
					continue
				}
				add(u, ma.ContextListen, s.listeners.Volume(sid, cid))
			}
		}
	}
	return recipients
}

func (s *Server) getVoiceTargetRecipients(sessionID uint32, targetID uint8) []uint32 {
	var ids []uint32
	for _, r := range s.resolveVoiceTarget(sessionID, targetID) {
		ids = append(ids, r.session)
	}
	return ids
}

func (s *Server) resolveVoiceTargetCanonical(sessionID uint32, targetID uint8, frame ma.Frame) []audio.Recipient {
	resolved := s.resolveVoiceTarget(sessionID, targetID)
	out := make([]audio.Recipient, 0, len(resolved))
	for _, recipient := range resolved {
		recipient.delivery.Frame = frame
		out = append(out, audio.Recipient{SessionID: recipient.session, Session: recipient.ref, Delivery: recipient.delivery})
	}
	return out
}

// removeVoiceTargetsLocked 在断线时释放其他目标对旧连接的引用；调用方持有 connMu 写锁。
func (s *Server) removeVoiceTargetsLocked(sessionID uint32) {
	s.voiceTargets.Delete(sessionID)
	s.voiceTargets.Range(func(key, value interface{}) bool {
		targets := value.(map[uint8]voiceTargetSpec)
		updated := make(map[uint8]voiceTargetSpec, len(targets))
		changed := false
		for id, spec := range targets {
			if _, ok := spec.sessions[sessionID]; ok {
				changed = true
				sessions := make(map[uint32]cluster.SessionRef, len(spec.sessions))
				for sid, ref := range spec.sessions {
					if sid != sessionID {
						sessions[sid] = ref
					}
				}
				spec.sessions = sessions
			}
			if len(spec.sessions) > 0 || len(spec.channels) > 0 {
				updated[id] = spec
			}
		}
		if changed {
			if len(updated) == 0 {
				s.voiceTargets.Delete(key)
			} else {
				s.voiceTargets.Store(key, updated)
			}
		}
		return true
	})
}
