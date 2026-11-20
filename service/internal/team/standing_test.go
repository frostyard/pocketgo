package team

import "testing"

// A standing session compacts once its context reaches its profile's
// threshold, below its model's window, and never on PiG's behalf.
func TestShouldCompact(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		estimate, compactAt, window int
		want                        bool
	}{
		{"below the threshold", 299_999, 300_000, 1_000_000, false},
		{"at the threshold", 300_000, 300_000, 1_000_000, true},
		{"past the threshold", 420_000, 300_000, 1_000_000, true},
		{"no threshold of its own", 900_000, 0, 1_000_000, false},
		{"a threshold at the window", 300_000, 300_000, 300_000, false},
		{"a threshold past the window", 400_000, 300_000, 200_000, false},
		{"an unknown window", 400_000, 300_000, 0, false},
	} {
		if got := ShouldCompact(tc.estimate, tc.compactAt, tc.window); got != tc.want {
			t.Errorf("%s: ShouldCompact(%d, %d, %d) = %v, want %v", tc.name, tc.estimate, tc.compactAt, tc.window, got, tc.want)
		}
	}
}
