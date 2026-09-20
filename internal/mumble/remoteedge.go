package mumble

import (
	"context"
	"net"
	"time"

	"github.com/dchote/go-mumble-server/internal/acl"
	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/edge"
	"github.com/dchote/go-mumble-server/internal/identity"
	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// AuthenticateRemote is the authoritative authentication entry point for a
// remote physical connection. It allocates and binds a Syncing session but
// deliberately does not expose it as Active until TransportReadyRemote.
func (s *Server) EdgeConnected(instance cluster.EdgeInstanceRef, transport cluster.VoiceTransport) {
	if generation, ok := s.registry.EdgeGeneration(instance.EdgeID); ok && generation == instance.Generation {
		s.dispatcher.RegisterVoiceTransport(instance.EdgeID, transport)
	}
}

func (s *Server) AuthenticateRemote(ctx context.Context, instance cluster.EdgeInstanceRef, req cluster.AuthRequest) cluster.AuthResponse {
	reject := func(t messages.RejectType, reason string) cluster.AuthResponse {
		return cluster.AuthResponse{ConnectionID: req.ConnectionID, Rejected: true, RejectType: uint32(t), Reason: reason}
	}
	if generation, ok := s.registry.EdgeGeneration(instance.EdgeID); !ok || generation != instance.Generation {
		return reject(messages.RejectAuthenticatorFail, "Stale edge instance")
	}
	addr := &net.TCPAddr{IP: net.ParseIP(req.RemoteIP)}
	if s.bans.IsBanned(addr, req.CertHash) {
		return reject(messages.RejectWrongServerPW, "Banned")
	}
	if req.Username == "" {
		return reject(messages.RejectInvalidUsername, "Username required")
	}
	if _, ok := s.users.SnapshotByName(req.Username); ok {
		return reject(messages.RejectUsernameInUse, "Username in use")
	}
	if s.cfg.MaxUsers > 0 && s.users.Count() >= s.cfg.MaxUsers {
		return reject(messages.RejectServerFull, "Server full")
	}
	if s.authorityErr != nil || s.authority == nil {
		return reject(messages.RejectAuthenticatorFail, "Identity service unavailable")
	}
	authReq := identity.AuthenticateRequest{ServerInstanceID: s.cfg.ExternalAuthServerInstanceID, Username: req.Username, Password: req.Password, CertificateHash: req.CertHash, RemoteIP: req.RemoteIP}
	if s.authority.External() && authReq.Password == "" {
		return reject(messages.RejectWrongServerPW, "Password required")
	}
	result, err := s.authority.Authenticate(ctx, authReq)
	if err != nil {
		return reject(messages.RejectAuthenticatorFail, "Identity service unavailable")
	}
	resolved := result.Identity
	if result.Decision != identity.DecisionAllow || !resolved.Eligible || resolved.Name == "" {
		return reject(messages.RejectWrongUserPW, "Invalid credentials")
	}
	u := pkgmumble.User{UserID: resolved.UserID, ChannelID: s.joinChannelFor(resolved.UserID), Name: resolved.Name, AccessTokens: sanitizedClientTokens(req.Tokens), ExternalGroups: append([]string(nil), resolved.Groups...), ExternalIdentity: s.authority.External(), IdentityVersion: resolved.IdentityVersion, PolicyVersion: resolved.PolicyVersion, IdentityLastValidatedAt: time.Now(), IsSuperUser: resolved.IsSuperUser, ClientVersion: req.ClientVersion, Address: req.RemoteIP, CertHash: req.CertHash, CertificateVerified: req.CertificateVerified}
	stored, ok := s.users.Add(u)
	if !ok {
		return reject(messages.RejectServerFull, "Server full")
	}
	ref := cluster.SessionRef{SessionID: stored.SessionID, Generation: stored.SessionGeneration}
	if err := s.registry.Bind(ref, instance.EdgeID); err != nil {
		s.users.RemoveIfGeneration(ref.SessionID, ref.Generation)
		return reject(messages.RejectAuthenticatorFail, "Session setup failed")
	}
	s.invalidateACLCache()
	return cluster.AuthResponse{ConnectionID: req.ConnectionID, Session: ref, Username: stored.Name}
}

func (s *Server) TransportReadyRemote(ctx context.Context, instance cluster.EdgeInstanceRef, event cluster.SessionEvent, sink cluster.ControlSink) error {
	if !s.remoteSessionOwned(instance, event.Session) {
		return cluster.ErrStaleSession
	}
	u, ok := s.users.SnapshotRef(event.Session)
	if !ok {
		return cluster.ErrStaleSession
	}
	if event.CryptoMode != "" {
		if updated, found := s.users.UpdateUser(event.Session.SessionID, func(live *pkgmumble.User) { live.CryptoMode = event.CryptoMode }); found {
			u = updated
		}
	}
	if err := s.control.BeginSync(event.Session, sink, controlDeferredLimit); err != nil {
		return err
	}
	if err := s.sendRemoteSync(u, event.Session); err != nil {
		s.closeFailedSync(event.Session)
		return err
	}
	if err := s.control.CommitSyncAndAnnounce(ctx, event.Session, func() { s.Broadcast(event.Session.SessionID, protocol.MessageUserState, userToState(u)) }); err != nil {
		s.closeFailedSync(event.Session)
		return err
	}
	s.UpdateChannelCrypto(u.ChannelID)
	return nil
}

func (s *Server) sendRemoteSync(u pkgmumble.User, ref cluster.SessionRef) error {
	initial := func(t protocol.MessageType, m messages.Message) error {
		return s.control.SendInitial(ref, cluster.ControlMessage{Type: t, Message: m})
	}
	if err := initial(protocol.MessageCodecVersion, &messages.CodecVersion{Opus: true}); err != nil {
		return err
	}
	restricted := s.enterRestrictedChannels()
	subject := acl.SubjectOf(u)
	for _, ch := range s.chans.GetTree() {
		if err := initial(protocol.MessageChannelState, s.channelToStateFor(ch, subject, restricted[ch.ID])); err != nil {
			return err
		}
	}
	self := userToState(u)
	self.ListeningChannelAdd = s.listeners.ChannelsFor(u.SessionID)
	self.ListeningVolumeAdjustment = s.listeners.ListeningVolumes(u.SessionID)
	if err := initial(protocol.MessageUserState, self); err != nil {
		return err
	}
	for _, other := range s.users.SnapshotAll() {
		if other.SessionID == u.SessionID {
			continue
		}
		oref := cluster.SessionRef{SessionID: other.SessionID, Generation: other.SessionGeneration}
		if !s.registry.Active(oref) {
			continue
		}
		state := userToState(other)
		state.ListeningChannelAdd = s.listeners.ChannelsFor(other.SessionID)
		if err := initial(protocol.MessageUserState, state); err != nil {
			return err
		}
	}
	perms := uint64(s.aclPermissions(subject, s.chans.RootID()))
	if err := initial(protocol.MessageServerSync, &messages.ServerSync{Session: u.SessionID, MaxBandwidth: uint32(s.cfg.MaxBandwidth), WelcomeText: s.cfg.WelcomeText, Permissions: perms}); err != nil {
		return err
	}
	return initial(protocol.MessageServerConfig, &messages.ServerConfig{MaxBandwidth: uint32(s.cfg.MaxBandwidth), WelcomeText: s.cfg.WelcomeText, AllowHTML: true, MessageLength: uint32(s.maxTextLength()), ImageMessageLength: uint32(s.maxImageLength()), MaxUsers: uint32(s.cfg.MaxUsers), RecordingAllowed: s.recordingAllowed()})
}

func (s *Server) ControlRemote(_ context.Context, instance cluster.EdgeInstanceRef, msg cluster.ControlWire) error {
	if !s.remoteSessionOwned(instance, msg.Session) {
		return cluster.ErrStaleSession
	}
	kind := protocol.MessageType(msg.MessageType)
	if kind >= protocol.MessageCount || edge.OwnershipOf(kind) != edge.OwnershipCore {
		return cluster.ErrWireMessageType
	}
	return s.CoreIngressHandler()(kind, append([]byte(nil), msg.Payload...), remotePeer{server: s, ref: msg.Session})
}
func (s *Server) VoiceRemote(_ context.Context, instance cluster.EdgeInstanceRef, voice cluster.VoiceUp) error {
	if !s.remoteSessionOwned(instance, voice.Session) || !s.registry.Active(voice.Session) {
		return cluster.ErrStaleSession
	}
	voice.Frame.OpusData = append([]byte(nil), voice.Frame.OpusData...)
	return s.router.RouteRef(voice.Session, voice.Frame)
}
func (s *Server) DisconnectRemote(instance cluster.EdgeInstanceRef, event cluster.SessionEvent) {
	if s.remoteSessionOwned(instance, event.Session) {
		s.cleanupRemote(event.Session)
	}
}
func (s *Server) EdgeDisconnected(instance cluster.EdgeInstanceRef) {
	for _, ref := range s.registry.SessionsForEdge(instance) {
		s.cleanupRemote(ref)
	}
	s.dispatcher.RegisterVoiceTransport(instance.EdgeID, nil)
}
func (s *Server) remoteSessionOwned(instance cluster.EdgeInstanceRef, ref cluster.SessionRef) bool {
	g, ok := s.registry.EdgeGeneration(instance.EdgeID)
	if !ok || g != instance.Generation {
		return false
	}
	snap, ok := s.registry.Snapshot(ref)
	return ok && snap.Edge == instance.EdgeID
}
func (s *Server) cleanupRemote(ref cluster.SessionRef) {
	u, exists := s.users.SnapshotRef(ref)
	announced := s.UnregisterConn(ref)
	removed, ok := s.users.RemoveIfGeneration(ref.SessionID, ref.Generation)
	if announced {
		s.Broadcast(ref.SessionID, protocol.MessageUserRemove, &messages.UserRemove{Session: ref.SessionID})
	}
	if ok && removed.ChannelID != 0 {
		s.UpdateChannelCrypto(removed.ChannelID)
	} else if exists && u.ChannelID != 0 {
		s.UpdateChannelCrypto(u.ChannelID)
	}
}

type remotePeer struct {
	server *Server
	ref    cluster.SessionRef
}

func (p remotePeer) SessionID() uint32         { return p.ref.SessionID }
func (p remotePeer) SessionGeneration() uint64 { return p.ref.Generation }
func (p remotePeer) WriteMessage(t protocol.MessageType, m messages.Message) error {
	return p.server.control.Send(p.ref, cluster.ControlMessage{Type: t, Message: m})
}

var _ cluster.RemoteEdgeBackend = (*Server)(nil)
var _ Peer = remotePeer{}
