package mumble

import (
	"context"

	"github.com/dchote/go-mumble-server/internal/cluster"
	mumbleaudio "github.com/dchote/go-mumble-server/pkg/mumble/audio"
)

type localVoiceTransport struct{ s *Server }

func (t localVoiceTransport) DeliverVoice(_ context.Context, batch cluster.VoiceBatch) error {
	cache := mumbleaudio.NewEncodingCache()
	for _, recipient := range batch.Recipients {
		// Revalidate generation and lifecycle immediately before touching local
		// transport state. This closes the disconnect/reuse race.
		if !t.s.registry.Active(recipient.Session) {
			continue
		}
		t.s.connMu.RLock()
		conn := t.s.conns[recipient.Session.SessionID]
		addr, _ := t.s.addrBySession.Load(recipient.Session)
		t.s.connMu.RUnlock()
		if conn == nil || conn.SessionGeneration() != recipient.Session.Generation {
			continue
		}
		delivery := mumbleaudio.Delivery{
			Frame:            batch.Frame,
			Context:          recipient.Context,
			VolumeAdjustment: recipient.Volume,
		}
		delivery.HasPosition = recipient.HasPosition
		_ = t.s.sendDeliveryTo(recipient.Session.SessionID, conn, addr, delivery, cache)
	}
	return nil
}
