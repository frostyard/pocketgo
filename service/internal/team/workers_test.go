package team

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A message to a worker holds up to MaxWorkerMessage characters, counted
// as characters, not bytes (ADR-0021; Codex review of #29, round 4).
func TestCheckWorkerMessageAtItsLimit(t *testing.T) {
	at := strings.Repeat("é", MaxWorkerMessage)
	if err := CheckWorkerMessage(WorkerMessage{Message: at}); err != nil {
		t.Fatalf("%d characters: %v", MaxWorkerMessage, err)
	}
	if err := CheckWorkerMessage(WorkerMessage{Message: at + "é"}); err == nil {
		t.Fatalf("%d characters were accepted", MaxWorkerMessage+1)
	}
	if err := CheckWorkerMessage(WorkerMessage{Message: " \n\t"}); err == nil {
		t.Fatal("an empty message was accepted")
	}
}

// A worker's reply reaches its specialist whole up to MaxWorkerReply
// characters; a longer one is clipped there, in characters, and says so
// (ADR-0021; Codex review of #29, round 4).
func TestWorkerReplyTextClipsAtItsLimit(t *testing.T) {
	at := strings.Repeat("ü", MaxWorkerReply)
	if got := WorkerReplyText(4, at); got != "[T4 · worker] "+at {
		t.Fatalf("a reply at the limit was changed: %d characters", utf8.RuneCountInString(got))
	}
	got := WorkerReplyText(4, at+"üü")
	body := strings.TrimPrefix(got, "[T4 · worker] ")
	clipped, notice, ok := strings.Cut(body, "\n\n[pocketd clipped")
	if !ok || clipped != at || !strings.Contains(notice, "8000 characters") {
		t.Fatalf("a reply past the limit: %d characters kept, notice %q", utf8.RuneCountInString(clipped), notice)
	}
	if got := WorkerReplyText(4, "  "); !strings.Contains(got, "no reply") {
		t.Fatalf("an empty reply: %q", got)
	}
}
