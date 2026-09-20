package cluster

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	mumbleaudio "github.com/dchote/go-mumble-server/pkg/mumble/audio"
)

const (
	WireMajor    uint16 = 1
	WireMinor    uint16 = 0
	MaxWireFrame        = 4 << 20
)

type ProtocolVersion struct {
	Major uint16 `json:"major"`
	Minor uint16 `json:"minor"`
}

type CoreEpoch string

type EdgeInstanceRef struct {
	EdgeID     EdgeID `json:"edge_id"`
	Generation uint64 `json:"generation"`
}

func (r EdgeInstanceRef) Valid() bool { return r.EdgeID != "" && r.Generation != 0 }

type ClientConnRef struct {
	EdgeInstance EdgeInstanceRef `json:"edge_instance"`
	ConnectionID uint64          `json:"connection_id"`
}

func (r ClientConnRef) Valid() bool { return r.EdgeInstance.Valid() && r.ConnectionID != 0 }

type WireType string

const (
	WireHello          WireType = "hello"
	WireHelloAck       WireType = "hello_ack"
	WireHeartbeat      WireType = "heartbeat"
	WireAuthRequest    WireType = "auth_request"
	WireAuthResult     WireType = "auth_result"
	WireTransportReady WireType = "transport_ready"
	WireControlUp      WireType = "control_up"
	WireControlDown    WireType = "control_down"
	WireControlFlush   WireType = "control_flush"
	WireControlFlushed WireType = "control_flushed"
	WireVoiceUp        WireType = "voice_up"
	WireVoiceDown      WireType = "voice_down"
	WireDisconnect     WireType = "disconnect"
	WireCloseSession   WireType = "close_session"
)

type WireEnvelope struct {
	Type WireType        `json:"type"`
	ID   uint64          `json:"id,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Hello struct {
	Version      ProtocolVersion `json:"version"`
	EdgeID       EdgeID          `json:"edge_id"`
	Capabilities []string        `json:"capabilities,omitempty"`
}

type HelloAck struct {
	Version      ProtocolVersion `json:"version"`
	CoreEpoch    CoreEpoch       `json:"core_epoch"`
	EdgeInstance EdgeInstanceRef `json:"edge_instance"`
	Capabilities []string        `json:"capabilities,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type AuthRequest struct {
	ConnectionID        uint64   `json:"connection_id"`
	Username            string   `json:"username"`
	Password            string   `json:"password,omitempty"`
	Tokens              []string `json:"tokens,omitempty"`
	CertHash            string   `json:"cert_hash,omitempty"`
	CertificateVerified bool     `json:"certificate_verified"`
	RemoteIP            string   `json:"remote_ip,omitempty"`
	ClientVersion       uint64   `json:"client_version"`
	ClientCryptoModes   uint32   `json:"client_crypto_modes"`
}

type AuthResponse struct {
	ConnectionID uint64     `json:"connection_id"`
	Session      SessionRef `json:"session"`
	Username     string     `json:"username,omitempty"`
	Rejected     bool       `json:"rejected"`
	RejectType   uint32     `json:"reject_type,omitempty"`
	Reason       string     `json:"reason,omitempty"`
}

type SessionEvent struct {
	ConnectionID uint64     `json:"connection_id,omitempty"`
	Session      SessionRef `json:"session"`
	CryptoMode   string     `json:"crypto_mode,omitempty"`
}

type ControlWire struct {
	ConnectionID uint64     `json:"connection_id,omitempty"`
	Session      SessionRef `json:"session"`
	MessageType  uint16     `json:"message_type"`
	Payload      []byte     `json:"payload,omitempty"`
}

type VoiceUp struct {
	Session SessionRef        `json:"session"`
	Frame   mumbleaudio.Frame `json:"frame"`
}

type VoiceDown struct {
	Batch VoiceBatch `json:"batch"`
}

var (
	ErrWireFrameTooLarge = errors.New("edge-core frame exceeds limit")
	ErrWireMessageType   = errors.New("invalid edge-core message type")
)

func WriteWire(w io.Writer, typ WireType, id uint64, value any) error {
	var data []byte
	var err error
	if value != nil {
		data, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	body, err := json.Marshal(WireEnvelope{Type: typ, ID: id, Data: data})
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > MaxWireFrame {
		return ErrWireFrameTooLarge
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err = w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func ReadWire(r io.Reader) (WireEnvelope, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return WireEnvelope{}, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > MaxWireFrame {
		return WireEnvelope{}, ErrWireFrameTooLarge
	}
	body := make([]byte, int(n))
	if _, err := io.ReadFull(r, body); err != nil {
		return WireEnvelope{}, err
	}
	var env WireEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return WireEnvelope{}, fmt.Errorf("decode edge-core frame: %w", err)
	}
	if !validWireType(env.Type) {
		return WireEnvelope{}, ErrWireMessageType
	}
	return env, nil
}

func DecodeWire(env WireEnvelope, dst any) error {
	if dst == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, dst)
}

func validWireType(t WireType) bool {
	switch t {
	case WireHello, WireHelloAck, WireHeartbeat, WireAuthRequest, WireAuthResult,
		WireTransportReady, WireControlUp, WireControlDown, WireControlFlush, WireControlFlushed, WireVoiceUp, WireVoiceDown,
		WireDisconnect, WireCloseSession:
		return true
	default:
		return false
	}
}
