package acl

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/dchote/go-mumble-server/internal/database/models"
	"github.com/dchote/go-mumble-server/pkg/mumble"
)

type selector struct {
	name                      string
	invert, here, token, cert bool
}

func parseSelector(name string) selector {
	s := selector{}
	for len(name) > 0 {
		switch name[0] {
		case '!':
			s.invert = true
		case '~':
			s.here = true
		case '#':
			s.token = true
		case '$':
			s.cert = true
		default:
			s.name = name
			return s
		}
		name = name[1:]
	}
	return s
}
func (e *Evaluator) entryMatches(row models.ChannelACL, id, target uint32, u *mumble.User) (bool, error) {
	if row.UserID != nil {
		// 匿名 ID 0 不能匹配注册账号；保留 API synthetic ID 的历史位表示。
		match := id != 0 && *row.UserID != -1 && uint32(*row.UserID) == id
		if row.Invert {
			match = !match
		}
		return match, nil
	}
	s := parseSelector(row.GroupName)
	if row.AccessToken != "" {
		s = selector{name: row.AccessToken, token: true}
	}
	s.here = s.here || row.EvalHere
	s.invert = s.invert || row.Invert
	return e.matchSelector(s, id, target, uint32(row.ChannelID), u)
}
func (e *Evaluator) userHasToken(u *mumble.User, token string) bool {
	if u == nil || token == "" {
		return false
	}
	for _, t := range u.AccessTokens {
		if strings.EqualFold(t, token) {
			return true
		}
	}
	return false
}
func (e *Evaluator) matchSelector(s selector, id, target, definition uint32, u *mumble.User) (bool, error) {
	if s.name == "" {
		return false, nil
	}
	context := target
	if s.here {
		context = definition
	}
	match := false
	switch {
	case s.token:
		match = e.userHasToken(u, s.name)
	case s.cert:
		match = u != nil && u.CertHash == s.name
	case s.name == "none":
	case s.name == "all":
		match = true
	case s.name == "auth":
		match = id > 0
	case s.name == "strong":
		match = u != nil && u.CertificateVerified
	case s.name == "in":
		match = u != nil && u.ChannelID == context
	case s.name == "out":
		match = u == nil || u.ChannelID != context
	case s.name == "sub" || strings.HasPrefix(s.name, "sub,"):
		match = e.matchSub(s.name, target, context, u)
	default:
		if u != nil && u.ExternalIdentity && slices.Contains(u.ExternalGroups, s.name) {
			match = true
		} else if s.name == "admin" && IsAPIUserID(id) {
			var err error
			match, err = e.apiAdmin(id)
			if err != nil {
				return false, err
			}
		} else {
			var err error
			match, err = e.storedGroup(id, s.name, context, u)
			if err != nil {
				return false, err
			}
		}
	}
	if s.invert {
		match = !match
	}
	return match, nil
}

// Qt QString::toInt 的失败结果是 0，范围是有符号 32 位。
func selectorInt(value string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0
	}
	return n
}
func (e *Evaluator) matchSub(name string, target, context uint32, u *mumble.User) bool {
	if u == nil {
		return false
	}
	args := strings.Split(strings.TrimPrefix(name, "sub,"), ",")
	if name == "sub" {
		args = nil
	}
	values := [3]int64{0, 1, 1000}
	for i := 0; i < len(args) && i < 3; i++ {
		if args[i] != "" {
			values[i] = selectorInt(args[i])
		}
	}
	path := e.chans.AncestorChain(target)
	slices.Reverse(path)
	index := slices.Index(path, context)
	if index < 0 {
		return false
	}
	required := int64(index) + values[0]
	if required < 0 {
		required = 0
	}
	if required >= int64(len(path)) {
		return false
	}
	home := e.chans.AncestorChain(u.ChannelID)
	if !slices.Contains(home, path[required]) {
		return false
	}
	depth := int64(len(home) - 1)
	return depth >= required+values[1] && depth <= required+values[2]
}

func (e *Evaluator) storedGroup(id uint32, name string, context uint32, u *mumble.User) (bool, error) {
	chain := e.chans.AncestorChain(context)
	if len(chain) == 0 || chain[len(chain)-1] != e.chans.RootID() {
		return false, fmt.Errorf("Group 频道路径无效")
	}
	var stack []models.ChannelGroup
	for _, cid := range chain {
		var rows []models.ChannelGroup
		if err := e.db.Where("server_id = ? AND channel_id = ? AND name = ?", e.chans.ServerID(), cid, name).Order("id").Find(&rows).Error; err != nil {
			return false, err
		}
		var g models.ChannelGroup
		if len(rows) == 0 {
			e.mu.RLock()
			_, temporary := e.temporary[groupKey{cid, name}]
			e.mu.RUnlock()
			if !temporary {
				continue
			}
			g = models.ChannelGroup{ChannelID: uint(cid), Inherit: true, Inheritable: true}
		} else {
			g = rows[0]
		}
		if cid != context && !g.Inheritable {
			break
		}
		stack = append(stack, g)
		if !g.Inherit {
			break
		}
	}
	match := false
	for i := len(stack) - 1; i >= 0; i-- {
		g := stack[i]
		if (id != 0 && slices.Contains(g.AddUserIDs, id)) || e.temporaryMatch(groupKey{uint32(g.ChannelID), name}, id, u) {
			match = true
		}
		if id != 0 && slices.Contains(g.RemoveUserIDs, id) {
			match = false
		}
	}
	return match, nil
}

// InGroup 与 ACL 共用解析规则；传入的用户值必须是调用方持有的快照。
func (e *Evaluator) InGroup(u mumble.User, channelID uint32, group string) bool {
	if _, ok := e.chans.GetChannel(channelID); !ok {
		return false
	}
	match, err := e.matchSelector(parseSelector(group), u.UserID, channelID, channelID, &u)
	if err != nil {
		slog.Warn("Group 计算失败，拒绝匹配", "channel_id", channelID, "error", err)
		return false
	}
	return match
}

type groupKey struct {
	channelID uint32
	name      string
}
type sessionMember struct {
	session    uint32
	generation uint64
}
type temporaryMembers struct {
	users    map[uint32]bool
	sessions map[sessionMember]bool
}

// ReplaceTemporaryMembers 替换运行时成员，不持久化。会话在加入时绑定当前连接代次。
func (e *Evaluator) ReplaceTemporaryMembers(channelID uint32, name string, ids []uint32, sessions []Subject) error {
	if name == "" {
		return fmt.Errorf("组名不能为空")
	}
	members := temporaryMembers{users: make(map[uint32]bool), sessions: make(map[sessionMember]bool)}
	for _, id := range ids {
		if id != 0 {
			members.users[id] = true
		}
	}
	// 从 generation 开始检查，防止断线清理先于本次写入导致残留。
	for {
		e.mu.RLock()
		generation := e.generation
		e.mu.RUnlock()
		if _, ok := e.chans.GetChannel(channelID); !ok {
			return fmt.Errorf("频道不存在")
		}
		clear(members.sessions)
		for _, subject := range sessions {
			u, err := e.resolveUser(subject)
			if err != nil {
				return err
			}
			if u == nil {
				return fmt.Errorf("临时会话成员需要在线连接")
			}
			members.sessions[sessionMember{u.SessionID, u.SessionGeneration}] = true
		}
		e.mu.Lock()
		if e.generation != generation {
			e.mu.Unlock()
			continue
		}
		e.temporary[groupKey{channelID, name}] = members
		e.invalidateLocked()
		e.mu.Unlock()
		return nil
	}
}
func (e *Evaluator) ClearSessionMembers(subject Subject) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, members := range e.temporary {
		for session := range members.sessions {
			if session.session == subject.SessionID && (subject.Generation == 0 || session.generation == subject.Generation) {
				delete(members.sessions, session)
			}
		}
		if len(members.sessions) == 0 && len(members.users) == 0 {
			delete(e.temporary, key)
		}
	}
	e.invalidateLocked()
}
func (e *Evaluator) temporaryMatch(key groupKey, id uint32, u *mumble.User) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	members := e.temporary[key]
	return (id != 0 && members.users[id]) || (u != nil && u.SessionGeneration != 0 && members.sessions[sessionMember{u.SessionID, u.SessionGeneration}])
}
