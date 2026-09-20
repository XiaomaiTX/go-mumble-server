// Package edge hosts the client-facing Edge plane shared by the Local Edge
// (standalone/core modes, in-process with the Core) and the edge mode's
// EdgeRuntime: the physical client transport, the client ingress ownership
// boundary, and the edge-owned protocol helpers. It must never gain
// authoritative business state — user, channel, ACL, ban and voice routing
// decisions belong to the Core alone.
package edge

import (
	"encoding/binary"

	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

// ProtocolVersionV2 is the server's announced 1.5.0 protocol capability. It is
// the single source shared by every edge (local and remote) and by the Core's
// connection-level WireMode negotiation baseline.
const ProtocolVersionV2 = uint64(1<<48 | 5<<32)

// ProtocolVersionV1 is the same capability in the legacy 32-bit encoding.
const ProtocolVersionV1 = uint32(1<<16 | 5<<8) // 1.5.0 as (major<<16)|(minor<<8)|patch

// Server banner fields sent on every accepted client connection. Identical
// for the Local Edge and a remote Edge so clients cannot tell which edge
// flavor terminated them.
const (
	BannerRelease   = "go-mumble-server"
	BannerOS        = "Go"
	BannerOSVersion = "1.0"
	// BannerCryptoModes: lite(1) | legacy(2) | secure(4) — the server supports all.
	BannerCryptoModes = uint32(0x07)
)

// SendReject delivers a Reject as the final message on the wire and then
// closes the connection. Murmur disconnects immediately after sending Reject
// (murmur/Messages.cpp: sendMessage(reject) then disconnectSocket()). Official
// clients raise the password retry prompt from the disconnect event, keyed on
// the reject type, so a rejected connection must not linger or the prompt
// fires at a random later moment (ping watchdog, manual disconnect, next
// reconnect). The flush-then-close is async because the writer only drains
// once the client reads the Reject; the pipe write receipt itself orders
// close after delivery.
func SendReject(c *connection.Conn, typ messages.RejectType, reason string) error {
	writeErr := c.WriteMessage(protocol.MessageReject, &messages.Reject{Type: typ, Reason: reason})
	go c.CloseAfterFlush()
	return writeErr
}

// CryptSetupResync is the edge-owned CryptSetup nonce resync: it applies the
// client's nonce and answers with the server's current one. It reads only
// local UDP crypto state and never crosses the Core/Edge boundary.
func CryptSetupResync(c *connection.Conn, payload []byte) error {
	if c.Crypt == nil {
		return nil
	}
	var cs messages.CryptSetup
	if len(payload) > 0 {
		if err := cs.Unmarshal(payload); err != nil {
			return err
		}
	}
	if len(cs.ClientNonce) > 0 {
		_ = c.Crypt.SetDecNonce(cs.ClientNonce)
	}
	if len(cs.ServerNonce) > 0 || (len(cs.Key) == 0 && len(cs.ClientNonce) == 0) {
		encNonce := c.Crypt.EncNonce()
		if len(encNonce) > 0 {
			_ = c.WriteMessage(protocol.MessageCryptSetup, &messages.CryptSetup{ServerNonce: encNonce})
		}
	}
	return nil
}

// Versions reported in server-list ping replies. This is a display field for
// public-server listings, separate from the authenticated Version exchange.
// The legacy ping format cannot express v2 versions, so both fields carry the
// same 1.5.0 value in their respective encodings.
const (
	pingLegacyVersion  = uint32(1<<16 | 5<<8)  // 1.5.0 as (major<<16)|(minor<<8)|patch
	pingVersionV2      = uint64(1<<48 | 5<<32) // 1.5.0 as major<<48|minor<<32|patch<<16
	pingHeaderProtobuf = byte(0x01)            // UDPMessageType::Ping, new (1.5+) format
)

// PlainPingReply builds the reply for an unencrypted server-list probe, or
// returns nil when data is not an extended-information probe. Murmur only
// answers probes that request extended information (expectExtended); plain
// echo pings from unknown senders get nothing, which keeps the socket from
// being a reflection amplifier.
func PlainPingReply(data []byte, userCount, maxUsers, maxBandwidth int) []byte {
	// Legacy (≤1.4) probe: 12 bytes, no header, leading u32 zero.
	if len(data) == 12 && binary.BigEndian.Uint32(data[0:4]) == 0 {
		out := make([]byte, 24)
		binary.BigEndian.PutUint32(out[0:4], pingLegacyVersion)
		copy(out[4:12], data[4:12]) // timestamp is opaque, echo request bytes verbatim
		binary.BigEndian.PutUint32(out[12:16], uint32(userCount))
		binary.BigEndian.PutUint32(out[16:20], uint32(maxUsers))
		binary.BigEndian.PutUint32(out[20:24], uint32(maxBandwidth))
		return out
	}
	// New (1.5+) probe: header byte plus protobuf MumbleUDP.Ping.
	if len(data) > 1 && data[0] == pingHeaderProtobuf {
		return ProtobufPingReply(data[1:], userCount, maxUsers, maxBandwidth)
	}
	return nil
}

// ProtobufPingReply answers a 1.5+ probe encoded as MumbleUDP.Ping. Only the
// varint fields of Ping are inspected (timestamp=1,
// request_extended_information=2); the reply echoes the timestamp and fills
// server_version_v2=3, user_count=4, max_user_count=5,
// max_bandwidth_per_user=6.
func ProtobufPingReply(body []byte, userCount, maxUsers, maxBandwidth int) []byte {
	timestamp, requestInfo, err := audio.DecodePing(audio.WireProtobuf, append([]byte{1}, body...))
	if err != nil {
		return nil
	}
	if !requestInfo {
		return nil
	}
	out := make([]byte, 1, 32)
	out[0] = pingHeaderProtobuf
	out = AppendProtoVarintField(out, 1, timestamp)
	out = AppendProtoVarintField(out, 3, pingVersionV2)
	out = AppendProtoVarintField(out, 4, uint64(userCount))
	out = AppendProtoVarintField(out, 5, uint64(maxUsers))
	out = AppendProtoVarintField(out, 6, uint64(maxBandwidth))
	return out
}

// DecodeProtoVarint decodes a protobuf base-128 varint, returning its value and
// the number of bytes consumed, or -1 when the input is truncated or overlong.
func DecodeProtoVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		if i == 9 && b[i] > 1 {
			return 0, -1
		}
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, -1
}

// AppendProtoVarintField appends a protobuf varint field with wire type 0.
func AppendProtoVarintField(buf []byte, field uint64, v uint64) []byte {
	buf = AppendProtoVarint(buf, field<<3)
	buf = AppendProtoVarint(buf, v)
	return buf
}

// AppendProtoVarint appends a base-128 varint.
func AppendProtoVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

// ClientVersionFull resolves a client's Version message to Mumble's packed
// 64-bit version_v2 form, preferring version_v2 and falling back to the
// legacy 32-bit version_v1 encoding. Mirrors
// MumbleProto::getVersion (research/mumble/src/ProtoUtils.cpp).
func ClientVersionFull(v messages.Version) uint64 {
	if v.VersionV2 != 0 {
		return v.VersionV2
	}
	if v.VersionV1 != 0 {
		major := uint64((v.VersionV1 & 0xFFFF0000) >> 16)
		minor := uint64((v.VersionV1 & 0xFF00) >> 8)
		patch := uint64(v.VersionV1 & 0xFF)
		return major<<48 | minor<<32 | patch<<16
	}
	return 0
}
