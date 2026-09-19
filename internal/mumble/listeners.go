package mumble

import (
	"sort"
	"sync"

	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// listenerManager 维护 Mumble 1.4+ 频道监听状态：双向索引支撑两条查询路径——按会话
// 枚举监听的频道（登录同步、断线清理）与按频道枚举监听者（语音路由）。音量按
// (会话, 频道) 存储，随监听关系存亡；本服务端只同步状态，增益由客户端本地应用。
//
// 零值可用（map 惰性创建，测试可用 &Server{} 字面量构造）。只持有自身锁，持锁期间
// 不回调 Server 的其他管理器——先快照 ID，锁外解析（voicetarget.go 同款纪律）。
type listenerManager struct {
	mu        sync.RWMutex
	bySession map[uint32]map[uint32]struct{} // session -> 监听的频道集
	byChannel map[uint32]map[uint32]struct{} // channel -> 监听者 session 集
	volumes   map[uint32]map[uint32]float32  // session -> channel -> 音量
}

// Add 记录一条监听关系；已存在时返回 false。
func (m *listenerManager) Add(session, channel uint32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bySession == nil {
		m.bySession = make(map[uint32]map[uint32]struct{})
		m.byChannel = make(map[uint32]map[uint32]struct{})
	}
	if m.bySession[session] == nil {
		m.bySession[session] = make(map[uint32]struct{})
	}
	if m.byChannel[channel] == nil {
		m.byChannel[channel] = make(map[uint32]struct{})
	}
	if _, ok := m.bySession[session][channel]; ok {
		return false
	}
	m.bySession[session][channel] = struct{}{}
	m.byChannel[channel][session] = struct{}{}
	return true
}

// Remove 移除一条监听关系（连带音量）；原本不存在时返回 false。
func (m *listenerManager) Remove(session, channel uint32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	chans, ok := m.bySession[session]
	if !ok {
		return false
	}
	if _, ok := chans[channel]; !ok {
		return false
	}
	delete(chans, channel)
	if len(chans) == 0 {
		delete(m.bySession, session)
	}
	sessions := m.byChannel[channel]
	delete(sessions, session)
	if len(sessions) == 0 {
		delete(m.byChannel, channel)
	}
	delete(m.volumes[session], channel)
	if len(m.volumes[session]) == 0 {
		delete(m.volumes, session)
	}
	return true
}

// RemoveAllFor 清除一个会话的全部监听关系（断线清理），返回被移除的频道。
func (m *listenerManager) RemoveAllFor(session uint32) []uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	chans, ok := m.bySession[session]
	if !ok {
		return nil
	}
	removed := make([]uint32, 0, len(chans))
	for cid := range chans {
		sessions := m.byChannel[cid]
		delete(sessions, session)
		if len(sessions) == 0 {
			delete(m.byChannel, cid)
		}
		removed = append(removed, cid)
	}
	delete(m.bySession, session)
	delete(m.volumes, session)
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	return removed
}

// RemoveChannel 清除对一个频道的全部监听（频道删除清理），返回受影响的会话。
func (m *listenerManager) RemoveChannel(channel uint32) []uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessions, ok := m.byChannel[channel]
	if !ok {
		return nil
	}
	affected := make([]uint32, 0, len(sessions))
	for sid := range sessions {
		chans := m.bySession[sid]
		delete(chans, channel)
		if len(chans) == 0 {
			delete(m.bySession, sid)
		}
		delete(m.volumes[sid], channel)
		if len(m.volumes[sid]) == 0 {
			delete(m.volumes, sid)
		}
		affected = append(affected, sid)
	}
	delete(m.byChannel, channel)
	sort.Slice(affected, func(i, j int) bool { return affected[i] < affected[j] })
	return affected
}

// ChannelsFor 返回会话正在监听的频道（升序）。
func (m *listenerManager) ChannelsFor(session uint32) []uint32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	chans, ok := m.bySession[session]
	if !ok {
		return nil
	}
	ids := make([]uint32, 0, len(chans))
	for cid := range chans {
		ids = append(ids, cid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// SessionIDsIn 返回正在监听某频道的会话（升序）。
func (m *listenerManager) SessionIDsIn(channel uint32) []uint32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions, ok := m.byChannel[channel]
	if !ok {
		return nil
	}
	ids := make([]uint32, 0, len(sessions))
	for sid := range sessions {
		ids = append(ids, sid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// IsListening 报告监听关系是否存在。
func (m *listenerManager) IsListening(session, channel uint32) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.bySession[session][channel]
	return ok
}

// CountInChannel 返回某频道的监听者数量（限额检查用）。
func (m *listenerManager) CountInChannel(channel uint32) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byChannel[channel])
}

// CountForSession 返回某会话监听的频道数量（限额检查用）。
func (m *listenerManager) CountForSession(session uint32) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.bySession[session])
}

// SetVolume 记录监听音量；仅对已存在的监听关系生效（murmur 对未监听频道的音量
// 设置告警并忽略）。
func (m *listenerManager) SetVolume(session, channel uint32, v float32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.bySession[session][channel]; !ok {
		return
	}
	if m.volumes == nil {
		m.volumes = make(map[uint32]map[uint32]float32)
	}
	if m.volumes[session] == nil {
		m.volumes[session] = make(map[uint32]float32)
	}
	m.volumes[session][channel] = v
}

// ListeningVolumes 返回会话的全部音量调节（升序，登录同步用）。
func (m *listenerManager) ListeningVolumes(session uint32) []messages.VolumeAdjustment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	vols, ok := m.volumes[session]
	if !ok || len(vols) == 0 {
		return nil
	}
	result := make([]messages.VolumeAdjustment, 0, len(vols))
	for cid, v := range vols {
		result = append(result, messages.VolumeAdjustment{ListeningChannel: cid, VolumeAdjustment: v})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ListeningChannel < result[j].ListeningChannel })
	return result
}

// applyListening 处理 UserState 携带的监听增量。逐频道校验 Listen 权限与数量限额，
// 被拒项只向发送者返回 PermissionDenied 并跳过——其余频道照常生效（部分成功，匹配
// murmur 的 Messages.cpp msgUserState）。已应用的增量以 truthful delta 广播：只广播
// 真正生效的变更，而非回显原始请求（本服务端不误报未建立的状态）。音量调节只同步
// 给监听者本人，对应 murmur broadcastListenerVolumeAdjustments=false 的默认值。
func (s *Server) applyListening(c *connection.Conn, target mumble.User, add, remove []uint32, vols []messages.VolumeAdjustment) {
	var appliedAdd, appliedRemove []uint32
	var appliedVols []messages.VolumeAdjustment
	subject := acl.SubjectOf(target)
	for _, cid := range add {
		if _, ok := s.chans.GetChannel(cid); !ok {
			continue
		}
		if !s.aclCheck(subject, cid, mumble.PermissionListen) {
			_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{
				ChannelID: cid, Session: target.SessionID,
				Permission: uint32(mumble.PermissionListen), Type: messages.DenyPermission,
			})
			continue
		}
		if s.cfg != nil && s.cfg.MaxChannelListeners > 0 &&
			s.listeners.CountInChannel(cid)+1 > s.cfg.MaxChannelListeners {
			_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{
				ChannelID: cid, Session: target.SessionID,
				Type: messages.DenyChannelListenerLimit, Reason: "Channel listener limit reached",
			})
			continue
		}
		if s.cfg != nil && s.cfg.MaxListenersPerUser > 0 &&
			s.listeners.CountForSession(target.SessionID)+1 > s.cfg.MaxListenersPerUser {
			_ = c.WriteMessage(protocol.MessagePermissionDenied, &messages.PermissionDenied{
				ChannelID: cid, Session: target.SessionID,
				Type: messages.DenyUserListenerLimit, Reason: "User listener limit reached",
			})
			continue
		}
		if s.listeners.Add(target.SessionID, cid) {
			appliedAdd = append(appliedAdd, cid)
		}
	}
	for _, cid := range remove {
		if s.listeners.Remove(target.SessionID, cid) {
			appliedRemove = append(appliedRemove, cid)
		}
	}
	for _, va := range vols {
		if !s.listeners.IsListening(target.SessionID, va.ListeningChannel) {
			continue
		}
		s.listeners.SetVolume(target.SessionID, va.ListeningChannel, va.VolumeAdjustment)
		appliedVols = append(appliedVols, va)
	}
	if len(appliedAdd) == 0 && len(appliedRemove) == 0 && len(appliedVols) == 0 {
		return
	}
	base := messages.UserState{
		Session:                target.SessionID,
		Actor:                  c.SessionID(),
		SetFields:              messages.UserStateSetSession | messages.UserStateSetActor,
		ListeningChannelAdd:    appliedAdd,
		ListeningChannelRemove: appliedRemove,
	}
	// 发送者收到含音量的完整版，其余客户端收到去掉音量的版本。
	full := base
	full.ListeningVolumeAdjustment = appliedVols
	_ = c.WriteMessage(protocol.MessageUserState, &full)
	s.Broadcast(c.SessionID(), protocol.MessageUserState, &base)
}

// Volume 返回监听关系的音量 factor；未设置时为单位增益。
func (m *listenerManager) Volume(session, channel uint32) float32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if v, ok := m.volumes[session][channel]; ok {
		return v
	}
	return 1
}
