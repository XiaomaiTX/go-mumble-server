package mumble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/identity"
	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

func (s *Server) StartIdentityRevalidation(ctx context.Context) error {
	if s.authority == nil || !s.authority.External() {
		<-ctx.Done()
		return nil
	}
	interval := s.cfg.ExternalAuthRevalidate
	if interval <= 0 {
		interval = 45 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.revalidateSnapshots(ctx, s.externalUserSnapshots()); err != nil {
				slog.Warn("periodic identity revalidation failed", "err", err)
			}
		}
	}
}

// RevalidateUserNow handles a provider invalidation signal. The caller supplies
// only a stable ID; this method always pulls the current truth from the provider.
func (s *Server) RevalidateUserNow(ctx context.Context, userID uint32) error {
	if s.authority == nil || !s.authority.External() {
		return errors.New("external identity mode is not enabled")
	}
	var selected []externalSession
	for _, session := range s.externalUserSnapshots() {
		if session.UserID == userID {
			selected = append(selected, session)
		}
	}
	if len(selected) == 0 {
		return nil
	}
	return s.revalidateSnapshots(ctx, selected)
}

type externalSession struct {
	Ref             cluster.SessionRef
	UserID          uint32
	LastValidatedAt time.Time
}

func (s *Server) externalUserSnapshots() []externalSession {
	users := s.users.SnapshotAll()
	result := make([]externalSession, 0, len(users))
	for _, user := range users {
		if user.ExternalIdentity && user.UserID != 0 {
			result = append(result, externalSession{Ref: cluster.SessionRef{SessionID: user.SessionID, Generation: user.SessionGeneration}, UserID: user.UserID, LastValidatedAt: user.IdentityLastValidatedAt})
		}
	}
	return result
}

func (s *Server) revalidateSnapshots(ctx context.Context, sessions []externalSession) error {
	if len(sessions) == 0 {
		return nil
	}
	ids := make([]uint32, 0, len(sessions))
	seen := make(map[uint32]struct{}, len(sessions))
	for _, session := range sessions {
		if _, exists := seen[session.UserID]; !exists {
			seen[session.UserID] = struct{}{}
			ids = append(ids, session.UserID)
		}
	}
	identities, err := s.authority.Resolve(ctx, identity.ResolveRequest{UserIDs: ids})
	if err != nil {
		s.applyRevalidationFailure(sessions, time.Now())
		return err
	}
	byID := make(map[uint32]identity.Identity, len(identities))
	for _, resolved := range identities {
		if resolved.UserID != 0 {
			byID[resolved.UserID] = resolved
		}
	}
	now := time.Now()
	for _, session := range sessions {
		resolved, found := byID[session.UserID]
		if !found {
			s.applyRevalidationFailure([]externalSession{session}, now)
			continue
		}
		if !resolved.Eligible {
			s.KickSessionRef(session.Ref, "identity is no longer eligible")
			continue
		}
		conflict := false
		_ = s.registry.WithSession(session.Ref, func() error {
			identityChanged := false
			updated, ok := s.users.UpdateIdentityRefAndPublish(session.Ref, func(user *pkgmumble.User) {
				identityChanged = user.Name != resolved.Name || !slices.Equal(user.ExternalGroups, resolved.Groups) || user.IdentityVersion != resolved.IdentityVersion || user.PolicyVersion != resolved.PolicyVersion
				user.Name = resolved.Name
				user.ExternalGroups = append([]string(nil), resolved.Groups...)
				user.IdentityVersion = resolved.IdentityVersion
				user.PolicyVersion = resolved.PolicyVersion
				user.IdentityLastValidatedAt = now
			}, func(updated pkgmumble.User) {
				if identityChanged {
					s.Broadcast(updated.SessionID, protocol.MessageUserState, userToState(updated))
				}
			})
			conflict = !ok
			if ok && identityChanged {
				// 权限刷新也固定在原 generation，并与关闭串行化。
				s.invalidateACLCache()
				maySpeak := s.maySpeak(acl.SubjectOf(updated), updated.ChannelID)
				suppressChanged := false
				_, _ = s.users.UpdateIdentityRefAndPublish(session.Ref, func(user *pkgmumble.User) {
					suppressChanged = applySuppressFromSpeak(user, maySpeak)
				}, func(user pkgmumble.User) {
					if suppressChanged {
						s.Broadcast(0, protocol.MessageUserState, &messages.UserState{Session: user.SessionID, Suppress: user.Suppress, SetFields: messages.UserStateSetSession | messages.UserStateSetSuppress})
					}
				})
				s.refreshEnterStatesFor(updated, s.chans.GetTree(), s.enterRestrictedChannels())
			}
			return nil
		})
		if conflict {
			s.KickSessionRef(session.Ref, "canonical identity conflicts with an online user")
		}
	}
	return nil
}

func (s *Server) applyRevalidationFailure(sessions []externalSession, now time.Time) {
	grace := s.cfg.ExternalAuthStaleGrace
	if grace <= 0 {
		grace = 3 * time.Minute
	}
	for _, session := range sessions {
		if session.LastValidatedAt.IsZero() || now.Sub(session.LastValidatedAt) > grace {
			s.KickSessionRef(session.Ref, "identity could not be revalidated within stale grace")
		}
	}
}

func (s *Server) IdentityAuthorityStatus() error {
	if s.authorityErr != nil {
		return s.authorityErr
	}
	if s.authority == nil {
		return fmt.Errorf("identity authority is not configured")
	}
	return nil
}
