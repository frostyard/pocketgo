//go:build windows

package config

import "testing"

// onTheDesktop changes nothing on Windows, whose lock follows the user's
// profile alone (signin_lock_windows.go).
func onTheDesktop(*testing.T) {}
