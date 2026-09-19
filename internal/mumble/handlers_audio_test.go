package mumble

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/internal/connection"
	"github.com/dchote/go-mumble-server/pkg/mumble"
	ma "github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/crypto"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/wire"
)

func audioClient(t *testing.T, s *Server, u *mumble.User, mode ma.WireMode) (*connection.Conn, net.Conn, *crypto.CryptState) {
	t.Helper()
	stored, ok := s.users.Add(*u)
	if !ok {
		t.Fatal("新增用户失败")
	}
	*u = stored
	serverCrypt := crypto.NewCryptState(crypto.ModeLegacy)
	clientCrypt := crypto.NewCryptState(crypto.ModeLegacy)
	key := bytes.Repeat([]byte{byte(u.SessionID)}, 16)
	enc := bytes.Repeat([]byte{1}, 16)
	dec := bytes.Repeat([]byte{2}, 16)
	if e := serverCrypt.SetKey(key, enc, dec); e != nil {
		t.Fatal(e)
	}
	if e := clientCrypt.SetKey(key, dec, enc); e != nil {
		t.Fatal(e)
	}
	peer, local := net.Pipe()
	c := connection.New(local, serverCrypt, nil)
	c.SetSessionID(u.SessionID)
	version := uint64(1<<48 | 4<<32)
	if mode == ma.WireProtobuf {
		version = 1<<48 | 5<<32
	}
	c.SetClientVersion(version)
	c.NegotiateAudioWireMode(1<<48 | 5<<32)
	c.SetActive()
	s.RegisterConn(u.SessionID, c)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Run(ctx, protocol.HandlerTable{}) }()
	t.Cleanup(func() { cancel(); _ = peer.Close() })
	return c, peer, clientCrypt
}
func udpSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	c, e := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func encryptAudio(t *testing.T, c *crypto.CryptState, b []byte) []byte {
	t.Helper()
	out := make([]byte, len(b)+c.Overhead())
	if e := c.Encrypt(out, b); e != nil {
		t.Fatal(e)
	}
	return out
}
func readUDPAudio(t *testing.T, c *net.UDPConn, crypt *crypto.CryptState) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _, e := c.ReadFrom(buf)
	if e != nil {
		t.Fatal(e)
	}
	out := make([]byte, n)
	if e = crypt.Decrypt(out, buf[:n]); e != nil {
		t.Fatal(e)
	}
	return out[:n-crypt.Overhead()]
}
func clientAudio(mode ma.WireMode, target byte) []byte {
	if mode == ma.WireLegacy {
		return []byte{0x80 | target, 7, 0xa0, 0x02, 0xab, 0xcd}
	}
	// 包含伪造 session/音量；context 在 target 之前，最后的 target 才生效。
	return []byte{0, 0x10, 3, 0x08, target, 0x18, 0xe7, 7, 0x20, 7, 0x2a, 2, 0xab, 0xcd, 0x3d, 0, 0, 0x20, 0x40, 0x80, 1, 1}
}
func TestAudioTransportMatrix(t *testing.T) {
	for _, in := range []ma.WireMode{ma.WireLegacy, ma.WireProtobuf} {
		for _, out := range []ma.WireMode{ma.WireLegacy, ma.WireProtobuf} {
			for _, udpIn := range []bool{false, true} {
				for _, udpOut := range []bool{false, true} {
					t.Run(fmt.Sprintf("%d_%d_udpIn%t_udpOut%t", in, out, udpIn, udpOut), func(t *testing.T) {
						s := newVoiceTargetTestServer(t)
						s.udpConn = udpSocket(t)
						a := &mumble.User{Name: "a"}
						ac, _, aCrypt := audioClient(t, s, a, in)
						b := &mumble.User{Name: "b"}
						_, bTCP, bCrypt := audioClient(t, s, b, out)
						bUDP := udpSocket(t)
						if udpOut {
							s.addrBySession.Store(b.SessionID, bUDP.LocalAddr())
						}
						input := clientAudio(in, 0)
						if udpIn {
							s.HandleUDP(udpSocket(t).LocalAddr(), encryptAudio(t, aCrypt, input))
						} else {
							if e := s.handleUDPTunnel(protocol.MessageUDPTunnel, input, ac); e != nil {
								t.Fatal(e)
							}
						}
						var got []byte
						if udpOut {
							got = readUDPAudio(t, bUDP, bCrypt)
						} else {
							kind, p := readMessage(t, bTCP)
							if kind != protocol.MessageUDPTunnel {
								t.Fatal(kind)
							}
							got = p
						}
						want, e := ma.EncodeServerPacket(out, ma.Delivery{Frame: ma.Frame{Codec: ma.CodecOpus, SenderSession: a.SessionID, FrameNumber: 7, OpusData: []byte{0xab, 0xcd}, IsTerminator: true}, Context: ma.ContextNormal, VolumeAdjustment: 1})
						if e != nil || !bytes.Equal(got, want) {
							t.Fatalf("身份/context/音量或转换错误 got=%x want=%x err=%v", got, want, e)
						}
					})
				}
			}
		}
	}
}
func TestAudioMixedCryptoAndRebinding(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	s.udpConn = udpSocket(t)
	a := &mumble.User{Name: "a"}
	ac, _, aCrypt := audioClient(t, s, a, ma.WireProtobuf)
	b := &mumble.User{Name: "b"}
	_, bTCP, _ := audioClient(t, s, b, ma.WireProtobuf)
	addr := udpSocket(t).LocalAddr()
	s.addrBySession.Store(b.SessionID, addr)
	s.channelCryptoMu.Lock()
	s.channelCrypto[b.ChannelID] = "mixed"
	s.channelCryptoMu.Unlock()
	for i := 0; i < 2; i++ {
		newAddr := udpSocket(t).LocalAddr()
		s.HandleUDP(newAddr, encryptAudio(t, aCrypt, clientAudio(ma.WireProtobuf, 0)))
		kind, p := readMessage(t, bTCP)
		if kind != protocol.MessageUDPTunnel || p[0] != 0 {
			t.Fatalf("回退改变 wire mode: %x", p)
		}
		mapped, _ := s.addrBySession.Load(a.SessionID)
		if mapped.(net.Addr).String() != newAddr.String() {
			t.Fatal("NAT 重绑定未更新")
		}
	}
	if e := s.handleUDPTunnel(protocol.MessageUDPTunnel, clientAudio(ma.WireLegacy, 0), ac); e == nil {
		t.Fatal("现代连接通过内容猜测接受 Legacy")
	}
}
func TestAuthenticatedPings(t *testing.T) {
	for _, mode := range []ma.WireMode{ma.WireLegacy, ma.WireProtobuf} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			s := newVoiceTargetTestServer(t)
			s.udpConn = udpSocket(t)
			u := &mumble.User{Name: "ping"}
			_, _, crypt := audioClient(t, s, u, mode)
			peer := udpSocket(t)
			ping := []byte{0x20, 42}
			if mode == ma.WireProtobuf {
				ping = []byte{1, 8, 42}
			}
			s.HandleUDP(peer.LocalAddr(), encryptAudio(t, crypt, ping))
			got := readUDPAudio(t, peer, crypt)
			if !bytes.Equal(got, ping) {
				t.Fatalf("ping %x", got)
			}
		})
	}
}
func TestAudioPositionAndListenerVolume(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	ch := s.chans.Create(s.chans.RootID(), "source", "", 0, false, 0)
	a := &mumble.User{Name: "a", ChannelID: ch.ID, PluginContext: []byte("game")}
	_, _, _ = audioClient(t, s, a, ma.WireProtobuf)
	b := &mumble.User{Name: "b", PluginContext: []byte("game")}
	_, bTCP, _ := audioClient(t, s, b, ma.WireProtobuf)
	s.listeners.Add(b.SessionID, ch.ID)
	s.listeners.SetVolume(b.SessionID, ch.ID, .5)
	frame := ma.Frame{Codec: ma.CodecOpus, OpusData: []byte{1}, HasPosition: true, Position: [3]float32{1, 2, 3}}
	for _, same := range []bool{true, false} {
		if !same {
			s.users.UpdateUser(b.SessionID, func(u *mumble.User) { u.PluginContext = []byte("other") })
		}
		if e := s.router.Route(a.SessionID, frame); e != nil {
			t.Fatal(e)
		}
		_, got := readMessage(t, bTCP)
		expected := frame
		expected.SenderSession = a.SessionID
		expected.HasPosition = same
		want, e := ma.EncodeServerPacket(ma.WireProtobuf, ma.Delivery{Frame: expected, Context: ma.ContextListen, VolumeAdjustment: .5})
		if e != nil || !bytes.Equal(got, want) {
			t.Fatalf("位置或音量错误 got=%x want=%x", got, want)
		}
	}
}
func TestVoiceTargetDeliveryMetadata(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	ch := s.chans.Create(s.chans.RootID(), "target", "", 0, false, 0)
	a := &mumble.User{Name: "a"}
	ac, _, _ := audioClient(t, s, a, ma.WireProtobuf)
	b := &mumble.User{Name: "b", ChannelID: ch.ID}
	_, _, _ = audioClient(t, s, b, ma.WireProtobuf)
	listener := &mumble.User{Name: "listener"}
	_, _, _ = audioClient(t, s, listener, ma.WireProtobuf)
	s.listeners.Add(listener.SessionID, ch.ID)
	s.listeners.SetVolume(listener.SessionID, ch.ID, 2.5)
	setVoiceTarget(t, s, ac, messages.VoiceTargetTarget{Session: []uint32{b.SessionID, listener.SessionID}, ChannelID: ch.ID, HasChannelID: true})
	got := s.resolveVoiceTarget(a.SessionID, 1)
	if len(got) != 2 {
		t.Fatal(got)
	}
	for _, r := range got {
		if r.session == b.SessionID && r.delivery.Context != ma.ContextShout {
			t.Fatal("SHOUT 应优先于 WHISPER")
		}
		if r.session == listener.SessionID && (r.delivery.Context != ma.ContextWhisper || r.delivery.VolumeAdjustment != 2.5) {
			t.Fatal("上下文与音量独立合并失败")
		}
	}
}
func TestAdvertisedAudioCapability(t *testing.T) {
	s := newVoiceTargetTestServer(t)
	if s.ProtocolVersionV1() != 1<<16|5<<8 || s.ProtocolVersionV2() != 1<<48|5<<32 {
		t.Fatal("未默认宣告 1.5")
	}
	c := connection.New(nil, nil, nil)
	v := messages.Version{VersionV1: 1<<16 | 5<<8, VersionV2: 1<<48 | 5<<32}
	payload, _ := v.Marshal()
	if e := s.handleVersion(protocol.MessageVersion, payload, c); e != nil {
		t.Fatal(e)
	}
	if c.AudioWireMode() != ma.WireProtobuf {
		t.Fatal("1.5 客户端未协商 Protobuf")
	}
}

// 单独检查服务端包中不存在 target，防止预期生成器掩盖方向错误。
func TestServerAudioDirection(t *testing.T) {
	b, e := ma.EncodeServerPacket(ma.WireProtobuf, ma.Delivery{Frame: ma.Frame{Codec: ma.CodecOpus, Target: 31, SenderSession: 42, OpusData: []byte{1}}})
	if e != nil {
		t.Fatal(e)
	}
	for b = b[1:]; len(b) > 0; {
		field, typ, n, e := wire.ReadTag(b)
		if e != nil {
			t.Fatal(e)
		}
		if field == 1 {
			t.Fatal("出站泄漏 target")
		}
		b = b[n:]
		n, e = wire.SkipField(b, typ)
		if e != nil {
			t.Fatal(e)
		}
		b = b[n:]
	}
}
