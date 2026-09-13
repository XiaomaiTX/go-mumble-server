package acl

import (
	"fmt"
	"github.com/dchote/go-mumble-server/internal/database/models"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"gorm.io/gorm"
	"sync/atomic"
	"testing"
	"time"
)

// 对照 Murmur v1.5.915 ACL.cpp 的路径阻断及 Write 展开规则。
func TestMurmurTraverseAndWrite(t *testing.T) {
	e, chans, db, _ := newTestEvaluator(t)
	root := chans.RootID()
	child := mustCreate(t, chans, root, "child")
	if err := CreateACL(db, &models.ChannelACL{ServerID: testServerID, ChannelID: uint(root), ApplyHere: true, GroupName: "all", Deny: uint32(mumble.PermissionTraverse)}); err != nil {
		t.Fatal(err)
	}
	seedACL(t, e, db, child.ID, mumble.PermissionWrite, true)
	if got := e.EffectivePermissions(SubjectForUserID(7), child.ID); got != 0 {
		t.Fatalf("祖先 Traverse 阻断失败: %#x", got)
	}
}

func TestMurmurWriteDoesNotGrantVoiceOrGlobalChildRights(t *testing.T) {
	e, chans, db, _ := newTestEvaluator(t)
	child := mustCreate(t, chans, chans.RootID(), "child")
	if err := CreateACL(db, &models.ChannelACL{ServerID: testServerID, ChannelID: uint(child.ID), ApplyHere: true, GroupName: "all", Grant: uint32(mumble.PermissionWrite | mumble.PermissionKick), Deny: uint32(mumble.PermissionSpeak | mumble.PermissionWhisper)}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []mumble.Permission{mumble.PermissionSpeak, mumble.PermissionWhisper, mumble.PermissionKick} {
		if e.Check(SubjectForUserID(7), child.ID, p) {
			t.Errorf("Write 意外放行 %#x", p)
		}
	}
	if !e.Check(SubjectForUserID(7), child.ID, mumble.PermissionEnter|mumble.PermissionListen) {
		t.Fatal("缺少 Write 隐含权限")
	}
}

// 对照 Group.cpp：Token 大小写不敏感；子级 remove 最后生效。
func TestMurmurTokenAndGroupOverride(t *testing.T) {
	e, chans, db, _ := newTestEvaluator(t)
	child := mustCreate(t, chans, chans.RootID(), "child")
	u := mumble.User{UserID: 7, ChannelID: child.ID, AccessTokens: []string{"Secret"}}
	if !e.InGroup(u, child.ID, "#secret") {
		t.Error("Token 应忽略大小写")
	}
	for _, g := range []models.ChannelGroup{
		{ServerID: testServerID, ChannelID: uint(chans.RootID()), Name: "team", Inherit: true, Inheritable: true, AddUserIDs: models.Uint32Slice{7}},
		{ServerID: testServerID, ChannelID: uint(child.ID), Name: "team", Inherit: true, Inheritable: true, RemoveUserIDs: models.Uint32Slice{7}},
	} {
		if err := CreateGroup(db, &g); err != nil {
			t.Fatal(err)
		}
	}
	if e.InGroup(u, child.ID, "team") {
		t.Fatal("父级 add 覆盖了子级 remove")
	}
}

// 下列预期来自 ACL.cpp 的逐频道状态机，而非被测计算器。
func TestMurmurACLMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		here, subs bool
	}{
		{"neither", false, false}, {"here", true, false}, {"subs", false, true}, {"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, chans, db, _ := newTestEvaluator(t)
			root := chans.RootID()
			child := mustCreate(t, chans, root, "child")
			if err := CreateACL(db, &models.ChannelACL{ServerID: testServerID, ChannelID: uint(root), ApplyHere: tc.here, ApplySubs: tc.subs, GroupName: "all", Grant: uint32(mumble.PermissionMuteDeafen)}); err != nil {
				t.Fatal(err)
			}
			if got := e.Check(SubjectForUserID(7), root, mumble.PermissionMuteDeafen); got != tc.here {
				t.Fatalf("here=%v", got)
			}
			if got := e.Check(SubjectForUserID(7), child.ID, mumble.PermissionMuteDeafen); got != tc.subs {
				t.Fatalf("subs=%v", got)
			}
		})
	}
	for _, p := range []mumble.Permission{mumble.PermissionKick, mumble.PermissionBan, mumble.PermissionRegister, mumble.PermissionSelfRegister, mumble.PermissionResetUser} {
		t.Run(fmt.Sprintf("root-only-%x", p), func(t *testing.T) {
			e, chans, db, _ := newTestEvaluator(t)
			root := chans.RootID()
			child := mustCreate(t, chans, root, "child")
			seedACL(t, e, db, root, p, true)
			seedACL(t, e, db, child.ID, p, true)
			if !e.Check(SubjectForUserID(7), root, p) {
				t.Fatal("root 授权丢失")
			}
			if e.Check(SubjectForUserID(7), child.ID, p) {
				t.Fatal("服务器权限泄漏到子频道")
			}
		})
	}
}

func TestMurmurTraverseStateAcrossInheritance(t *testing.T) {
	for _, withWrite := range []bool{false, true} {
		t.Run(fmt.Sprint(withWrite), func(t *testing.T) {
			e, chans, db, _ := newTestEvaluator(t)
			root := chans.RootID()
			child := mustCreate(t, chans, root, "child")
			if err := db.Model(&models.Channel{}).Where("id = ?", child.ID).Update("inherit_acl", false).Error; err != nil {
				t.Fatal(err)
			}
			chans.Reload()
			var grant uint32
			if withWrite {
				grant = uint32(mumble.PermissionWrite)
			}
			if err := CreateACL(db, &models.ChannelACL{ServerID: testServerID, ChannelID: uint(root), ApplyHere: true, ApplySubs: true, GroupName: "all", Grant: grant, Deny: uint32(mumble.PermissionTraverse)}); err != nil {
				t.Fatal(err)
			}
			got := e.EffectivePermissions(SubjectForUserID(7), child.ID)
			if !withWrite && got != 0 {
				t.Fatalf("不继承不能绕过祖先阻断: %#x", got)
			}
			if withWrite && got != uint32(DefaultPermissions) {
				t.Fatalf("Write 路径例外后重置为基础权限: %#x", got)
			}
		})
	}
}

func TestMurmurOrderedAllowDeny(t *testing.T) {
	e, chans, db, _ := newTestEvaluator(t)
	root := chans.RootID()
	row := models.ChannelACL{ServerID: testServerID, ChannelID: uint(root), Priority: 1, ApplyHere: true, GroupName: "all", Grant: uint32(mumble.PermissionSpeak), Deny: uint32(mumble.PermissionSpeak)}
	if err := CreateACL(db, &row); err != nil {
		t.Fatal(err)
	}
	if e.Check(SubjectForUserID(7), root, mumble.PermissionSpeak) {
		t.Fatal("同条 deny 必须覆盖 grant")
	}
	row.Priority = 2
	row.Deny = 0
	if err := CreateACL(db, &row); err != nil {
		t.Fatal(err)
	}
	e.InvalidateCache()
	if !e.Check(SubjectForUserID(7), root, mumble.PermissionSpeak) {
		t.Fatal("后续 grant 应恢复权限")
	}
	row.Priority = 3
	row.Grant = 0
	row.Deny = uint32(mumble.PermissionSpeak)
	if err := CreateACL(db, &row); err != nil {
		t.Fatal(err)
	}
	e.InvalidateCache()
	if e.CheckCurrent(SubjectForUserID(7), root, mumble.PermissionSpeak) {
		t.Fatal("后续 deny 应撤销权限")
	}
}

// Group.cpp 的前缀、内置组及 sub 参数转换规则。
func TestMurmurSelectorMatrix(t *testing.T) {
	e, chans, _, _ := newTestEvaluator(t)
	root := chans.RootID()
	a := mustCreate(t, chans, root, "a")
	b := mustCreate(t, chans, a.ID, "b")
	other := mustCreate(t, chans, root, "other")
	u := mumble.User{UserID: 7, ChannelID: b.ID, AccessTokens: []string{"SeCrEt"}, CertHash: "abcdef", CertificateVerified: true, ExternalIdentity: true, ExternalGroups: []string{"none", "in", "team"}}
	for _, tc := range []struct {
		spec               string
		target, definition uint32
		want               bool
	}{
		{"", a.ID, a.ID, false}, {"!", a.ID, a.ID, false}, {"!~#$", a.ID, a.ID, false},
		{"none", a.ID, a.ID, false}, {"!none", a.ID, a.ID, true}, {"!!none", a.ID, a.ID, true}, {"all", a.ID, a.ID, true},
		{"auth", a.ID, a.ID, true}, {"strong", a.ID, a.ID, true}, {"in", a.ID, a.ID, false}, {"out", a.ID, a.ID, true},
		{"~in", b.ID, a.ID, false}, {"in", b.ID, a.ID, true}, {"!~in", b.ID, a.ID, true},
		{"#secret", a.ID, a.ID, true}, {"$abcdef", a.ID, a.ID, true}, {"$ABCDEF", a.ID, a.ID, false},
		{"$#secret", a.ID, a.ID, true}, {"#$abcdef", a.ID, a.ID, false}, {"~!#secret", a.ID, a.ID, false},
		{"team", a.ID, a.ID, true}, {"Team", a.ID, a.ID, false},
		{"sub", a.ID, a.ID, true}, {"sub", b.ID, b.ID, false}, {"sub,0,0,0", b.ID, b.ID, true},
		{"sub,0,2,3", a.ID, a.ID, false}, {"sub,-1,2,2", a.ID, a.ID, true},
		{"~sub,1,0,0", b.ID, a.ID, true}, {"sub,1", a.ID, a.ID, false}, {"sub,-99,2,2", a.ID, a.ID, true},
		{"sub,,,", a.ID, a.ID, true}, {"sub,bad,1,1", a.ID, a.ID, true},
		{"sub,0,0,bad", a.ID, a.ID, false}, {"sub,2147483648,1,1", a.ID, a.ID, true},
		{"sub,0,2,1", a.ID, a.ID, false}, {"sub,0,1,1,ignored", a.ID, a.ID, true},
		{"sub", other.ID, other.ID, false}, {"!sub", other.ID, other.ID, true},
	} {
		t.Run(tc.spec+fmt.Sprint(tc.target, tc.definition), func(t *testing.T) {
			got, err := e.matchSelector(parseSelector(tc.spec), u.UserID, tc.target, tc.definition, &u)
			if err != nil || got != tc.want {
				t.Fatalf("got=%v err=%v want=%v", got, err, tc.want)
			}
		})
	}
	u.CertificateVerified = false
	if e.InGroup(u, a.ID, "strong") {
		t.Fatal("证书存在不能代替可信验证")
	}
	u.UserID = 0
	if e.InGroup(u, a.ID, "auth") {
		t.Fatal("匿名用户不能匹配 auth")
	}
}

func TestProjectStoredSelectorCompatibility(t *testing.T) {
	e, chans, _, _ := newTestEvaluator(t)
	root := chans.RootID()
	child := mustCreate(t, chans, root, "child")
	u := mumble.User{UserID: 7, ChannelID: root, AccessTokens: []string{"!secret"}}
	id := int32(7)
	for _, tc := range []struct {
		row  models.ChannelACL
		want bool
	}{
		{models.ChannelACL{UserID: &id, GroupName: "none", AccessToken: "missing"}, true},
		{models.ChannelACL{AccessToken: "!SECRET", GroupName: "none"}, true},
		{models.ChannelACL{AccessToken: "!SECRET", Invert: true}, false},
		{models.ChannelACL{GroupName: "~in", EvalHere: true}, true},
		{models.ChannelACL{GroupName: "!none", Invert: true}, true},
		{models.ChannelACL{GroupName: "!", Invert: true}, false},
	} {
		tc.row.ChannelID = uint(root)
		got, err := e.entryMatches(tc.row, 7, child.ID, &u)
		if err != nil || got != tc.want {
			t.Fatalf("row=%+v got=%v err=%v", tc.row, got, err)
		}
	}
}

func TestMurmurGroupInheritanceMatrix(t *testing.T) {
	for _, inherit := range []bool{false, true} {
		for _, inheritable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v-%v", inherit, inheritable), func(t *testing.T) {
				e, chans, db, _ := newTestEvaluator(t)
				root := chans.RootID()
				mid := mustCreate(t, chans, root, "mid")
				leaf := mustCreate(t, chans, mid.ID, "leaf")
				// ID 7 只在祖先加入，ID 8 在中间层加入，ID 9 在中间层被移除。
				for _, g := range []models.ChannelGroup{
					{ServerID: testServerID, ChannelID: uint(root), Name: "g", Inherit: true, Inheritable: true, AddUserIDs: models.Uint32Slice{7, 9}},
					{ServerID: testServerID, ChannelID: uint(mid.ID), Name: "g", Inherit: inherit, Inheritable: inheritable, AddUserIDs: models.Uint32Slice{8, 9}, RemoveUserIDs: models.Uint32Slice{9}},
				} {
					if err := CreateGroup(db, &g); err != nil {
						t.Fatal(err)
					}
				}
				for _, tc := range []struct {
					id, cid uint32
					want    bool
				}{{7, mid.ID, inherit}, {8, mid.ID, true}, {9, mid.ID, false}, {7, leaf.ID, inherit && inheritable}, {8, leaf.ID, inheritable}, {9, leaf.ID, false}} {
					if got := e.InGroup(mumble.User{UserID: tc.id}, tc.cid, "g"); got != tc.want {
						t.Errorf("id=%d cid=%d got=%v", tc.id, tc.cid, got)
					}
				}
			})
		}
	}
}

func TestMurmurGroupMissingMiddleAndChildReAdd(t *testing.T) {
	e, chans, db, _ := newTestEvaluator(t)
	root := chans.RootID()
	mid := mustCreate(t, chans, root, "mid")
	leaf := mustCreate(t, chans, mid.ID, "leaf")
	if err := db.Model(&models.Channel{}).Where("id = ?", mid.ID).Update("inherit_acl", false).Error; err != nil {
		t.Fatal(err)
	}
	chans.Reload()
	for _, g := range []models.ChannelGroup{
		{ServerID: testServerID, ChannelID: uint(root), Name: "g", Inherit: true, Inheritable: true, AddUserIDs: models.Uint32Slice{8}, RemoveUserIDs: models.Uint32Slice{7}},
		{ServerID: testServerID, ChannelID: uint(leaf.ID), Name: "g", Inherit: true, Inheritable: false, AddUserIDs: models.Uint32Slice{7}},
	} {
		if err := CreateGroup(db, &g); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uint32{7, 8} {
		if !e.InGroup(mumble.User{UserID: id}, leaf.ID, "g") {
			t.Fatalf("ID %d 未继承或未恢复", id)
		}
	}
}

func TestProjectTemporaryMembersLifecycle(t *testing.T) {
	e, chans, db, users := newTestEvaluator(t)
	root := chans.RootID()
	child := mustCreate(t, chans, root, "child")
	u, _ := users.Add(mumble.User{Name: "guest", ChannelID: child.ID})
	if err := e.ReplaceTemporaryMembers(root, "runtime", []uint32{8}, []Subject{SubjectOf(u)}); err != nil {
		t.Fatal(err)
	}
	if !e.InGroup(u, child.ID, "runtime") {
		t.Fatal("会话临时成员未继承")
	}
	if err := CreateGroup(db, &models.ChannelGroup{ServerID: testServerID, ChannelID: uint(child.ID), Name: "runtime", Inherit: true, Inheritable: true, RemoveUserIDs: models.Uint32Slice{8}}); err != nil {
		t.Fatal(err)
	}
	if e.InGroup(mumble.User{UserID: 8}, child.ID, "runtime") {
		t.Fatal("remove 必须覆盖临时加入")
	}
	users.Remove(u.SessionID)
	next, _ := users.Add(mumble.User{Name: "next", ChannelID: child.ID})
	if next.SessionID != u.SessionID {
		t.Fatal("测试未复用 session")
	}
	if e.InGroup(next, child.ID, "runtime") {
		t.Fatal("复用 session 继承了临时权限")
	}
	if p := e.EffectivePermissions(SubjectOf(u), root); p != 0 {
		t.Fatalf("失效 Subject 获得 %#x", p)
	}
	if err := e.ReplaceTemporaryMembers(child.ID, "deleted", nil, []Subject{SubjectOf(next)}); err != nil {
		t.Fatal(err)
	}
	if !chans.Remove(child.ID) {
		t.Fatal("删除频道失败")
	}
	e.mu.RLock()
	_, exists := e.temporary[groupKey{child.ID, "deleted"}]
	e.mu.RUnlock()
	if exists {
		t.Fatal("频道临时成员未清理")
	}
	var count int64
	if err := db.Model(&models.ChannelGroup{}).Where("name = ?", "runtime").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("临时成员不应创建数据库组")
	}
	fresh := NewEvaluator(db, chans, users)
	if fresh.InGroup(mumble.User{UserID: 8}, root, "runtime") {
		t.Fatal("重建 evaluator 后临时成员不应恢复")
	}
}

func TestProjectCacheTracksSessionChanges(t *testing.T) {
	e, chans, db, users := newTestEvaluator(t)
	root := chans.RootID()
	child := mustCreate(t, chans, root, "child")
	for i, g := range []string{"#secret", "in", "team"} {
		p := []mumble.Permission{mumble.PermissionMuteDeafen, mumble.PermissionMove, mumble.PermissionMakeChannel}[i]
		if err := CreateACL(db, &models.ChannelACL{ServerID: testServerID, ChannelID: uint(child.ID), ApplyHere: true, GroupName: g, Grant: uint32(p)}); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := users.Add(mumble.User{Name: "a", UserID: 7, ChannelID: root})
	other, _ := users.Add(mumble.User{Name: "b", UserID: 7, ChannelID: child.ID, AccessTokens: []string{"secret"}})
	subject := SubjectOf(u)
	if e.Check(subject, child.ID, mumble.PermissionMuteDeafen) {
		t.Fatal("同账号另一会话 Token 泄漏")
	}
	users.UpdateUser(u.SessionID, func(u *mumble.User) { u.AccessTokens = []string{"SECRET"} })
	if !e.Check(subject, child.ID, mumble.PermissionMuteDeafen) {
		t.Fatal("Token 更新未失效缓存")
	}
	users.SetChannel(u.SessionID, child.ID)
	if !e.Check(subject, child.ID, mumble.PermissionMove) {
		t.Fatal("移动未失效缓存")
	}
	users.UpdateIdentity(u.SessionID, func(u *mumble.User) { u.ExternalIdentity = true; u.ExternalGroups = []string{"team"} })
	if !e.Check(subject, child.ID, mumble.PermissionMakeChannel) {
		t.Fatal("外部身份更新未失效缓存")
	}
	users.UpdateIdentity(u.SessionID, func(u *mumble.User) { u.ExternalGroups = nil })
	if e.Check(subject, child.ID, mumble.PermissionMakeChannel) {
		t.Fatal("撤销外部组未生效")
	}
	if e.Check(SubjectForUserID(other.UserID), child.ID, mumble.PermissionMuteDeafen) {
		t.Fatal("无会话主体借用了在线 Token")
	}
	users.Remove(u.SessionID)
	if e.EffectivePermissions(subject, child.ID) != 0 {
		t.Fatal("断线后缓存继续授权")
	}
}

func TestProjectAPIAdminRevocation(t *testing.T) {
	e, chans, db, _ := newTestEvaluator(t)
	root := chans.RootID()
	account := models.User{Username: "admin", PasswordHash: "x", JWTSecret: "x", Role: models.RoleAdmin}
	if err := db.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	subject := SubjectForUserID(MakeAPIUserID(account.ID))
	if !e.Check(subject, root, mumble.PermissionBan) {
		t.Fatal("API 管理员缺少 root 管理权限")
	}
	if err := CreateACL(db, &models.ChannelACL{ServerID: testServerID, ChannelID: uint(root), ApplyHere: true, GroupName: "all", Deny: uint32(mumble.PermissionSpeak)}); err != nil {
		t.Fatal(err)
	}
	if e.Check(subject, root, mumble.PermissionSpeak) {
		t.Fatal("API Write 绕过了 Speak deny")
	}
	if err := db.Model(&account).Update("role", models.RoleUser).Error; err != nil {
		t.Fatal(err)
	}
	if e.Check(subject, root, mumble.PermissionBan) {
		t.Fatal("API 撤权后仍然命中旧缓存")
	}
}

func TestProjectDefaultAuthACL(t *testing.T) {
	e, chans, db, _ := newTestEvaluator(t)
	root := chans.RootID()
	child := mustCreate(t, chans, root, "child")
	if e.Check(SubjectForUserID(7), root, mumble.PermissionSelfRegister) {
		t.Fatal("不应自动补权")
	}
	if err := EnsureDefaultRootACLs(db, testServerID); err != nil {
		t.Fatal(err)
	}
	e.InvalidateCache()
	if !e.Check(SubjectForUserID(7), root, mumble.PermissionSelfRegister) {
		t.Fatal("auth ACL 未授权")
	}
	if e.Check(SubjectForUserID(7), child.ID, mumble.PermissionSelfRegister) {
		t.Fatal("SelfRegister 泄漏到子频道")
	}
	if !e.Check(SubjectForUserID(7), child.ID, mumble.PermissionMakeTempChannel) {
		t.Fatal("auth 频道权限未继承")
	}
}

func TestProjectSuperUserIdentityKeepsMurmurPermissions(t *testing.T) {
	e, chans, _, users := newTestEvaluator(t)
	super, _ := users.Add(mumble.User{Name: mumble.SuperUserName, UserID: 7, IsSuperUser: true, ChannelID: chans.RootID()})
	permissions := e.EffectivePermissions(SubjectOf(super), chans.RootID())
	if permissions&uint32(mumble.PermissionBan|mumble.PermissionWrite) != uint32(mumble.PermissionBan|mumble.PermissionWrite) {
		t.Fatalf("SuperUser 缺少管理权限: %#x", permissions)
	}
	if permissions&uint32(mumble.PermissionSpeak|mumble.PermissionWhisper) != 0 {
		t.Fatalf("SuperUser 意外获得语音权限: %#x", permissions)
	}
}

func TestProjectQueryFailureFailsClosed(t *testing.T) {
	for _, table := range []string{"channel_acls", "channel_groups"} {
		t.Run(table, func(t *testing.T) {
			e, chans, db, _ := newTestEvaluator(t)
			root := chans.RootID()
			if err := CreateACL(db, &models.ChannelACL{ServerID: testServerID, ChannelID: uint(root), ApplyHere: true, GroupName: "!missing", Grant: uint32(mumble.PermissionWrite)}); err != nil {
				t.Fatal(err)
			}
			if err := db.Migrator().DropTable(table); err != nil {
				t.Fatal(err)
			}
			if got := e.EffectivePermissions(SubjectForUserID(7), root); got != 0 {
				t.Fatalf("查询失败仍授权 %#x", got)
			}
			if table == "channel_groups" && e.InGroup(mumble.User{UserID: 7}, root, "!missing") {
				t.Fatal("查询错误不能被取反为匹配")
			}
		})
	}
}

func TestProjectInvalidationDoesNotRefillOldResult(t *testing.T) {
	e, chans, db, _ := newTestEvaluator(t)
	root := chans.RootID()
	seedACL(t, e, db, root, mumble.PermissionMuteDeafen, true)
	captured, resume := make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	if err := db.Callback().Query().After("gorm:query").Register("test:block_old_acl", func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_acls" && paused.CompareAndSwap(false, true) {
			close(captured)
			<-resume
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove("test:block_old_acl")
	done := make(chan uint32, 1)
	go func() { done <- e.EffectivePermissions(SubjectForUserID(7), root) }()
	select {
	case <-captured:
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("旧计算未到达屏障")
	}
	err := db.Model(&models.ChannelACL{}).Where("server_id = ?", testServerID).Updates(map[string]any{"grant": 0, "deny": uint32(mumble.PermissionMuteDeafen)}).Error
	e.InvalidateCache()
	close(resume)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-done:
		if p&uint32(mumble.PermissionMuteDeafen) != 0 {
			t.Fatal("旧结果回填缓存")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("重算未完成")
	}
	if e.Check(SubjectForUserID(7), root, mumble.PermissionMuteDeafen) {
		t.Fatal("失效后缓存仍放行")
	}
}
