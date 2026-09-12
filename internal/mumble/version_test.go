package mumble

import "testing"

// The advertised version gates client-side features: >=1.4.0 makes Mumble
// clients show the channel-listening UI (issue #19), while >=1.5.0 makes 1.5
// clients negotiate the protobuf UDP voice protocol, which this server cannot
// parse yet (issue #22). Guard the 1.4.x window so neither side breaks silently.
func TestServerVersionV1Within14xWindow(t *testing.T) {
	if ServerVersionV1 == 0 {
		t.Fatal("ServerVersionV1 is zero — clients read that as an unknown version and hide all version-gated UI")
	}
	major := ServerVersionV1 >> 16
	minor := (ServerVersionV1 >> 8) & 0xFF
	patch := ServerVersionV1 & 0xFF
	if major != 1 || minor < 4 {
		t.Fatalf("advertised version is %d.%d.%d, below 1.4.0 — clients hide the channel-listening UI for such servers (issue #19)", major, minor, patch)
	}
	if major > 1 || minor >= 5 {
		t.Fatalf("advertised version is %d.%d.%d, >= 1.5.0 — clients would negotiate protobuf UDP voice this server cannot parse (issue #22); keep the advertisement at 1.4.x until #22 lands", major, minor, patch)
	}
}
