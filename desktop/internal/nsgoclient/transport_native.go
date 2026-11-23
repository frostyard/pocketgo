//go:build !js

package nsgoclient

import "net/http"

func defaultServiceURL() string { return "" }

// The state store selects its direct, origin-checked transport on desktop.
func transport() http.RoundTripper { return nil }
