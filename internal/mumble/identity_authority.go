package mumble

import (
	"context"
	"errors"
	"strings"

	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/auth"
	"github.com/dchote/go-mumble-server/internal/database/models"
	"github.com/dchote/go-mumble-server/internal/identity"
	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
	"gorm.io/gorm"
)

// localAuthority preserves standalone behavior behind the same boundary used
// by production external authentication.
type localAuthority struct{ server *Server }

func (a *localAuthority) External() bool { return false }

func (a *localAuthority) Authenticate(_ context.Context, req identity.AuthenticateRequest) (identity.AuthenticateResult, error) {
	server := a.server
	var apiUserID uint
	if server.cfg.ServerPassword != "" {
		if req.Password != server.cfg.ServerPassword {
			id, hash, _, found := server.users.LookupAPIUser(req.Username)
			if !found || !auth.ComparePassword(hash, req.Password) {
				return identity.AuthenticateResult{Decision: identity.DecisionDeny}, nil
			}
			apiUserID = uint(id)
		}
	} else if req.Password != "" {
		id, hash, _, found := server.users.LookupAPIUser(req.Username)
		if found {
			if !auth.ComparePassword(hash, req.Password) {
				return identity.AuthenticateResult{Decision: identity.DecisionDeny}, nil
			}
			apiUserID = uint(id)
		}
	}
	userID := uint32(0)
	if apiUserID != 0 {
		userID = acl.MakeAPIUserID(apiUserID)
	} else if uid, hash, certHash, found := server.lookupRegisteredUser(req.Username); found {
		if !registeredUserCredentialsValid(hash, certHash, req.Password, req.CertificateHash) {
			return identity.AuthenticateResult{Decision: identity.DecisionDeny}, nil
		}
		userID = uid
	}
	if req.Username == pkgmumble.SuperUserName && userID == 0 {
		return identity.AuthenticateResult{Decision: identity.DecisionDeny}, nil
	}
	return identity.AuthenticateResult{Decision: identity.DecisionAllow, Identity: identity.Identity{Eligible: true, UserID: userID, Name: req.Username}}, nil
}

func (a *localAuthority) Resolve(_ context.Context, req identity.ResolveRequest) ([]identity.Identity, error) {
	result := make([]identity.Identity, 0, len(req.UserIDs)+len(req.Names))
	for _, userID := range req.UserIDs {
		if online, ok := a.server.users.SnapshotByUserID(userID); ok {
			result = append(result, identity.Identity{Eligible: true, UserID: userID, Name: online.Name})
			continue
		}
		name, ok := a.localNameForID(userID)
		result = append(result, identity.Identity{Eligible: ok, UserID: userID, Name: name})
	}
	for _, name := range req.Names {
		if online, ok := a.server.users.SnapshotByName(name); ok {
			result = append(result, identity.Identity{Eligible: true, UserID: online.UserID, Name: name})
			continue
		}
		userID, ok := a.localIDForName(name)
		result = append(result, identity.Identity{Eligible: ok, UserID: userID, Name: name})
	}
	return result, nil
}

func (a *localAuthority) localNameForID(userID uint32) (string, bool) {
	if acl.IsAPIUserID(userID) {
		var row models.User
		err := a.server.db.First(&row, acl.APIUserIDToDBID(userID)).Error
		return row.Username, err == nil
	}
	var row models.RegisteredUser
	err := a.server.db.Where("server_id = ? AND user_id = ?", a.server.chans.ServerID(), userID).First(&row).Error
	return row.Name, err == nil
}

func (a *localAuthority) localIDForName(name string) (uint32, bool) {
	var apiUser models.User
	if err := a.server.db.Where("username = ?", name).First(&apiUser).Error; err == nil {
		return acl.MakeAPIUserID(apiUser.ID), true
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false
	}
	var row models.RegisteredUser
	err := a.server.db.Where("server_id = ? AND name = ?", a.server.chans.ServerID(), strings.TrimSpace(name)).First(&row).Error
	return uint32(row.UserID), err == nil
}
