package storage

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"

	"github.com/peak/s5cmd/v2/log"
)

type testPKI struct {
	dir        string
	caFile     string
	caCert     *x509.Certificate
	caKey      *ecdsa.PrivateKey
	serverCert tls.Certificate
	clientCert string
	clientKey  string
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	b := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string, serial int64, server bool) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	p := &testPKI{dir: dir, caCert: caCert, caKey: caKey}
	p.caFile = filepath.Join(dir, "ca.pem")
	writePEM(t, p.caFile, "CERTIFICATE", caDER)

	sDER, sKey := issue(t, caCert, caKey, "server", 2, true)
	p.serverCert = tls.Certificate{Certificate: [][]byte{sDER}, PrivateKey: sKey}

	cDER, cKey := issue(t, caCert, caKey, "client", 3, false)
	p.clientCert = filepath.Join(dir, "client.pem")
	p.clientKey = filepath.Join(dir, "client.key")
	writePEM(t, p.clientCert, "CERTIFICATE", cDER)
	kb, err := x509.MarshalECPrivateKey(cKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, p.clientKey, "EC PRIVATE KEY", kb)
	return p
}

func TestTLSConfigPairing(t *testing.T) {
	pki := newTestPKI(t)
	tests := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{"cert without key", Options{ClientCert: pki.clientCert}, "--client-cert requires --client-key"},
		{"key without cert", Options{ClientKey: pki.clientKey}, "--client-key requires --client-cert"},
		{"missing cert file", Options{ClientCert: filepath.Join(pki.dir, "nope.pem"), ClientKey: pki.clientKey}, "cannot load client certificate"},
		{"mismatched pair", Options{ClientCert: pki.caFile, ClientKey: pki.clientKey}, "cannot load client certificate"},
		{"missing ca file", Options{CACert: filepath.Join(pki.dir, "nope.pem")}, "cannot read CA certificate file"},
		{"ca file is not pem", Options{CACert: writeFile(t, pki.dir, "junk.pem", "not a pem")}, "no valid PEM certificates"},
		{"good pair", Options{ClientCert: pki.clientCert, ClientKey: pki.clientKey}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newHTTPClient(tc.opts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewHTTPClientTLSOptions(t *testing.T) {
	pki := newTestPKI(t)
	c, err := newHTTPClient(Options{
		CACert:       pki.caFile,
		ClientCert:   pki.clientCert,
		ClientKey:    pki.clientKey,
		ConnPoolSize: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Transport.(*http.Transport)
	cfg := tr.TLSClientConfig
	if cfg == nil || cfg.RootCAs == nil {
		t.Fatal("RootCAs not set")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("client certificates = %d, want 1", len(cfg.Certificates))
	}
	if cfg.InsecureSkipVerify {
		t.Error("verification must stay on")
	}
	// the sized pool and the timeouts of the default transport are kept
	if tr.MaxIdleConnsPerHost != 64 || tr.MaxIdleConns != 64 {
		t.Errorf("idle pool = %d/%d, want 64/64", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second || tr.IdleConnTimeout != 90*time.Second || tr.Proxy == nil {
		t.Errorf("timeouts or proxy lost: %v %v", tr.TLSHandshakeTimeout, tr.IdleConnTimeout)
	}

	// --no-verify-ssl keeps its meaning next to the other options
	c, err = newHTTPClient(Options{NoVerifySSL: true, CACert: pki.caFile})
	if err != nil {
		t.Fatal(err)
	}
	cfg = c.Transport.(*http.Transport).TLSClientConfig
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify not set")
	}
}

func TestNewHTTPClientTransportValidation(t *testing.T) {
	if _, err := newHTTPClient(Options{Transport: "spdy"}); err == nil {
		t.Error("expected an error for an unknown transport")
	}
	if _, err := newHTTPClient(Options{Transport: "http3"}); err == nil {
		t.Error("http3 is not supported")
	}
	for _, mode := range []string{"", TransportAuto} {
		if c, err := newHTTPClient(Options{Transport: mode}); err != nil || c != nil {
			t.Errorf("mode %q: got %v, %v; want the SDK default client", mode, c, err)
		}
	}
}

func TestNegotiatedProtocol(t *testing.T) {
	pki := newTestPKI(t)

	newServer := func(h2 bool) *httptest.Server {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("ok"))
		}))
		srv.EnableHTTP2 = h2
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCert}}
		srv.StartTLS()
		t.Cleanup(srv.Close)
		return srv
	}
	h2Server, h1Server := newServer(true), newServer(false)

	tests := []struct {
		name string
		mode string
		h2   bool
		want string // empty means the request must fail
	}{
		{"auto on h2 server", TransportAuto, true, "HTTP/1.1"},
		{"auto on h1 server", TransportAuto, false, "HTTP/1.1"},
		{"default on h2 server", "", true, "HTTP/1.1"},
		{"http1 on h2 server", TransportHTTP1, true, "HTTP/1.1"},
		{"http1 on h1 server", TransportHTTP1, false, "HTTP/1.1"},
		{"http2 on h2 server", TransportHTTP2, true, "HTTP/2.0"},
		{"http2 on h1 server", TransportHTTP2, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := h1Server
			if tc.h2 {
				srv = h2Server
			}
			// ConnPoolSize forces a custom transport even for mode auto
			c, err := newHTTPClient(Options{CACert: pki.caFile, Transport: tc.mode, ConnPoolSize: 4})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.Get(srv.URL)
			if tc.want == "" {
				if err == nil {
					resp.Body.Close()
					t.Fatalf("expected an error, got protocol %s", resp.Proto)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.Proto != tc.want {
				t.Errorf("protocol = %s, want %s", resp.Proto, tc.want)
			}
		})
	}
}

// Servers that do not know ALPN just ignore it. --transport http2 has to fail
// there instead of silently speaking HTTP/1.1.
func TestHTTP2FailsWhenServerIgnoresALPN(t *testing.T) {
	pki := newTestPKI(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pki.serverCert}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	url := "https://" + ln.Addr().String()
	for mode, wantErr := range map[string]bool{TransportAuto: false, TransportHTTP1: false, TransportHTTP2: true} {
		c, err := newHTTPClient(Options{CACert: pki.caFile, Transport: mode, ConnPoolSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Get(url)
		if err == nil {
			resp.Body.Close()
		}
		if (err != nil) != wantErr {
			t.Errorf("mode %s: err = %v, want error: %v", mode, err, wantErr)
		}
		if wantErr && (err == nil || !strings.Contains(err.Error(), "negotiated")) {
			t.Errorf("mode %s: unclear error: %v", mode, err)
		}
	}
}

func TestTransportConnsOpensIndependentConnections(t *testing.T) {
	pki := newTestPKI(t)

	var accepted atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCert}}
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			accepted.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	tests := []struct {
		name  string
		mode  string
		conns int
		want  int32
	}{
		{"http2 default is 8", TransportHTTP2, 0, defaultH2Conns},
		{"http2 with 4", TransportHTTP2, 4, 4},
		{"http2 with 1 uses one connection", TransportHTTP2, 1, 1},
		{"auto ignores the setting", TransportAuto, 4, 1},
		{"http1 ignores the setting", TransportHTTP1, 4, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			accepted.Store(0)
			c, err := newHTTPClient(Options{
				CACert: pki.caFile, Transport: tc.mode, TransportConns: tc.conns, ConnPoolSize: 4,
				LogLevel: log.LevelError, // the global logger is not set up in unit tests
			})
			if err != nil {
				t.Fatal(err)
			}
			// requests are sequential, so every connection is reused
			for i := 0; i < 32; i++ {
				resp, err := c.Get(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			if got := accepted.Load(); got != tc.want {
				t.Errorf("accepted connections = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTransportConnsValidation(t *testing.T) {
	if _, err := newHTTPClient(Options{Transport: TransportHTTP2, TransportConns: -1}); err == nil {
		t.Error("negative --transport-conns must fail")
	}
}

// The SDK resolves the CA bundle from --ca-cert, AWS_CA_BUNDLE and ca_bundle in
// the shared config. All three must reach the transports of a multi connection
// client, which the SDK cannot configure itself.
func TestTransportConnsTrustAnchors(t *testing.T) {
	pki := newTestPKI(t)
	other := newTestPKI(t) // a CA the server does not use

	var accepted atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCert}}
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			accepted.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	configWith := func(ca string) string {
		return writeFile(t, t.TempDir(), "config", "[default]\nca_bundle = "+ca+"\n")
	}

	tests := []struct {
		name     string
		flag     string
		env      string
		sharedCA string
		wantOK   bool
	}{
		{"flag", pki.caFile, "", "", true},
		{"AWS_CA_BUNDLE", "", pki.caFile, "", true},
		{"shared config ca_bundle", "", "", pki.caFile, true},
		{"flag wins over a wrong AWS_CA_BUNDLE", pki.caFile, other.caFile, "", true},
		{"AWS_CA_BUNDLE wins over a wrong shared config", "", pki.caFile, other.caFile, true},
		{"wrong CA is rejected", "", other.caFile, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfgFile := filepath.Join(t.TempDir(), "none")
			if tc.sharedCA != "" {
				cfgFile = configWith(tc.sharedCA)
			}
			t.Setenv("AWS_SDK_LOAD_CONFIG", "1")
			t.Setenv("AWS_CONFIG_FILE", cfgFile)
			t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
			t.Setenv("AWS_CA_BUNDLE", tc.env)
			t.Setenv("AWS_SDK_GO_CLIENT_TLS_CERT", "")
			t.Setenv("AWS_SDK_GO_CLIENT_TLS_KEY", "")

			accepted.Store(0)
			sc := &SessionCache{sessions: map[Options]*session.Session{}}
			sess, err := sc.newSession(context.Background(), Options{
				Endpoint:       srv.URL,
				CACert:         tc.flag,
				Transport:      TransportHTTP2,
				TransportConns: 4,
				LogLevel:       log.LevelError,
				region:         "us-east-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			client := sess.Config.HTTPClient
			if _, ok := client.Transport.(*roundRobinTransport); !ok {
				t.Fatalf("transport = %T, want the multi connection transport", client.Transport)
			}
			for i := 0; i < 8; i++ {
				resp, err := client.Get(srv.URL)
				if !tc.wantOK {
					if err == nil {
						resp.Body.Close()
						t.Fatal("request must fail with an untrusted CA")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			if got := accepted.Load(); got != 4 {
				t.Errorf("accepted connections = %d, want 4", got)
			}
		})
	}
}

func TestTransportConnsClientCertFromEnv(t *testing.T) {
	pki := newTestPKI(t)
	t.Setenv("AWS_SDK_GO_CLIENT_TLS_CERT", pki.clientCert)
	t.Setenv("AWS_SDK_GO_CLIENT_TLS_KEY", pki.clientKey)
	t.Setenv("AWS_CA_BUNDLE", "")

	sc := &SessionCache{sessions: map[Options]*session.Session{}}
	sess, err := sc.newSession(context.Background(), Options{
		CACert:         pki.caFile,
		Transport:      TransportHTTP2,
		TransportConns: 2,
		LogLevel:       log.LevelError,
		region:         "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	rr := sess.Config.HTTPClient.Transport.(*roundRobinTransport)
	cfg := rr.transports[0].TLSClientConfig
	if len(cfg.Certificates) != 1 || cfg.RootCAs == nil {
		t.Errorf("TLS settings not adopted: %d certificates, RootCAs %v", len(cfg.Certificates), cfg.RootCAs)
	}
	if cfg != rr.transports[1].TLSClientConfig {
		t.Error("transports must share one TLS config")
	}
}

// With AWS_CA_BUNDLE and nothing else set, the SDK adds the CA to the
// TLSClientConfig of our transport. The transport must not carry over the
// "h2" ALPN entry that http.DefaultTransport gets once it has been used, or
// the server picks HTTP/2 and an HTTP/1.1 transport cannot read the answer.
func TestHTTP1WithAWSCABundleFromEnv(t *testing.T) {
	pki := newTestPKI(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Proto))
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// Using the default transport once makes it add h2 to its TLS config.
	if resp, err := http.DefaultClient.Get(srv.URL); err == nil {
		resp.Body.Close()
	}

	t.Setenv("AWS_SDK_LOAD_CONFIG", "0")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_CA_BUNDLE", pki.caFile)
	t.Setenv("AWS_SDK_GO_CLIENT_TLS_CERT", "")
	t.Setenv("AWS_SDK_GO_CLIENT_TLS_KEY", "")

	for _, mode := range []string{"", TransportAuto, TransportHTTP1} {
		t.Run("mode="+mode, func(t *testing.T) {
			sc := &SessionCache{sessions: map[Options]*session.Session{}}
			sess, err := sc.newSession(context.Background(), Options{
				Endpoint:     srv.URL,
				Transport:    mode,
				ConnPoolSize: 8,
				LogLevel:     log.LevelError,
				region:       "us-east-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := sess.Config.HTTPClient.Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.Proto != "HTTP/1.1" {
				t.Errorf("protocol = %s, want HTTP/1.1", resp.Proto)
			}
		})
	}
}

// The SDK uses the session client for credential requests (container
// credentials, STS, the instance metadata service). Those are plain HTTP or
// need HTTP/1.1, so they must not go through the h2-only transports.
func TestCredentialRequestsAvoidH2OnlyTransport(t *testing.T) {
	pki := newTestPKI(t)
	// An HTTP/1.1 only https server: a plain http server would not do, because
	// newer Go versions speak HTTP/1.1 over http:// even when only h2 is set.
	creds := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"AccessKeyId":"id","SecretAccessKey":"secret","Token":"tok","Expiration":"2099-01-01T00:00:00Z"}`))
	}))
	creds.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCert}, NextProtos: []string{"http/1.1"}}
	creds.StartTLS()
	t.Cleanup(creds.Close)

	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_SDK_LOAD_CONFIG", "0")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_CA_BUNDLE", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", creds.URL)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	for _, conns := range []int{1, 4} {
		sc := &SessionCache{sessions: map[Options]*session.Session{}}
		sess, err := sc.newSession(context.Background(), Options{
			Endpoint:       "https://127.0.0.1:1",
			CACert:         pki.caFile,
			Transport:      TransportHTTP2,
			TransportConns: conns,
			ConnPoolSize:   4,
			LogLevel:       log.LevelError,
			region:         "us-east-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		v, err := sess.Config.Credentials.Get()
		if err != nil {
			t.Fatalf("conns=%d: credential request failed: %v", conns, err)
		}
		if v.AccessKeyID != "id" {
			t.Errorf("conns=%d: access key = %q", conns, v.AccessKeyID)
		}
	}
}

// The probe transport that credential clients use must behave like the real
// ones apart from the protocol.
func TestHTTP2ProbeTransport(t *testing.T) {
	pki := newTestPKI(t)
	c, err := newHTTPClient(Options{
		CACert: pki.caFile, ClientCert: pki.clientCert, ClientKey: pki.clientKey,
		NoVerifySSL: true, Transport: TransportHTTP2, TransportConns: 1, ConnPoolSize: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	rr, ok := c.Transport.(*roundRobinTransport)
	if !ok {
		t.Fatalf("transport = %T, want the multi connection transport", c.Transport)
	}
	p := rr.probe
	if p == nil {
		t.Fatal("no probe transport")
	}
	if p.Proxy == nil || p.TLSHandshakeTimeout == 0 || p.DialContext == nil {
		t.Error("probe lost the proxy, the TLS handshake timeout or the dialer of the default transport")
	}
	if p.MaxIdleConnsPerHost != 8 {
		t.Errorf("probe idle pool = %d, want 8", p.MaxIdleConnsPerHost)
	}
	if p.Protocols == nil || p.Protocols.HTTP2() || !p.Protocols.HTTP1() {
		t.Errorf("probe protocols = %v, want HTTP/1.1 only", p.Protocols)
	}
	cfg := p.TLSClientConfig
	if cfg == nil || !cfg.InsecureSkipVerify || cfg.RootCAs == nil || len(cfg.Certificates) != 1 {
		t.Errorf("probe TLS config does not carry the options: %+v", cfg)
	}
	if cfg != nil && cfg.VerifyConnection != nil {
		t.Error("probe must not require h2")
	}
}
