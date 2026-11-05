package storage

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go/aws/request"

	"github.com/peak/s5cmd/v2/log"
)

// Transport modes accepted by --transport.
const (
	TransportAuto  = "auto"
	TransportHTTP1 = "http1"
	TransportHTTP2 = "http2"
	TransportHTTP3 = "http3"
)

// TransportModes lists the values accepted by --transport. http3 is always
// listed, but only works in builds made with the http3 build tag.
var TransportModes = []string{TransportAuto, TransportHTTP1, TransportHTTP2, TransportHTTP3}

// errNoHTTP3 is returned by builds without HTTP/3 support.
var errNoHTTP3 = errors.New("this build has no HTTP/3 support")

func validTransport(mode string) error {
	switch mode {
	case "", TransportAuto, TransportHTTP1, TransportHTTP2, TransportHTTP3:
		return nil
	}
	return fmt.Errorf("invalid transport %q, allowed values: auto, http1, http2, http3", mode)
}

// tlsConfig builds the TLS configuration from the TLS related options. It
// returns nil when no option asks for a custom configuration.
//
// A CA file replaces the system pool; it is not appended to it. This is what
// AWS_CA_BUNDLE and curl --cacert do, so all three behave the same way.
func tlsConfig(opts Options) (*tls.Config, error) {
	if opts.ClientCert == "" && opts.ClientKey != "" {
		return nil, errors.New("--client-key requires --client-cert")
	}
	if opts.ClientCert != "" && opts.ClientKey == "" {
		return nil, errors.New("--client-cert requires --client-key")
	}
	if opts.CACert == "" && opts.ClientCert == "" && !opts.NoVerifySSL {
		return nil, nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.NoVerifySSL {
		cfg.InsecureSkipVerify = true
	}

	if opts.CACert != "" {
		pool, err := loadCAPool(opts.CACert)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}

	if opts.ClientCert != "" {
		cert, err := tls.LoadX509KeyPair(opts.ClientCert, opts.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("cannot load client certificate %q and key %q: %w", opts.ClientCert, opts.ClientKey, err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read CA certificate file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no valid PEM certificates found in CA file %q", path)
	}
	return pool, nil
}

// setProtocols restricts the HTTP protocols of t. Mode auto leaves the
// transport untouched, so ALPN picks h2 or HTTP/1.1 as before.
func setProtocols(t *http.Transport, mode string) {
	var p http.Protocols
	switch mode {
	case TransportHTTP1:
		p.SetHTTP1(true)
	case TransportHTTP2:
		p.SetHTTP2(true)
	default:
		return
	}
	t.Protocols = &p
}

// requireH2 makes the TLS handshake fail when the server does not pick h2 in
// ALPN. Servers that do not know ALPN simply ignore it, and Go would then fall
// back to HTTP/1.1 even though only HTTP/2 was asked for.
func requireH2(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	prev := cfg.VerifyConnection
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if cs.NegotiatedProtocol != "h2" {
			got := cs.NegotiatedProtocol
			if got == "" {
				got = "no protocol"
			}
			return fmt.Errorf("--transport http2 was requested, but the server negotiated %s instead of h2", got)
		}
		if prev != nil {
			return prev(cs)
		}
		return nil
	}
	return cfg
}

// newHTTP3RoundTripper is provided by transport_http3.go in builds with the
// http3 tag and by transport_nohttp3.go otherwise.

// protocolLogger logs the protocol negotiated by the first response of each
// kind (HTTP/1.1, HTTP/2.0). With an explicit --transport it is a debug
// message. With auto it is a trace message, because many scripts and tests
// compare the exact debug output of a run.
func protocolLogger(opts Options) func(r *request.Request) {
	need := log.LevelTrace
	if opts.Transport != "" && opts.Transport != TransportAuto {
		need = log.LevelDebug
	}
	var seen sync.Map
	return func(r *request.Request) {
		// the global logger is not initialized in every code path (shell
		// completion, for one), so check the level before using it.
		if opts.LogLevel > need || r.HTTPResponse == nil {
			return
		}
		proto := r.HTTPResponse.Proto
		if _, loaded := seen.LoadOrStore(proto, struct{}{}); loaded {
			return
		}
		msg := fmt.Sprintf("negotiated protocol: %s", proto)
		if need == log.LevelDebug {
			log.Debug(log.DebugMessage{Err: msg})
		} else {
			log.Trace(log.TraceMessage{Message: msg})
		}
	}
}
