package storage

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	_, err := newHTTPClient(Options{Transport: TransportHTTP3})
	if err == nil {
		t.Fatal("http3 must fail until a build provides it")
	}
	if !strings.Contains(err.Error(), "HTTP/3") {
		t.Errorf("unclear http3 error: %v", err)
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
		{"auto on h2 server", TransportAuto, true, "HTTP/2.0"},
		{"auto on h1 server", TransportAuto, false, "HTTP/1.1"},
		{"default on h2 server", "", true, "HTTP/2.0"},
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
