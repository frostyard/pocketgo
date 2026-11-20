package terminal

import "unicode/utf8"

// history is the last Keep bytes of a shell's output, in a ring: a write
// copies only its own bytes, never what is kept already, so a shell that
// prints in small pieces holds the Manager's lock for those alone (Codex
// review of #41, round 4). Once full, what is kept begins at a
// character's first byte.
type history struct {
	buf   []byte
	start int // index in buf of the oldest byte kept
	size  int // bytes kept
	// copied counts the bytes writes copied in, for tests.
	copied int
}

// write keeps p, and drops what is older than the last Keep bytes.
func (h *history) write(p []byte) {
	if len(p) == 0 {
		return
	}
	if h.buf == nil {
		h.buf = make([]byte, Keep)
	}
	dropped := false
	if len(p) >= Keep {
		p = p[len(p)-Keep:]
		h.start, h.size, dropped = 0, 0, true
	}
	at := (h.start + h.size) % Keep
	n := copy(h.buf[at:], p)
	copy(h.buf, p[n:])
	h.copied += len(p)
	h.size += len(p)
	if over := h.size - Keep; over > 0 {
		h.start = (h.start + over) % Keep
		h.size = Keep
		dropped = true
	}
	for i := 0; dropped && i < utf8.UTFMax-1 && h.size > 0 && !utf8.RuneStart(h.buf[h.start]); i++ {
		h.start = (h.start + 1) % Keep
		h.size--
	}
}

// from copies the bytes kept from the i-th on.
func (h *history) from(i int) []byte {
	if h.size-i <= 0 {
		return []byte{}
	}
	out := make([]byte, 0, h.size-i)
	first := (h.start + i) % Keep
	end := first + (h.size - i)
	if end <= Keep {
		return append(out, h.buf[first:end]...)
	}
	return append(append(out, h.buf[first:]...), h.buf[:end-Keep]...)
}
