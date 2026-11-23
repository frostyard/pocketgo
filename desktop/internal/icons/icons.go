// Package icons holds the icons PocketGo draws that neither the desktop's
// theme nor frostui's set names: those of frostyard/pocket's sidebar,
// ported with it (ADR-0010). They follow Lucide's look (24 DIP grid, 2 DIP
// strokes, round ends).
package icons

const open = `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">`

func svg(body string) []byte { return []byte(open + body + `</svg>`) }

// Embedded returns the icons by name, for frostui.NewIcons.
func Embedded() map[string][]byte {
	return map[string][]byte{
		// A speech bubble: a session.
		"pocket-session": svg(`<path d="M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z"/>`),
		// A chip: a session's model (ADR-0065).
		"pocket-model": svg(`<rect x="4" y="4" width="16" height="16" rx="2"/><rect x="9" y="9" width="6" height="6" rx="1"/><path d="M15 2v2M15 20v2M2 15h2M2 9h2M20 15h2M20 9h2M9 2v2M9 20v2"/>`),
		// A robot face: a named agent.
		"pocket-agent": svg(`<rect x="4" y="8" width="16" height="12" rx="3"/><path d="M12 8V4M9 13v1M15 13v1M10 17h4"/><circle cx="12" cy="3" r="1"/>`),
		// A raised hand: waiting for you.
		"pocket-waiting": svg(`<path d="M18 11V6a2 2 0 0 0-4 0v5M14 10V4a2 2 0 0 0-4 0v6M10 10.5V6a2 2 0 0 0-4 0v8"/><path d="M18 8a2 2 0 1 1 4 0v6a8 8 0 0 1-8 8h-2c-2.8 0-4.5-.9-6-2.4l-3.6-3.6a2 2 0 0 1 2.8-2.8L7 15"/>`),
		// A terminal prompt: the Terminal place and its shells.
		"pocket-terminal": svg(`<path d="M4 17l6-6-6-6M12 19h8"/>`),
		// A bell: notifications (ADR-0049).
		"pocket-bell": svg(`<path d="M10.3 21a2 2 0 0 0 3.4 0"/><path d="M3.3 15.3A1 1 0 0 0 4 17h16a1 1 0 0 0 .7-1.7C19.4 14 18 12.5 18 8A6 6 0 0 0 6 8c0 4.5-1.4 6-2.7 7.3"/>`),
		// A plus in a bubble: a new session.
		"pocket-new": svg(`<path d="M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z"/><path d="M12 8v8M8 12h8"/>`),
		// A gauge: what the sessions used (ADR-0052).
		"pocket-usage": svg(`<path d="m12 14 4-4"/><path d="M3.34 19a10 10 0 1 1 17.32 0"/>`),
		// A slash in a square: the owner's saved prompts (ADR-0051).
		"pocket-prompt": svg(`<rect x="3" y="3" width="18" height="18" rx="3"/><path d="M15 7l-6 10"/>`),
		// A page with a plus over a minus: a session's changes (ADR-0053).
		"pocket-changes": svg(`<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7z"/><path d="M9 10h6M12 7v6M9 17h6"/>`),
	}
}
