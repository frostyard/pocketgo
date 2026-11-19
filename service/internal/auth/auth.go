// Package auth signs the owner's devices in with bearer tokens. A token is
// shown once when it is created; the store keeps only its SHA-256 hash.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/frostyard/pocketgo/service/internal/store"
)

// tokenPrefix marks a pocketgo device token, so a leaked one is recognizable.
const tokenPrefix = "pgd_"

// ErrUnauthorized is returned for a missing, malformed, unknown or revoked
// token.
var ErrUnauthorized = errors.New("unauthorized")

// NewToken returns a new random device token.
func NewToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return tokenPrefix + hex.EncodeToString(b[:])
}

// Hash is the stored form of a token.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateDevice creates a device and returns its token, which is never
// stored or shown again.
func CreateDevice(ctx context.Context, s *store.Store, name string) (store.Device, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Device{}, "", errors.New("a device needs a name")
	}
	token := NewToken()
	device, err := s.CreateDevice(ctx, name, Hash(token))
	return device, token, err
}

// Authenticate returns the device of an Authorization header value
// ("Bearer <token>").
func Authenticate(ctx context.Context, s *store.Store, header string) (store.Device, error) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || !strings.HasPrefix(token, tokenPrefix) || len(token) != len(tokenPrefix)+64 {
		return store.Device{}, ErrUnauthorized
	}
	device, err := s.DeviceByTokenHash(ctx, Hash(token))
	if errors.Is(err, store.ErrNotFound) {
		return store.Device{}, ErrUnauthorized
	}
	return device, err
}
