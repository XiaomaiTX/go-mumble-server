package mumble

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/identity"
	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
)

type delayedRevalidationAuthority struct {
	fakeAuthority
	entered chan struct{}
	release chan struct{}
}

func (a *delayedRevalidationAuthority) Resolve(ctx context.Context, req identity.ResolveRequest) ([]identity.Identity, error) {
	close(a.entered)
	<-a.release
	return a.fakeAuthority.Resolve(ctx, req)
}

func TestRevalidationLateResultsCannotAffectReusedGeneration(t *testing.T) {
	for _, outcome := range []string{"allow", "reject", "timeout", "authority_failure", "missing_grace_expiry", "unvalidated_failure", "name_conflict"} {
		t.Run(outcome, func(t *testing.T) {
			s := newACLTestServer(t)
			closeTestDBBeforeTempCleanup(t, s)
			// 精确构造 generation 10 → 11，保持同一个 SessionID/UserID。
			for i := 0; i < 9; i++ {
				u, _ := s.users.Add(pkgmumble.User{Name: "reserve"})
				s.users.RemoveIfGeneration(u.SessionID, u.SessionGeneration)
			}
			old := &pkgmumble.User{Name: "old", UserID: 173, ExternalIdentity: true, IdentityLastValidatedAt: time.Now().Add(-time.Hour)}
			if outcome == "unvalidated_failure" {
				old.IdentityLastValidatedAt = time.Time{}
			}
			oldRef := connectRemoteTestUser(t, s, old, &recordingSink{})
			if oldRef.Generation != 10 {
				t.Fatalf("old generation = %d", oldRef.Generation)
			}
			a := &delayedRevalidationAuthority{entered: make(chan struct{}), release: make(chan struct{})}
			switch outcome {
			case "allow", "name_conflict":
				a.resolved = []identity.Identity{{UserID: 173, Eligible: true, Name: "observer", Groups: []string{"late"}, IdentityVersion: 99}}
			case "reject":
				a.resolved = []identity.Identity{{UserID: 173, Eligible: false}}
			case "timeout":
				a.err = context.DeadlineExceeded
			case "authority_failure", "unvalidated_failure":
				a.err = errors.New("authority unavailable")
			}
			s.authority = a
			done := make(chan error, 1)
			go func() { done <- s.RevalidateUserNow(context.Background(), 173) }()
			select {
			case <-a.entered:
			case <-time.After(time.Second):
				t.Fatal("未发起重验")
			}
			s.UnregisterConn(oldRef)
			s.users.RemoveIfGeneration(oldRef.SessionID, oldRef.Generation)
			fresh := &pkgmumble.User{Name: "fresh", UserID: 173, ExternalIdentity: true, ExternalGroups: []string{"new"}, IdentityLastValidatedAt: time.Now()}
			freshSink := &recordingSink{}
			freshRef := connectRemoteTestUser(t, s, fresh, freshSink)
			if freshRef.SessionID != oldRef.SessionID || freshRef.Generation != 11 {
				t.Fatalf("没有按预期复用: %+v", freshRef)
			}
			observerSink := &recordingSink{}
			observer := &pkgmumble.User{Name: "observer"}
			observerRef := connectRemoteTestUser(t, s, observer, observerSink)
			before, _ := s.users.SnapshotRef(freshRef)
			close(a.release)
			select {
			case err := <-done:
				if !errors.Is(err, a.err) {
					t.Fatalf("重验错误: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("迟到结果处理阻塞")
			}
			after, ok := s.users.SnapshotRef(freshRef)
			if !ok || !reflect.DeepEqual(before, after) || !s.registry.Active(freshRef) {
				t.Fatalf("新 generation 被修改: before=%+v after=%+v", before, after)
			}
			if s.KickSessionRef(oldRef, "late eviction") {
				t.Fatal("stale kick 成功")
			}
			// 通过 FIFO flush 确认所有可能的迟到广播已经被检查，不依赖 sleep。
			for _, ref := range []cluster.SessionRef{freshRef, observerRef} {
				if err := s.control.CloseAfterFlush(ref, time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			for _, sink := range []*recordingSink{freshSink, observerSink} {
				for _, message := range sink.delivered() {
					if message.Type == protocol.MessageUserState || message.Type == protocol.MessageUserRemove {
						t.Fatalf("stale result 产生广播: %+v", message)
					}
				}
			}
		})
	}
}
