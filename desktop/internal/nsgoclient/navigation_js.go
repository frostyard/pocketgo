package nsgoclient

import "syscall/js"

// NSGo owns history in a top-level browser window. Its URL change callback
// restores the same visit for a toolbar press and a browser Back gesture.
func moveBrowser(delta int) bool {
	w := js.Global().Get("window")
	if !w.Truthy() || !w.Equal(w.Get("top")) {
		return false
	}
	w.Get("history").Call("go", delta)
	return true
}
