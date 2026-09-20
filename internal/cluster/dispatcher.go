package cluster

import (
	"context"
	"log/slog"
	"sync"

	mumbleaudio "github.com/dchote/go-mumble-server/pkg/mumble/audio"
)

type VoiceTransport interface {
	DeliverVoice(context.Context, VoiceBatch) error
}

// Dispatcher groups canonical recipients by Edge. A transport failure is
// isolated to that Edge and does not fail the sender's protocol operation.
type Dispatcher struct {
	registry *Registry
	mu       sync.RWMutex
	voice    map[EdgeID]VoiceTransport
}

func NewDispatcher(registry *Registry) *Dispatcher {
	return &Dispatcher{registry: registry, voice: make(map[EdgeID]VoiceTransport)}
}

func (d *Dispatcher) RegisterVoiceTransport(edge EdgeID, transport VoiceTransport) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if transport == nil {
		delete(d.voice, edge)
		return
	}
	d.voice[edge] = transport
}

// VoiceTransportFor reports the registered voice transport for an edge.
// Read-only composition introspection; it changes no dispatch state.
func (d *Dispatcher) VoiceTransportFor(edge EdgeID) VoiceTransport {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.voice[edge]
}

func (d *Dispatcher) DispatchVoice(ctx context.Context, batch VoiceBatch) {
	if !d.registry.Active(batch.Sender) {
		return
	}
	groups := make(map[EdgeID][]VoiceRecipient)
	for _, recipient := range batch.Recipients {
		s, ok := d.registry.Snapshot(recipient.Session)
		if !ok || s.State != StateActive {
			continue
		}
		groups[s.Edge] = append(groups[s.Edge], recipient)
	}
	d.mu.RLock()
	transports := make(map[EdgeID]VoiceTransport, len(groups))
	for edge := range groups {
		transports[edge] = d.voice[edge]
	}
	d.mu.RUnlock()
	for edge, recipients := range groups {
		transport := transports[edge]
		if transport == nil {
			continue
		}
		copied := VoiceBatch{Sender: batch.Sender, Frame: cloneFrame(batch.Frame), Recipients: append([]VoiceRecipient(nil), recipients...)}
		if err := transport.DeliverVoice(ctx, copied); err != nil {
			slog.Warn("edge voice delivery failed", "edge", edge, "err", err)
		}
	}
}

func cloneFrame(frame mumbleaudio.Frame) mumbleaudio.Frame {
	frame.OpusData = append([]byte(nil), frame.OpusData...)
	return frame
}
