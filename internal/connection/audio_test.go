package connection

import (
	ma "github.com/dchote/go-mumble-server/pkg/mumble/audio"
	"testing"
)

func TestAudioNegotiation(t *testing.T) {
	old := uint64(1<<48 | 4<<32)
	modern := uint64(1<<48 | 5<<32)
	for _, tc := range []struct {
		client, server uint64
		want           ma.WireMode
	}{{0, modern, ma.WireLegacy}, {old, modern, ma.WireLegacy}, {modern, old, ma.WireLegacy}, {modern, modern, ma.WireProtobuf}} {
		c := New(nil, nil, nil)
		c.SetClientVersion(tc.client)
		c.NegotiateAudioWireMode(tc.server)
		if c.AudioWireMode() != tc.want {
			t.Fatal(tc)
		}
		c.SetActive()
		c.SetClientVersion(old)
		c.NegotiateAudioWireMode(old)
		if c.AudioWireMode() != tc.want {
			t.Fatal("认证后模式改变")
		}
	}
}
