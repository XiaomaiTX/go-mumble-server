package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dchote/go-mumble-server/internal/cluster"
	"github.com/dchote/go-mumble-server/internal/config"
	"github.com/dchote/go-mumble-server/internal/edge"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol"
	"github.com/dchote/go-mumble-server/pkg/mumble/protocol/messages"
)

func TestCoreAndRemoteEdgeMumbleLogin(t *testing.T) {
	identity := distributedTestIdentity(t, "edge-a")
	coreCfg, _ := config.Load("")
	coreCfg.Mode = config.ModeCore
	coreCfg.Host = "127.0.0.1"
	coreCfg.MumblePort = freePort(t)
	coreCfg.RESTPort = freePort(t)
	coreCfg.DatabasePath = filepath.Join(t.TempDir(), "core.sqlite")
	coreCfg.EdgeListenAddr = net.JoinHostPort("127.0.0.1", itoa(freePort(t)))
	coreCfg.EdgeTLSCertPath = identity.serverCert
	coreCfg.EdgeTLSKeyPath = identity.serverKey
	coreCfg.EdgeClientCAPath = identity.ca
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coreApp, err := Build(ctx, coreCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = coreApp.Run(ctx) }()
	t.Cleanup(func() { _ = coreApp.Shutdown(context.Background()) })
	edgeCfg, _ := config.Load("")
	edgeCfg.Mode = config.ModeEdge
	edgeCfg.Host = "127.0.0.1"
	edgeCfg.MumblePort = freePort(t)
	edgeCfg.CoreAddress = coreCfg.EdgeListenAddr
	edgeCfg.EdgeID = "edge-a"
	edgeCfg.CoreCACertPath = identity.ca
	edgeCfg.EdgeClientCertPath = identity.edgeCert
	edgeCfg.EdgeClientKeyPath = identity.edgeKey
	edgeCfg.CoreServerName = "core.test"
	edgeCfg.EdgeReconnectMin = 10 * time.Millisecond
	edgeCfg.EdgeReconnectMax = 20 * time.Millisecond
	edgeApp, err := Build(ctx, edgeCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = edgeApp.Run(ctx) }()
	t.Cleanup(func() { _ = edgeApp.Shutdown(context.Background()) })
	remote := edgeApp.RemoteCore.(*edge.RemoteCoreClient)
	deadline := time.Now().Add(4 * time.Second)
	for !remote.EdgeInstance().Valid() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !remote.EdgeInstance().Valid() {
		t.Fatal("edge did not register with Core")
	}
	client, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(edgeCfg.MumblePort)), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	kind, _, err := protocol.ReadPacket(client)
	if err != nil || kind != protocol.MessageVersion {
		t.Fatalf("banner kind=%d err=%v", kind, err)
	}
	if err = protocol.WriteMessage(client, protocol.MessageVersion, &messages.Version{VersionV2: edge.ProtocolVersionV2, Release: "integration"}); err != nil {
		t.Fatal(err)
	}
	if err = protocol.WriteMessage(client, protocol.MessageAuthenticate, &messages.Authenticate{Username: "remote-user", Opus: true}); err != nil {
		t.Fatal(err)
	}
	var session uint32
	configured := false
	channels := 0
	users := 0
	for session == 0 || !configured {
		kind, payload, readErr := protocol.ReadPacket(client)
		if readErr != nil {
			t.Fatal(readErr)
		}
		switch kind {
		case protocol.MessageReject:
			var reject messages.Reject
			_ = reject.Unmarshal(payload)
			t.Fatalf("rejected: %s", reject.Reason)
		case protocol.MessageChannelState:
			channels++
		case protocol.MessageUserState:
			users++
		case protocol.MessageServerSync:
			var sync messages.ServerSync
			if err = sync.Unmarshal(payload); err != nil {
				t.Fatal(err)
			}
			session = sync.Session
		case protocol.MessageServerConfig:
			configured = true
		}
	}
	if channels == 0 || users == 0 {
		t.Fatalf("incomplete remote sync: channels=%d users=%d", channels, users)
	}
	if _, ok := coreApp.Server.Mumble().UserManager().Snapshot(session); !ok {
		t.Fatal("remote session missing from authoritative Core")
	}
	deadline = time.Now().Add(time.Second)
	var ref cluster.SessionRef
	var ok bool
	for time.Now().Before(deadline) {
		ref, ok = coreApp.Server.Mumble().Registry().Ref(session)
		if ok && coreApp.Server.Mumble().Registry().Active(ref) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !ok || !coreApp.Server.Mumble().Registry().Active(ref) {
		t.Fatal("remote session was not committed Active")
	}
}

func itoa(v int) string { return new(big.Int).SetInt64(int64(v)).String() }

type distributedIdentity struct{ ca, serverCert, serverKey, edgeCert, edgeKey string }

func distributedTestIdentity(t *testing.T, edgeID string) distributedIdentity {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "distributed-test-ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, _ := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	issue := func(serial int64, cn string, dns []string, usage x509.ExtKeyUsage) ([]byte, []byte) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, DNSNames: dns, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, caT, &key.PublicKey, caKey)
		keyDER, _ := x509.MarshalECPrivateKey(key)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}
	serverCert, serverKey := issue(2, "core.test", []string{"core.test"}, x509.ExtKeyUsageServerAuth)
	edgeCert, edgeKey := issue(3, edgeID, []string{edgeID}, x509.ExtKeyUsageClientAuth)
	id := distributedIdentity{}
	files := map[*string]struct {
		name string
		data []byte
	}{&id.ca: {"ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})}, &id.serverCert: {"server.crt", serverCert}, &id.serverKey: {"server.key", serverKey}, &id.edgeCert: {"edge.crt", edgeCert}, &id.edgeKey: {"edge.key", edgeKey}}
	for target, f := range files {
		*target = filepath.Join(dir, f.name)
		if err := os.WriteFile(*target, f.data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return id
}
