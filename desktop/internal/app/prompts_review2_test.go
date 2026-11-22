package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/proto"
)

// A prompt's row the keyboard chose with Enter puts the prompt in once,
// and the held Enter's repeats in the field it gives the keys back to send
// nothing; the next press sends (Codex review of #108, round 2).
func TestHoldingEnterOnAChosenRowNeverSends(t *testing.T) {
	x := promptSession(t)
	x.typeInto(t, "Message", "/")
	for range 10 {
		if x.tt.Focused("/status") {
			break
		}
		x.tt.Key(ui.Shift, ui.KeyTab)
		x.settle()
	}
	if !x.tt.Focused("/status") {
		t.Fatalf("Shift+Tab never reached a prompt's row: %q", x.tt.Texts())
	}
	x.tt.KeyDown(ui.KeyEnter)
	x.settle()
	if got := x.draftOf(1); !strings.HasPrefix(got, "Tell me where each of your open threads stands") {
		t.Fatalf("the row's Enter put in %q", got)
	}
	if !x.tt.Focused("Message") {
		t.Fatal("the field did not get the keys back")
	}
	for range 4 {
		x.tt.KeyRepeat(0, ui.KeyEnter)
		x.settle()
	}
	messages := proto.Path(proto.PathMessages, 1)
	if n := len(x.srv.RequestsTo(http.MethodPost, messages)); n != 0 {
		t.Fatalf("holding Enter on the row sent %d messages", n)
	}
	x.tt.KeyUp(ui.KeyEnter)
	x.settle()
	x.tt.Key(0, ui.KeyEnter)
	x.until(t, func() bool { return len(x.srv.RequestsTo(http.MethodPost, messages)) == 1 })
}
