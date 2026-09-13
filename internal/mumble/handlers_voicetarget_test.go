package mumble

import (
	"bytes"
	"fmt"
	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/channel"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/internal/database/models"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
	"sync"
	"testing"
)

func TestVoiceTargetSessionReuse(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	a := &mumble.User{Name: "a"}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b"}
	connectTestUser(t, s, b)
	payload, err := (&messages.VoiceTarget{ID: 1, Targets: []messages.VoiceTargetTarget{{Session: []uint32{b.SessionID}}}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.handleVoiceTarget(protocol.MessageVoiceTarget, payload, ac); err != nil {
		t.Fatal(err)
	}
	s.users.Remove(b.SessionID)
	s.UnregisterConn(b.SessionID)
	c := &mumble.User{Name: "c"}
	connectTestUser(t, s, c)
	if c.SessionID != b.SessionID {
		t.Fatal("未复用 session ID")
	}
	for _, sid := range s.getVoiceTargetRecipients(a.SessionID, 1) {
		if sid == c.SessionID {
			t.Fatal("旧私聊目标被重定向到复用 session ID 的新用户")
		}
	}
}
func newVoiceTargetTestServer(t *testing.T) *Server {
	t.Helper()
	s := newACLTestServer(t)
	db, err := s.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return s
}

func setVoiceTarget(t *testing.T, s *Server, c *connection.Conn, targets ...messages.VoiceTargetTarget) {
	t.Helper()
	payload, err := (&messages.VoiceTarget{ID: 1, Targets: targets}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.handleVoiceTarget(protocol.MessageVoiceTarget, payload, c); err != nil {
		t.Fatal(err)
	}
}

func assertVoiceRecipients(t *testing.T, s *Server, a uint32, want ...uint32) {
	t.Helper()
	got := s.getVoiceTargetRecipients(a, 1)
	expected := make(map[uint32]bool)
	for _, id := range want {
		expected[id] = true
	}
	if len(got) != len(expected) {
		t.Fatalf("接收者=%v，期望=%v", got, want)
	}
	for _, id := range got {
		if !expected[id] {
			t.Fatalf("意外接收者 %d", id)
		}
		delete(expected, id)
	}
}

func TestVoiceTargetDynamicChannelAndACL(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "target", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: root}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: root}
	connectTestUser(t, s, b)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{ChannelID: ch.ID})
	assertVoiceRecipients(t, s, a.SessionID)
	s.users.SetChannel(b.SessionID, ch.ID)
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
	denyACL(t, s, ch.ID, mumble.PermissionWhisper)
	assertVoiceRecipients(t, s, a.SessionID)
	if err := s.db.Where("channel_id = ?", ch.ID).Delete(&models.ChannelACL{}).Error; err != nil {
		t.Fatal(err)
	}
	// 不手动清缓存，语音仍必须读取当前权限。
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
	s.users.SetChannel(b.SessionID, root)
	assertVoiceRecipients(t, s, a.SessionID)
	s.users.SetChannel(b.SessionID, ch.ID)
	s.users.UpdateUser(b.SessionID, func(u *mumble.User) { u.SelfDeaf = true })
	assertVoiceRecipients(t, s, a.SessionID)
	s.users.UpdateUser(b.SessionID, func(u *mumble.User) { u.SelfDeaf = false })
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
	setVoiceTarget(t, s, ac)
	assertVoiceRecipients(t, s, a.SessionID)
}

func TestVoiceTargetRootPresenceAndDirectOnly(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	a := &mumble.User{Name: "a"}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b"}
	connectTestUser(t, s, b)
	c := &mumble.User{Name: "c"}
	connectTestUser(t, s, c)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{Session: []uint32{b.SessionID}})
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{HasChannelID: true})
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID, c.SessionID)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{Session: []uint32{999}})
	assertVoiceRecipients(t, s, a.SessionID)
}

func TestVoiceTargetDynamicLinksChildrenGroups(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	root := s.chans.RootID()
	ch := s.chans.Create(root, "target", "", 0, false, 0)
	child := s.chans.Create(ch.ID, "child", "", 0, false, 0)
	linked := s.chans.Create(root, "linked", "", 0, false, 0)
	end := s.chans.Create(root, "end", "", 0, false, 0)
	a := &mumble.User{Name: "a"}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b", ChannelID: child.ID, UserID: 2}
	connectTestUser(t, s, b)
	c := &mumble.User{Name: "c", ChannelID: end.ID, UserID: 3}
	connectTestUser(t, s, c)
	target := messages.VoiceTargetTarget{ChannelID: ch.ID, Children: true, Links: true}
	setVoiceTarget(t, s, ac, target)
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
	s.chans.Update(ch.ID, channel.UpdateOpts{Links: []uint32{linked.ID}})
	s.chans.Update(linked.ID, channel.UpdateOpts{Links: []uint32{end.ID, ch.ID}})
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID, c.SessionID)
	if _, ok := s.chans.UpdateLinks(ch.ID, nil); !ok {
		t.Fatal("unlink channels")
	}
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
	target.Group = "#secret"
	setVoiceTarget(t, s, ac, target)
	assertVoiceRecipients(t, s, a.SessionID)
	s.users.UpdateUser(b.SessionID, func(u *mumble.User) { u.AccessTokens = []string{"secret"} })
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
	s.users.UpdateUser(b.SessionID, func(u *mumble.User) { u.AccessTokens = nil })
	assertVoiceRecipients(t, s, a.SessionID)
	target.Group = "team"
	setVoiceTarget(t, s, ac, target)
	assertVoiceRecipients(t, s, a.SessionID)
	if err := acl.CreateGroup(s.db, &models.ChannelGroup{ServerID: s.chans.ServerID(), ChannelID: uint(child.ID), Name: "team", AddUserIDs: models.Uint32Slice{2}}); err != nil {
		t.Fatal(err)
	}
	assertVoiceRecipients(t, s, a.SessionID, b.SessionID)
	if err := s.db.Where("name = ?", "team").Delete(&models.ChannelGroup{}).Error; err != nil {
		t.Fatal(err)
	}
	assertVoiceRecipients(t, s, a.SessionID)
}

func TestVoiceTargetPinsConnectionThroughSend(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	a := &mumble.User{Name: "a"}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b"}
	_, bc := connectTestUser(t, s, b)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{Session: []uint32{b.SessionID}})
	resolved := s.resolveVoiceTarget(a.SessionID, 1)
	if len(resolved) != 1 {
		t.Fatal("目标未解析")
	}
	s.users.Remove(b.SessionID)
	s.UnregisterConn(b.SessionID)
	c := &mumble.User{Name: "c"}
	connectTestUser(t, s, c)
	if c.SessionID != b.SessionID {
		t.Fatal("未复用 session")
	}
	packet := []byte{1, 2, 3}
	r := resolved[0]
	if err := s.sendAudioTo(r.session, r.conn, nil, packet); err != nil {
		t.Fatal(err)
	}
	kind, got := readMessage(t, bc)
	if kind != protocol.MessageUDPTunnel || !bytes.Equal(got, packet) {
		t.Fatal("发送未使用原连接")
	}
	assertVoiceRecipients(t, s, a.SessionID)
}

func TestVoiceTargetConcurrentSessionReuse(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	a := &mumble.User{Name: "a"}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b"}
	connectTestUser(t, s, b)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{Session: []uint32{b.SessionID}})
	s.users.Remove(b.SessionID)
	s.UnregisterConn(b.SessionID)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			u, ok := s.users.Add(mumble.User{Name: fmt.Sprintf("replacement-%d", i)})
			if !ok {
				t.Error("连接创建失败")
				return
			}
			c := connection.New(nil, nil, nil)
			c.SetSessionID(u.SessionID)
			c.SetActive()
			s.RegisterConn(u.SessionID, c)
			s.users.Remove(u.SessionID)
			s.UnregisterConn(u.SessionID)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if got := s.getVoiceTargetRecipients(a.SessionID, 1); len(got) != 0 {
				t.Errorf("旧目标泄漏到 %v", got)
			}
			_ = s.router.Route(a.SessionID, 1, []byte{1})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s.storeVoiceTarget(ac, messages.VoiceTarget{ID: 2, Targets: []messages.VoiceTargetTarget{{Session: []uint32{b.SessionID}}}})
		}
	}()
	wg.Wait()
	// 发送者断线后，其目标也不能被继任连接继承。
	s.users.Remove(a.SessionID)
	s.UnregisterConn(a.SessionID)
	replacement := &mumble.User{Name: "new-owner"}
	connectTestUser(t, s, replacement)
	assertVoiceRecipients(t, s, replacement.SessionID)
}

func TestVoiceTargetSenderVoiceGates(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	a := &mumble.User{Name: "a"}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b"}
	_, bc := connectTestUser(t, s, b)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{Session: []uint32{b.SessionID}})
	packet := []byte{1, 2, 3}
	if err := s.router.Route(a.SessionID, 1, packet); err != nil {
		t.Fatal(err)
	}
	kind, got := readMessage(t, bc)
	if kind != protocol.MessageUDPTunnel || !bytes.Equal(got, packet) {
		t.Fatal("私聊路由未正常发送")
	}
	for _, state := range []mumble.VoiceState{{Mute: true}, {SelfMute: true}, {Suppress: true}} {
		s.users.UpdateUser(a.SessionID, func(u *mumble.User) { u.VoiceState = state })
		assertVoiceRecipients(t, s, a.SessionID)
	}
}

// 模拟 session 已复用、断线清理尚未执行的窗口，验证连接身份检查本身有效。
func TestVoiceTargetSessionReuseBeforeCleanup(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	a := &mumble.User{Name: "a"}
	ac, _ := connectTestUser(t, s, a)
	b := &mumble.User{Name: "b"}
	connectTestUser(t, s, b)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{Session: []uint32{b.SessionID}})
	s.users.Remove(b.SessionID)
	c := &mumble.User{Name: "c"}
	connectTestUser(t, s, c)
	if c.SessionID != b.SessionID {
		t.Fatal("未复用接收者 session")
	}
	assertVoiceRecipients(t, s, a.SessionID)
	// 原发送者的频道目标也不得被复用其 ID 的连接继承。
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{HasChannelID: true})
	s.users.Remove(a.SessionID)
	d := &mumble.User{Name: "d"}
	connectTestUser(t, s, d)
	if d.SessionID != a.SessionID {
		t.Fatal("未复用发送者 session")
	}
	assertVoiceRecipients(t, s, d.SessionID)
}
