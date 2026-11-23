package nsgoclient

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestRemoteOutputKeepsDisplayAndLinksLocalToTheSession(t *testing.T) {
	const visible = "界📝\x1b[32mhello\x1b[0m\x1b]8;;https://example.org\x07site\x1b]8;;\x07"
	r := newRemoteOutput(iotest.OneByteReader(strings.NewReader("\x1b]7;file://localhost/workspace\x07" + visible)))
	b, err := io.ReadAll(r)
	if err != nil || strings.Contains(string(b), "/workspace") || !strings.HasSuffix(string(b), visible) {
		t.Fatalf("filtered terminal output %q, %v", b, err)
	}
}
