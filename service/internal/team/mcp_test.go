package team

import "testing"

// A profile's MCP tools go to the sessions the owner talks in and to a
// thread's specialist that builds; never to a review, a worker, a consult,
// a gather's agent or a plan's lead (ADR-0063).
func TestWhichSessionsTakeTheirProfilesMCPServers(t *testing.T) {
	for name, tc := range map[string]struct {
		role SessionRole
		want bool
	}{
		"the owner's session":   {SessionRole{}, true},
		"a thread's specialist": {SessionRole{Thread: true}, true},
		"a cross-review":        {SessionRole{Thread: true, Review: true}, false},
		"a thread's worker":     {SessionRole{Thread: true, Worker: true}, false},
		"a worker alone":        {SessionRole{Worker: true}, false},
		"a consult":             {SessionRole{Consult: true}, false},
		"a gather's agent":      {SessionRole{Gather: true}, false},
		"a plan's lead":         {SessionRole{Thread: true, Plan: true}, false},
		// An agent's work session builds, as the owner's session of its
		// profile does (ADR-0073).
		"a work session": {SessionRole{Work: true}, true},
	} {
		if got := MayUseMCP(tc.role); got != tc.want {
			t.Errorf("%s: MayUseMCP = %v, want %v", name, got, tc.want)
		}
	}
}
