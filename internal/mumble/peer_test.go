package mumble

import (
	"reflect"
	"testing"

	"github.com/dchote/go-mumble-server/internal/connection"
)

// The transitional Peer adapter must stay minimal: logical session metadata
// and control send only. Growing it back into a fat session object would undo
// the Core/Edge boundary this phase establishes.
func TestPeerSurfaceIsFrozen(t *testing.T) {
	typ := reflect.TypeOf((*Peer)(nil)).Elem()
	got := make(map[string]bool, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		got[typ.Method(i).Name] = true
	}
	want := map[string]bool{"SessionID": true, "SessionGeneration": true, "WriteMessage": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Peer method set = %v, want exactly %v", got, want)
	}
	// The local connection remains a valid control peer.
	var _ Peer = (*connection.Conn)(nil)
}
