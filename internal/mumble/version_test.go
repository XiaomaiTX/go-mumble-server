package mumble

import "testing"

// The advertised version gates client-side features. The server now declares
// 1.5.0 because the Protobuf UDP voice path is enabled by default.
func TestServerVersionV1Is150(t *testing.T) {
	if ServerVersionV1 == 0 {
		t.Fatal("ServerVersionV1 is zero — clients read that as an unknown version and hide all version-gated UI")
	}
	major := ServerVersionV1 >> 16
	minor := (ServerVersionV1 >> 8) & 0xFF
	patch := ServerVersionV1 & 0xFF
	if major != 1 || minor != 5 || patch != 0 {
		t.Fatalf("advertised version is %d.%d.%d, want 1.5.0", major, minor, patch)
	}
}
