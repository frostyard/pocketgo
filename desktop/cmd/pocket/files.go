package main

import (
	"context"

	"github.com/egoist/mygo"
)

// pickFiles is the native chooser of the files to attach to a message
// (ADR-0029), as frostyard/pocket's. Dialog.Open marshals onto the native
// UI thread; this function runs on a worker.
func pickFiles(context.Context) ([]string, error) {
	return mygo.Dialog.Open(mygo.OpenDialogOptions{Title: "Attach files", Multiple: true})
}
