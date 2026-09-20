package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/internal/cluster"
	ma "github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

type wireTestBackend struct {
	registry  *cluster.Registry
	ready     chan struct{}
	voice     chan cluster.VoiceUp
	transport cluster.VoiceTransport
}

func (b *wireTestBackend) EdgeConnected(_ cluster.EdgeInstanceRef, t cluster.VoiceTransport) {
	b.transport = t
}
func (b *wireTestBackend) AuthenticateRemote(_ context.Context, i cluster.EdgeInstanceRef, r cluster.AuthRequest) cluster.AuthResponse {
	ref := cluster.SessionRef{SessionID: 42, Generation: 10}
	if err := b.registry.Bind(ref, i.EdgeID); err != nil {
		return cluster.AuthResponse{ConnectionID: r.ConnectionID, Rejected: true, Reason: err.Error()}
	}
	return cluster.AuthResponse{ConnectionID: r.ConnectionID, Session: ref, Username: r.Username}
}
func (b *wireTestBackend) TransportReadyRemote(ctx context.Context, _ cluster.EdgeInstanceRef, e cluster.SessionEvent, s cluster.ControlSink) error {
	_ = s.WriteControl(cluster.ControlMessage{Type: protocol.MessageServerSync, Message: &messages.ServerSync{Session: e.Session.SessionID}})
	err := s.Flush(ctx)
	close(b.ready)
	return err
}
func (b *wireTestBackend) ControlRemote(context.Context, cluster.EdgeInstanceRef, cluster.ControlWire) error {
	return nil
}
func (b *wireTestBackend) VoiceRemote(_ context.Context, _ cluster.EdgeInstanceRef, v cluster.VoiceUp) error {
	b.voice <- v
	return nil
}
func (b *wireTestBackend) DisconnectRemote(cluster.EdgeInstanceRef, cluster.SessionEvent) {}
func (b *wireTestBackend) EdgeDisconnected(cluster.EdgeInstanceRef)                       {}

type wireTestDelivery struct {
	control      chan cluster.ControlWire
	voice        chan cluster.VoiceBatch
	disconnected chan struct{}
	once         sync.Once
}

func (d *wireTestDelivery) DeliverControl(v cluster.ControlWire)  { d.control <- v }
func (d *wireTestDelivery) FlushControl(cluster.SessionRef) error { return nil }
func (d *wireTestDelivery) DeliverVoice(v cluster.VoiceBatch)     { d.voice <- v }
func (d *wireTestDelivery) CloseSession(cluster.SessionRef)       {}
func (d *wireTestDelivery) CoreDisconnected(cluster.CoreEpoch) {
	d.once.Do(func() { close(d.disconnected) })
}

func TestRemoteCoreMTLSAuthControlAndVoice(t *testing.T) {
	serverTLS, clientTLS := wireTLS(t, "edge-a")
	registry := cluster.NewRegistry()
	backend := &wireTestBackend{registry: registry, ready: make(chan struct{}), voice: make(chan cluster.VoiceUp, 1)}
	server := cluster.NewRemoteEdgeServerWithConfig(registry, backend, cluster.RemoteEdgeServerConfig{ListenAddr: "127.0.0.1:0", TLSConfig: serverTLS, HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Start(ctx) }()
	t.Cleanup(func() { _ = server.Close() })
	var addr net.Addr
	deadline := time.Now().Add(2 * time.Second)
	for addr == nil && time.Now().Before(deadline) {
		addr = server.Addr()
		time.Sleep(time.Millisecond)
	}
	if addr == nil {
		t.Fatal("server did not listen")
	}
	client, err := NewRemoteCoreClientWithConfig(RemoteCoreClientConfig{Address: addr.String(), EdgeID: "edge-a", TLSConfig: clientTLS, ReconnectMin: 10 * time.Millisecond, ReconnectMax: 20 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	delivery := &wireTestDelivery{control: make(chan cluster.ControlWire, 2), voice: make(chan cluster.VoiceBatch, 2), disconnected: make(chan struct{})}
	client.SetDelivery(delivery)
	go func() { _ = client.Start(ctx) }()
	t.Cleanup(func() { _ = client.Close() })
	deadline = time.Now().Add(2 * time.Second)
	for !client.EdgeInstance().Valid() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !client.EdgeInstance().Valid() {
		t.Fatal("edge handshake did not complete")
	}
	if client.CoreEpoch() == "" {
		t.Fatal("missing CoreEpoch")
	}
	result, err := client.Authenticate(context.Background(), AuthForward{ConnID: 7, Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	ref := cluster.SessionRef{SessionID: result.SessionID, Generation: result.Generation}
	if ref.SessionID != 42 || ref.Generation != 10 {
		t.Fatalf("session=%+v", ref)
	}
	if err = client.TransportReady(context.Background(), 7, ref); err != nil {
		t.Fatal(err)
	}
	select {
	case <-backend.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("transport-ready flush timed out")
	}
	select {
	case got := <-delivery.control:
		if got.Session != ref || got.MessageType != uint16(protocol.MessageServerSync) {
			t.Fatalf("control=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing control down")
	}
	frame := ma.Frame{Codec: ma.CodecOpus, OpusData: []byte{1, 2, 3}}
	if err = client.VoiceUp(context.Background(), ref, frame); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-backend.voice:
		if got.Session != ref || len(got.Frame.OpusData) != 3 {
			t.Fatalf("voice up=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing voice up")
	}
	batch := cluster.VoiceBatch{Sender: ref, Frame: frame, Recipients: []cluster.VoiceRecipient{{Session: ref}}}
	if err = backend.transport.DeliverVoice(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-delivery.voice:
		if got.Sender != ref {
			t.Fatalf("voice down=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing voice down")
	}
}

func wireTLS(t *testing.T, edgeID string) (*tls.Config, *tls.Config) {
	t.Helper()
	now := time.Now()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, _ := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	issue := func(serial int64, cn string, dns []string, usage x509.ExtKeyUsage) tls.Certificate {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, DNSNames: dns, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}
	}
	serverCert := issue(2, "core.test", []string{"core.test"}, x509.ExtKeyUsageServerAuth)
	edgeCert := issue(3, edgeID, []string{edgeID}, x509.ExtKeyUsageClientAuth)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{edgeCert}, RootCAs: pool, ServerName: "core.test"}
}
