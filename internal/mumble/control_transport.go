package mumble

import (
	"context"
	"time"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// controlDeferredLimit bounds the deferred queue a Syncing session accumulates
// before ordinary broadcasts are treated as a sync failure.
const controlDeferredLimit = 64

// controlFlushTimeout bounds barrier and final-message waits; mirrors
// connection.flushTimeout so edge delivery failure paths behave alike.
const controlFlushTimeout = 250 * time.Millisecond

// controlDeadline returns the absolute deadline used by SendThenClose and
// CloseAfterFlush callers.
func controlDeadline() time.Time {
	return time.Now().Add(controlFlushTimeout)
}

// controlSink adapts a local connection.Conn to the cluster ControlSink
// contract. This is the Local Edge half of the control plane: ordering comes
// from the connection's own FIFO writer, and Flush is the socket write
// receipt.
type controlSink struct {
	conn *connection.Conn
}

func (s controlSink) WriteControl(m cluster.ControlMessage) error {
	return s.conn.WriteMessage(m.Type, m.Message)
}

func (s controlSink) Flush(ctx context.Context) error {
	return s.conn.AwaitFlush(ctx)
}

func (s controlSink) Close() error {
	return s.conn.Close()
}

// sendThenClose delivers a dying session's final control message and closes
// the transport after the write receipt (SendThenClose contract): kicks, bans
// and rejects must be the last message on the wire, never followed by another
// queued write. The full SessionRef is required: a final message for a stale
// generation must not reach the session that reused the ID.
// 不绕过关闭队列重试原始连接，否则可能重复或追加 final message。
func (s *Server) sendThenClose(ref cluster.SessionRef, msgType protocol.MessageType, msg messages.Message) {
	if _, ok := s.registry.Snapshot(ref); !ok {
		return
	}
	_ = s.control.SendThenClose(ref, cluster.ControlMessage{Type: msgType, Message: msg}, controlDeadline())
}

// controlFailed 清理失败的精确 generation，远端 sink 无需模拟本地断线回调。
func (s *Server) controlFailed(ref cluster.SessionRef, _ error) {
	_, _ = s.registry.BeginCloseAndNotify(ref, func(snap cluster.SessionSnapshot) {
		if snap.Announced {
			s.Broadcast(ref.SessionID, protocol.MessageUserRemove, &messages.UserRemove{Session: ref.SessionID})
		}
	})
	s.UnregisterConn(ref)
	if u, ok := s.users.RemoveIfGeneration(ref.SessionID, ref.Generation); ok && u.ChannelID != 0 {
		s.UpdateChannelCrypto(u.ChannelID)
	}
}

// closeFailedSync rolls back a session whose initial sync failed: no Active
// commit, no join announce, and every binding is cleaned generation-safe.
func (s *Server) closeFailedSync(ref cluster.SessionRef) {
	_ = s.control.AbortSync(ref)
	s.UnregisterConn(ref)
	s.users.RemoveIfGeneration(ref.SessionID, ref.Generation)
}
