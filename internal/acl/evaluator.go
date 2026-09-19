package acl

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/dchote/go-mumble-server/internal/channel"
	"github.com/dchote/go-mumble-server/internal/database/models"
	"github.com/dchote/go-mumble-server/internal/user"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"gorm.io/gorm"
)

const DefaultPermissions = mumble.PermissionTraverse | mumble.PermissionEnter |
	mumble.PermissionSpeak | mumble.PermissionWhisper | mumble.PermissionTextMessage | mumble.PermissionListen
const rootPermissions = mumble.PermissionKick | mumble.PermissionBan | mumble.PermissionRegister |
	mumble.PermissionSelfRegister | mumble.PermissionResetUser
const writePermissions = mumble.PermissionTraverse | mumble.PermissionEnter | mumble.PermissionMuteDeafen |
	mumble.PermissionMove | mumble.PermissionMakeChannel | mumble.PermissionLinkChannel |
	mumble.PermissionTextMessage | mumble.PermissionMakeTempChannel | mumble.PermissionListen

// Subject 区分账号与连接；Generation 非零时禁止旧连接引用匹配复用的 session。
type Subject struct {
	SessionID  uint32
	UserID     uint32
	Generation uint64
}

func SubjectOf(u mumble.User) Subject {
	return Subject{SessionID: u.SessionID, UserID: u.UserID, Generation: u.SessionGeneration}
}
func SubjectForUserID(id uint32) Subject { return Subject{UserID: id} }

type Evaluator struct {
	mu         sync.RWMutex
	db         *gorm.DB
	chans      *channel.Manager
	users      *user.Manager
	cache      map[cacheKey]uint32
	generation uint64
	temporary  map[groupKey]temporaryMembers
}
type cacheKey struct {
	subject   Subject
	channelID uint32
	revision  uint64
}

func NewEvaluator(db *gorm.DB, chans *channel.Manager, users *user.Manager) *Evaluator {
	e := &Evaluator{db: db, chans: chans, users: users, cache: make(map[cacheKey]uint32), temporary: make(map[groupKey]temporaryMembers)}
	if users != nil {
		users.AddAuthorizationListener(func(u mumble.User, removed bool) {
			if removed {
				e.ClearSessionMembers(SubjectOf(u))
			} else {
				e.InvalidateCache()
			}
		})
	}
	chans.AddAuthorizationListener(func() {
		e.mu.Lock()
		for key := range e.temporary {
			if _, ok := chans.GetChannel(key.channelID); !ok {
				delete(e.temporary, key)
			}
		}
		e.invalidateLocked()
		e.mu.Unlock()
	})
	return e
}
func (e *Evaluator) invalidateLocked() {
	e.generation++
	e.cache = make(map[cacheKey]uint32)
}
func (e *Evaluator) InvalidateCache() { e.mu.Lock(); defer e.mu.Unlock(); e.invalidateLocked() }
func (e *Evaluator) Check(subject Subject, channelID uint32, perm mumble.Permission) bool {
	return e.EffectivePermissions(subject, channelID)&uint32(perm) == uint32(perm)
}
func (e *Evaluator) EffectivePermissions(subject Subject, channelID uint32) uint32 {
	return e.permissions(subject, channelID, true)
}
func (e *Evaluator) CheckCurrent(subject Subject, channelID uint32, perm mumble.Permission) bool {
	return e.permissions(subject, channelID, false)&uint32(perm) == uint32(perm)
}

// EnterRestrictedChannels returns channels that contain a local ACL denying
// Enter. This intentionally ignores selector matching and inheritance: Murmur's
// is_enter_restricted flag describes whether the channel itself contains an
// Enter denial, while can_enter carries the receiver-specific effective result.
func (e *Evaluator) EnterRestrictedChannels() (map[uint32]bool, error) {
	restricted := make(map[uint32]bool)
	if e == nil || e.db == nil || e.chans == nil {
		return restricted, nil
	}
	var rows []models.ChannelACL
	if err := e.db.Select("channel_id", "deny").Where("server_id = ?", e.chans.ServerID()).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Deny&uint32(mumble.PermissionEnter) != 0 {
			restricted[uint32(row.ChannelID)] = true
		}
	}
	return restricted, nil
}

func (e *Evaluator) permissions(subject Subject, channelID uint32, cached bool) uint32 {
	// API 角色来自数据库：不缓存该扩展，直接撤权也不会留下旧权限。
	cached = cached && !IsAPIUserID(subject.UserID)
	for {
		e.mu.RLock()
		generation := e.generation
		e.mu.RUnlock()
		u, err := e.resolveUser(subject)
		if err != nil {
			return 0
		}
		// 项目仍以 IsSuperUser 表示保留身份。其权限与 Murmur 的账号 0
		// 一致，但不隐含 Speak/Whisper，避免管理身份绕过语音 ACL。
		if u != nil && u.IsSuperUser {
			return uint32(mumble.PermissionAll &^ (mumble.PermissionSpeak | mumble.PermissionWhisper))
		}
		key := cacheKey{subject: subject, channelID: channelID}
		if u != nil {
			key.subject = SubjectOf(*u)
			key.revision = u.AuthorizationRevision
		}
		if cached {
			e.mu.RLock()
			p, ok := e.cache[key]
			valid := generation == e.generation
			e.mu.RUnlock()
			if !valid {
				continue
			}
			if ok {
				return p
			}
		}
		p, err := e.evaluate(subject, channelID, u)
		if err != nil {
			slog.Warn("ACL 计算失败，拒绝授权", "channel_id", channelID, "error", err)
			return 0
		}
		e.mu.Lock()
		if generation != e.generation {
			e.mu.Unlock()
			continue
		}
		if cached {
			e.cache[key] = p
		}
		e.mu.Unlock()
		return p
	}
}

// evaluate 对照 Murmur v1.5.915 ACL.cpp；不能通过截断祖先链实现 InheritACL。
func (e *Evaluator) evaluate(subject Subject, target uint32, u *mumble.User) (uint32, error) {
	chain := e.chans.AncestorChain(target)
	if len(chain) == 0 || chain[len(chain)-1] != e.chans.RootID() {
		return 0, fmt.Errorf("频道路径不存在或不完整")
	}
	def := uint32(DefaultPermissions)
	if IsAPIUserID(subject.UserID) {
		admin, err := e.apiAdmin(subject.UserID)
		if err != nil {
			return 0, err
		}
		if admin {
			def |= uint32(mumble.PermissionWrite)
		}
	}
	granted := def
	traverse, write := true, def&uint32(mumble.PermissionWrite) != 0
	for i := len(chain) - 1; i >= 0; i-- {
		cid := chain[i]
		_, inherit, ok := e.chans.GetChannelWithMeta(cid)
		if !ok {
			return 0, fmt.Errorf("频道已删除")
		}
		if !inherit {
			granted = def
		}
		var rows []models.ChannelACL
		if err := e.db.Where("server_id = ? AND channel_id = ?", e.chans.ServerID(), cid).Order("priority, id").Find(&rows).Error; err != nil {
			return 0, err
		}
		for _, row := range rows {
			match, err := e.entryMatches(row, subject.UserID, target, u)
			if err != nil {
				return 0, err
			}
			if !match {
				continue
			}
			self := cid == target && row.ApplyHere
			inherited := cid != target && row.ApplySubs
			apply := self || inherited
			if inherited || row.ApplyHere {
				if row.Grant&uint32(mumble.PermissionTraverse) != 0 {
					traverse = true
				}
				if row.Deny&uint32(mumble.PermissionTraverse) != 0 {
					traverse = false
				}
			}
			if apply {
				if row.Grant&uint32(mumble.PermissionWrite) != 0 {
					write = true
				}
				if row.Deny&uint32(mumble.PermissionWrite) != 0 {
					write = false
				}
			}
			if cid == e.chans.RootID() && self {
				granted |= row.Grant & uint32(rootPermissions)
			}
			if apply {
				granted |= row.Grant & uint32(mumble.PermissionAll&^rootPermissions)
				granted &^= row.Deny
			}
		}
		if !traverse && !write {
			return 0, nil
		}
	}
	if granted&uint32(mumble.PermissionWrite) != 0 {
		granted |= uint32(writePermissions)
		if target == e.chans.RootID() {
			granted |= uint32(rootPermissions)
		}
	}
	return granted & uint32(mumble.PermissionAll), nil
}

func (e *Evaluator) resolveUser(subject Subject) (*mumble.User, error) {
	if subject.SessionID == 0 {
		return nil, nil
	}
	if e.users != nil {
		u, ok := e.users.Snapshot(subject.SessionID)
		if ok && u.UserID == subject.UserID && (subject.Generation == 0 || subject.Generation == u.SessionGeneration) {
			return &u, nil
		}
	}
	return nil, fmt.Errorf("会话不存在或已复用")
}
func (e *Evaluator) apiAdmin(id uint32) (bool, error) {
	var account models.User
	err := e.db.Where("id = ?", APIUserIDToDBID(id)).First(&account).Error
	if err == gorm.ErrRecordNotFound {
		return false, nil
	}
	return account.Role == models.RoleAdmin, err
}
