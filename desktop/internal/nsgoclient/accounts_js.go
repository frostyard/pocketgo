//go:build js && wasm

package nsgoclient

// The window keeps the device token in the store until reload. GitHub's
// desktop OAuth client requires a keyring and an exclusive sign-in lock.
func (c *client) platformAccounts() {}
