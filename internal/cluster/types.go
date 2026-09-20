package cluster

import mumbleaudio "github.com/dchote/go-mumble-server/pkg/mumble/audio"

// EdgeID identifies one logical edge inside the lifetime of this Core process.
// A future wire protocol must pair it with an edge instance generation.
type EdgeID string

const LocalEdgeID EdgeID = "local"

// SessionRef is the non-reusable identity of a logical client session.
// A future cross-process representation must additionally carry a Core epoch.
type SessionRef struct {
	SessionID  uint32
	Generation uint64
}

func (r SessionRef) Valid() bool { return r.SessionID != 0 && r.Generation != 0 }

type SessionState uint8

const (
	StateSyncing SessionState = iota + 1
	StateActive
	StateClosing
	StateClosed
)

type VoiceRecipient struct {
	Session     SessionRef
	Context     mumbleaudio.Context
	Volume      float32
	HasPosition bool
}

type VoiceBatch struct {
	Sender     SessionRef
	Frame      mumbleaudio.Frame
	Recipients []VoiceRecipient
}
