package mumble

import (
	"net"

	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// voiceTargetSpec 保留频道语义；显式用户目标绑定连接身份，不能随 session 复用转移。
// 发布后不再修改，所有读取和更新均在 connMu 下进行。
type voiceTargetSpec struct {
	owner    *connection.Conn
	sessions map[uint32]*connection.Conn
	channels []messages.VoiceTargetTarget
}

type voiceRecipient struct {
	session uint32
	conn    *connection.Conn
	addr    net.Addr
}

func (s *Server) storeVoiceTarget(c *connection.Conn, vt messages.VoiceTarget) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	sid := c.SessionID()
	if s.conns[sid] != c {
		return
	}
	spec := voiceTargetSpec{owner: c, sessions: make(map[uint32]*connection.Conn)}
	for _, target := range vt.Targets {
		for _, id := range target.Session {
			if recipient := s.conns[id]; recipient != nil && s.users.Exists(id) {
				spec.sessions[id] = recipient
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
	if !ok || spec.owner != s.conns[sessionID] || spec.owner.State() != connection.StateActive {
		return nil
	}
	speaker, ok := s.users.Snapshot(sessionID)
	if !ok || speaker.Mute || speaker.Suppress || speaker.SelfMute {
		return nil
	}
	var recipients []voiceRecipient
	seen := make(map[uint32]bool)
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
	add := func(u mumble.User) {
		if u.SessionID == sessionID || seen[u.SessionID] || u.Deaf || u.SelfDeaf {
			return
		}
		c := s.conns[u.SessionID]
		if c == nil || c.State() != connection.StateActive {
			return
		}
		seen[u.SessionID] = true
		addr, _ := s.addrBySession.Load(u.SessionID)
		a, _ := addr.(net.Addr)
		recipients = append(recipients, voiceRecipient{u.SessionID, c, a})
	}
	for sid, expected := range spec.sessions {
		if s.conns[sid] != expected {
			continue
		}
		if u, ok := s.users.Snapshot(sid); ok && canWhisper(u.ChannelID) {
			add(u)
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
				add(u)
			}
			for _, sid := range s.listeners.SessionIDsIn(cid) {
				u, ok := s.users.Snapshot(sid)
				if !ok || !groupAllows(u) {
					continue
				}
				add(u)
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

func (s *Server) routeVoiceTarget(sessionID uint32, targetID uint8, packet []byte) error {
	for _, r := range s.resolveVoiceTarget(sessionID, targetID) {
		var addr interface{}
		if r.addr != nil {
			addr = r.addr
		}
		_ = s.sendAudioTo(r.session, r.conn, addr, packet)
	}
	return nil
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
				sessions := make(map[uint32]*connection.Conn, len(spec.sessions))
				for sid, c := range spec.sessions {
					if sid != sessionID {
						sessions[sid] = c
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
