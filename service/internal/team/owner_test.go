package team

import "testing"

func TestOnlyADevicesKeyIsTheOwners(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"u:dev:1", true},
		{"", false},
		{"u", false},
		{"U:dev:1", false},
		{"team:1:2:c1:report", false},
		{SendKey(7, "team:7:3:c1") + ":report", false},
		{"plan:abc:done", false},
	} {
		if got := IsOwnerKey(tc.key); got != tc.want {
			t.Errorf("IsOwnerKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}
