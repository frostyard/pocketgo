package files

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// TestCodesRoundTrip keeps sentinel identity and messages across the helper.
func TestCodesRoundTrip(t *testing.T) {
	for _, base := range []error{ErrNotFound, ErrOutside, ErrHidden, ErrNotFolder, ErrNotFile, ErrBadPath} {
		t.Run(base.Error(), func(t *testing.T) {
			err, ok := FromCode(Code(fmt.Errorf("read: %w", base)), "message")
			if !ok || !errors.Is(err, base) || err.Error() != "message" {
				t.Fatalf("%v %v", err, ok)
			}
		})
	}
	if Code(io.EOF) != "" {
		t.Fatal("EOF has a code")
	}
	if err, ok := FromCode("x", "y"); ok || err != nil {
		t.Fatalf("unknown: %v %v", err, ok)
	}
}
