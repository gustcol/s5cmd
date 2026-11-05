//go:build !http3

package storage

import (
	"crypto/tls"
	"net/http"
)

func newHTTP3RoundTripper(*tls.Config, Options) (http.RoundTripper, error) {
	return nil, errNoHTTP3
}
