package storage

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

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
	if opts.TransportConns < 0 {
		return nil, errors.New("--transport-conns must not be negative")
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

// setProtocols restricts the HTTP protocols of t. Mode auto uses HTTP/1.1.
//
// Go sends every request to a host over a single HTTP/2 connection, so when a
// TLS front end such as Caddy or nginx offers h2 the whole transfer shares one
// TCP stream and one encryption pipeline. A 4 GiB download took 7.8 s that way
// against 1.7 s over HTTP/1.1 with the idle pool of this package, and
// spreading h2 over 8 connections only got back to 1.9 s with about twice the
// CPU. AWS S3 itself only speaks HTTP/1.1.
func setProtocols(t *http.Transport, mode string) {
	var p http.Protocols
	switch mode {
	case TransportHTTP2:
		p.SetHTTP2(true)
	default:
		p.SetHTTP1(true)
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

// defaultH2Conns is the number of connections used for --transport http2 when
// --transport-conns is not given. Go sends every request to a host over one
// HTTP/2 connection, which caps the throughput of bulk copies at what a single
// TCP stream and its flow-control window can do.
const defaultH2Conns = 8

// transportConns returns how many independent transports newHTTPClient builds.
func transportConns(opts Options) int {
	if opts.Transport != TransportHTTP2 {
		if opts.TransportConns > 0 && opts.LogLevel <= log.LevelDebug {
			log.Debug(log.DebugMessage{Err: fmt.Sprintf("--transport-conns is ignored with --transport %s", transportName(opts.Transport))})
		}
		return 1
	}
	if opts.TransportConns > 0 {
		return opts.TransportConns
	}
	return defaultH2Conns
}

func transportName(mode string) string {
	if mode == "" {
		return TransportAuto
	}
	return mode
}

// roundRobinTransport spreads requests over several independent transports.
// Each one has its own connection pool, so with HTTP/2 each opens its own
// connection and has its own flow-control windows.
type roundRobinTransport struct {
	transports []*http.Transport
	// probe is the HTTP/1.1 transport that the session starts with. The SDK
	// loads its TLS settings into it and uses it for credential requests.
	probe *http.Transport
	next  atomic.Uint64
}

func (r *roundRobinTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	i := (r.next.Add(1) - 1) % uint64(len(r.transports))
	return r.transports[i].RoundTrip(req)
}

func (r *roundRobinTransport) CloseIdleConnections() {
	for _, t := range r.transports {
		t.CloseIdleConnections()
	}
}

// adoptSDKTLS copies the TLS settings that the SDK resolved while it created
// the session onto the transports. probe is the plain transport the session
// was given: the SDK loads the CA bundle (--ca-cert, AWS_CA_BUNDLE or ca_bundle
// from the shared config, in that order of priority) and the client
// certificate from AWS_SDK_GO_CLIENT_TLS_CERT and _KEY into it. The
// transports share one tls.Config, so it is updated once, before any request.
func (r *roundRobinTransport) adoptSDKTLS(probe *http.Transport) {
	sdk := probe.TLSClientConfig
	if sdk == nil || len(r.transports) == 0 {
		return
	}
	cfg := r.transports[0].TLSClientConfig
	if cfg == nil {
		return
	}
	if sdk.RootCAs != nil {
		cfg.RootCAs = sdk.RootCAs
	}
	for _, c := range sdk.Certificates {
		known := false
		for _, have := range cfg.Certificates {
			if len(have.Certificate) > 0 && len(c.Certificate) > 0 && string(have.Certificate[0]) == string(c.Certificate[0]) {
				known = true
			}
		}
		if !known {
			cfg.Certificates = append(cfg.Certificates, c)
		}
	}
}
