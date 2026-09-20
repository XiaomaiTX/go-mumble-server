package user

import (
	"reflect"
	"sync"
	"testing"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/pkg/mumble"
)

func TestIdentityGenerationGuardIsAtomicWithReuse(t *testing.T) {
	m := NewManager(nil, 10)
	for i := 0; i < 100; i++ {
		old, _ := m.Add(mumble.User{Name: "old"})
		ref := cluster.SessionRef{SessionID: old.SessionID, Generation: old.SessionGeneration}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 30; n++ {
				m.UpdateIdentityRef(ref, func(u *mumble.User) { u.Name = "late"; u.ExternalGroups = []string{"old"} })
				m.SnapshotRef(ref)
			}
		}()
		m.RemoveIfGeneration(ref.SessionID, ref.Generation)
		fresh, ok := m.Add(mumble.User{Name: "fresh", ExternalGroups: []string{"fresh"}})
		if !ok || fresh.SessionID != ref.SessionID {
			t.Fatal("未复用 session")
		}
		wg.Wait()
		if _, ok := m.SnapshotRef(ref); ok {
			t.Fatal("stale snapshot 成功")
		}
		if _, ok := m.UpdateIdentityRefAndPublish(ref, func(*mumble.User) { t.Error("执行了 stale 更新") }, func(mumble.User) { t.Error("发布了 stale 更新") }); ok {
			t.Fatal("stale update 成功")
		}
		if _, ok := m.RemoveIfGeneration(ref.SessionID, ref.Generation); ok {
			t.Fatal("stale eviction 成功")
		}
		got, _ := m.Snapshot(fresh.SessionID)
		if !reflect.DeepEqual(got, fresh) {
			t.Fatalf("新会话被修改: %+v", got)
		}
		m.RemoveIfGeneration(fresh.SessionID, fresh.SessionGeneration)
	}
}
