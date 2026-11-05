//go:build http3

package storage

import (
	"crypto/tls"
	"errors"
	"net/http"
)

// newHTTP3RoundTripper is the hook for the HTTP/3 transport. The QUIC
// dependency is not part of the module yet, so this build only reserves the
// tag.
func newHTTP3RoundTripper(*tls.Config, Options) (http.RoundTripper, error) {
	return nil, errors.New("HTTP/3 support is not implemented yet")
}
