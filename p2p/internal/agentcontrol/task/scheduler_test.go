package task

import "testing"

func TestClaimPolicyUsesOneGlobalLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limit   int
		running int
		want    bool
	}{
		{name: "one global slot is available", limit: 1, running: 0, want: true},
		{name: "capacity remains below global limit", limit: 4, running: 3, want: true},
		{name: "global limit is saturated", limit: 4, running: 4, want: false},
		{name: "stale over-count cannot claim", limit: 4, running: 5, want: false},
		{name: "invalid global limit cannot claim", limit: 0, running: 0, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (ClaimPolicy{MaxConcurrent: tc.limit}).CanClaim(tc.running); got != tc.want {
				t.Fatalf("CanClaim(limit=%d, running=%d)=%v, want %v", tc.limit, tc.running, got, tc.want)
			}
		})
	}
}
