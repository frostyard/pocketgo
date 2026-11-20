package team

// ShouldCompact says whether pocketd compacts a standing session whose
// context is estimate tokens, at the end of a run (ADR-0025): once the
// estimate reaches compactAt, its profile's threshold, below window, its
// model's context window. A session with no threshold of its own (0), or
// one at or past the window, which the profile check refuses, is left to
// PiG's own threshold, the backstop of every session.
func ShouldCompact(estimate, compactAt, window int) bool {
	return compactAt > 0 && compactAt < window && estimate >= compactAt
}

// StandingTitle is the title of an agent's standing session: its name, as
// the owner knows it.
func StandingTitle(name string) string { return name }
