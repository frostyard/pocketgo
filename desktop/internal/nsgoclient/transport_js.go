//go:build js && wasm

package nsgoclient

import (
	"net/http"
	"syscall/js"
)

func defaultServiceURL() string {
	location := js.Global().Get("location")
	if location.Truthy() && (location.Get("protocol").String() == "http:" || location.Get("protocol").String() == "https:") {
		return location.Get("origin").String()
	}
	return ""
}

// Fetch follows redirects before Go's CheckRedirect sees them. Refuse redirects
// at Fetch so a signed request cannot silently change destination or method.
type browserTransport struct{ base *http.Transport }

func (b browserTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.Header.Set("js.fetch:redirect", "error")
	return b.base.RoundTrip(req)
}
func transport() http.RoundTripper { return browserTransport{new(http.Transport)} }
