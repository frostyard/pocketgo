//go:build !unix

package main

import "errors"

// lockData has no implementation off Unix: pocketd serve runs on Linux and
// macOS (CROSS_service).
func lockData(string) (func(), error) {
	return nil, errors.New("pocketd serve needs a Unix host")
}
