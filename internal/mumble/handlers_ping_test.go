package mumble

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
)

func newPingTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newACLTestServer(t)
	srv.cfg.MaxBandwidth = 128000
	if _, ok := srv.users.Add(pkgmumble.User{Name: "probe-user"}); !ok {
		t.Fatal("add probe user")
	}
	return srv
}

func TestPlainPingReply_LegacyProbe(t *testing.T) {
	srv := newPingTestServer(t)

	req := make([]byte, 12)
	binary.BigEndian.PutUint64(req[4:12], 0x1122334455667788) // arbitrary timestamp bytes
	reply := srv.plainPingReply(req)
	if reply == nil {
		t.Fatal("legacy probe got no reply")
	}
	if len(reply) != 24 {
		t.Fatalf("reply length = %d, want 24", len(reply))
	}
	if v := binary.BigEndian.Uint32(reply[0:4]); v != pingLegacyVersion {
		t.Errorf("legacy version = %#x, want %#x", v, pingLegacyVersion)
	}
	if string(reply[4:12]) != string(req[4:12]) {
		t.Errorf("timestamp not echoed verbatim: got % x, want % x", reply[4:12], req[4:12])
	}
	if v := binary.BigEndian.Uint32(reply[12:16]); v != 1 {
		t.Errorf("user count = %d, want 1", v)
	}
	if v := binary.BigEndian.Uint32(reply[16:20]); v != 100 {
		t.Errorf("max users = %d, want 100", v)
	}
	if v := binary.BigEndian.Uint32(reply[20:24]); v != 128000 {
		t.Errorf("max bandwidth = %d, want 128000", v)
	}
}

func TestPlainPingReply_ProtobufProbe(t *testing.T) {
	srv := newPingTestServer(t)

	const ts = uint64(0xAABBCCDD)
	probe := appendProtoVarintField([]byte{pingHeaderProtobuf}, 1, ts)
	probe = appendProtoVarintField(probe, 2, 1) // request_extended_information
	reply := srv.plainPingReply(probe)
	if reply == nil {
		t.Fatal("protobuf probe got no reply")
	}
	if reply[0] != pingHeaderProtobuf {
		t.Fatalf("header byte = %#x, want %#x", reply[0], pingHeaderProtobuf)
	}

	fields := map[uint64]uint64{}
	for i := 1; i < len(reply); {
		key, n := decodeProtoVarint(reply[i:])
		if n <= 0 {
			t.Fatalf("malformed key varint at offset %d", i)
		}
		i += n
		val, n := decodeProtoVarint(reply[i:])
		if n <= 0 {
			t.Fatalf("malformed value varint at offset %d", i)
		}
		i += n
		fields[key>>3] = val
	}
	want := map[uint64]uint64{
		1: ts,
		3: pingVersionV2,
		4: 1,
		5: 100,
		6: 128000,
	}
	for field, wv := range want {
		if got, ok := fields[field]; !ok {
			t.Errorf("field %d missing from reply", field)
		} else if got != wv {
			t.Errorf("field %d = %d, want %d", field, got, wv)
		}
	}
	if _, ok := fields[2]; ok {
		t.Error("reply must not re-send request_extended_information")
	}
}

func TestPlainPingReply_IgnoresNonProbes(t *testing.T) {
	srv := newPingTestServer(t)
	cases := []struct {
		name string
		data []byte
	}{
		{"legacy echo ping (no extension request)", []byte{0x20, 0x2A}},
		{"protobuf ping without extended request", appendProtoVarintField([]byte{pingHeaderProtobuf}, 1, 42)},
		{"protobuf ping with request cleared", []byte{pingHeaderProtobuf, 0x10, 0x00}},
		{"empty", nil},
		{"single header byte", []byte{pingHeaderProtobuf}},
		{"truncated varint", []byte{pingHeaderProtobuf, 0x08, 0x80}},
		{"random voice-like packet", []byte{0x80, 0x12, 0x34, 0x56, 0x78, 0x9A}},
	}
	for _, tc := range cases {
		if reply := srv.plainPingReply(tc.data); reply != nil {
			t.Errorf("%s: got reply % x, want nil", tc.name, reply)
		}
	}
}

func TestHandleUDP_RespondsToPlainPingOverSocket(t *testing.T) {
	srv := newPingTestServer(t)

	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen server udp: %v", err)
	}
	t.Cleanup(func() { _ = serverConn.Close() })
	probeConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen probe udp: %v", err)
	}
	t.Cleanup(func() { _ = probeConn.Close() })
	srv.udpConn = serverConn

	legacyReq := make([]byte, 12)
	binary.BigEndian.PutUint64(legacyReq[4:12], 0x0123456789ABCDEF)
	srv.HandleUDP(probeConn.LocalAddr(), legacyReq)

	buf := make([]byte, 64)
	if err := probeConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	n, _, err := probeConn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if n != 24 {
		t.Fatalf("legacy reply length = %d, want 24", n)
	}
	if string(buf[4:12]) != string(legacyReq[4:12]) {
		t.Errorf("timestamp not echoed: got % x, want % x", buf[4:12], legacyReq[4:12])
	}

	protoReq := appendProtoVarintField([]byte{pingHeaderProtobuf}, 1, 7)
	protoReq = appendProtoVarintField(protoReq, 2, 1)
	srv.HandleUDP(probeConn.LocalAddr(), protoReq)
	n, _, err = probeConn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read protobuf reply: %v", err)
	}
	if n < 2 || buf[0] != pingHeaderProtobuf {
		t.Fatalf("protobuf reply malformed: % x", buf[:n])
	}
}
