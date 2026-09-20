package mumble

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/internal/edge"
	pkgmumble "github.com/dchote/go-mumble-server/pkg/mumble"
)

// udpPingHeader is the 1.5+ protobuf UDP ping header byte, shared with the
// edge package's probe encoder.
const udpPingHeader = byte(0x01)

func newPingTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newACLTestServer(t)
	srv.cfg.MaxBandwidth = 128000
	if _, ok := srv.users.Add(pkgmumble.User{Name: "probe-user"}); !ok {
		t.Fatal("add probe user")
	}
	return srv
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

	protoReq := edge.AppendProtoVarintField([]byte{udpPingHeader}, 1, 7)
	protoReq = edge.AppendProtoVarintField(protoReq, 2, 1)
	srv.HandleUDP(probeConn.LocalAddr(), protoReq)
	n, _, err = probeConn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read protobuf reply: %v", err)
	}
	if n < 2 || buf[0] != udpPingHeader {
		t.Fatalf("protobuf reply malformed: % x", buf[:n])
	}
}
