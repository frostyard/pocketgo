package pig

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestLineReaderStopsWhenTheContextEnds(t *testing.T) {
	in, out := io.Pipe()
	defer func() { _ = out.Close() }()
	r := newLineReader(in)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.read(ctx)
		done <- err
	}()
	cancel() // the browser callback signed in; the terminal stays silent
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel: %v", err)
	}
	// A later prompt still gets the next line.
	go func() { _, _ = io.WriteString(out, "  the-code \n") }()
	line, err := r.read(context.Background())
	if err != nil || line != "the-code" {
		t.Fatalf("next line %q %v", line, err)
	}
}

func TestLineReaderReportsEOF(t *testing.T) {
	in, out := io.Pipe()
	r := newLineReader(in)
	_ = out.Close()
	if _, err := r.read(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("read at EOF: %v", err)
	}
}
