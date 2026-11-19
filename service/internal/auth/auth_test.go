package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/store"
)

func TestDeviceTokens(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	device, token, err := CreateDevice(ctx, s, " laptop ")
	if err != nil || device.Name != "laptop" || !strings.HasPrefix(token, tokenPrefix) {
		t.Fatalf("CreateDevice %+v %q %v", device, token, err)
	}
	got, err := Authenticate(ctx, s, "Bearer "+token)
	if err != nil || got.ID != device.ID {
		t.Fatalf("Authenticate %+v %v", got, err)
	}
	for _, header := range []string{"", token, "Bearer ", "Bearer " + NewToken(), "Bearer pgd_short", "Basic " + token} {
		if _, err := Authenticate(ctx, s, header); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("header %q: %v", header, err)
		}
	}
	if err := s.RevokeDevice(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := Authenticate(ctx, s, "Bearer "+token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked token: %v", err)
	}
	if _, _, err := CreateDevice(ctx, s, "  "); err == nil {
		t.Fatal("empty name accepted")
	}
}
