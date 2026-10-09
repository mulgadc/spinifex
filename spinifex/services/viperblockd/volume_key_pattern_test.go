package viperblockd

import "testing"

//test:in-package — volumeLeaseKeyPattern is unexported, and this pins the
//key rule it enforces before dirty-marker vocabulary (which shares it) moves
//to contracts/viperblockd/legacy/v1.

// TestVolumeLeaseKeyPattern_PinsValidAndInvalidKeys characterizes which
// volume names are accepted as a JetStream KV key, for both the lease and the
// dirty-marker buckets. A name carrying "." or ">" would address somebody
// else's key, which is what the pattern exists to refuse.
func TestVolumeLeaseKeyPattern_PinsValidAndInvalidKeys(t *testing.T) {
	valid := []string{"vol-1", "VOL_1", "abcXYZ09", "a", "A-B_c9"}
	for _, key := range valid {
		if !volumeLeaseKeyPattern.MatchString(key) {
			t.Errorf("expected %q to be accepted as a volume key", key)
		}
	}

	invalid := []string{"", "vol.1", "vol/1", "vol:1", "vol 1", "vol>1", "ebs.*", "day's"}
	for _, key := range invalid {
		if volumeLeaseKeyPattern.MatchString(key) {
			t.Errorf("expected %q to be rejected as a volume key", key)
		}
	}
}
