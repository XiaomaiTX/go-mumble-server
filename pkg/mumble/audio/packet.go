package audio

import (
	"errors"
)

// ErrPacketTooShort is returned when the packet buffer is too short to parse.
var ErrPacketTooShort = errors.New("audio: packet too short")

// ParsedPacket is the result of parsing a client-to-server legacy binary audio packet.
type ParsedPacket struct {
	Position     [3]float32
	HasPosition  bool
	Codec        uint8  // Audio codec ID (bits 7-5 of header)
	Target       uint8  // Voice target (bits 4-0 of header)
	Sequence     int64  // Sequence number
	PayloadLen   int64  // Payload length in bytes (bit 13 masked out)
	IsTerminator bool   // True if bit 13 of payload length varint was set
	Payload      []byte // Audio frame data
}

// ParsePacket parses a client-to-server legacy binary UDP audio packet.
// The server receives these from clients; no session field is present.
// See docs/protocol/voice-data.md.
func ParsePacket(data []byte) (*ParsedPacket, error) {
	f, err := decodeLegacy(data, false)
	if err != nil {
		return nil, err
	}
	return &ParsedPacket{Codec: uint8(f.Codec), Target: uint8(f.Target), Sequence: int64(f.FrameNumber), PayloadLen: int64(len(f.OpusData)), IsTerminator: f.IsTerminator, Payload: f.OpusData, Position: f.Position, HasPosition: f.HasPosition}, nil
}
