package e2e

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
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/igungor/gofakes3"
	"github.com/igungor/gofakes3/backend/s3mem"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/fs"
	"gotest.tools/v3/icmd"
)

type tlsFixture struct {
	url        string
	caFile     string
	clientCert string
	clientKey  string
}

func pemFile(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	b := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	assert.NilError(t, os.WriteFile(path, b, 0o600))
}

// newTLSServer starts an S3 server over TLS. With mutual set, it rejects
// clients that do not present a certificate signed by the test CA.
func newTLSServer(t *testing.T, mutual bool) tlsFixture {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	assert.NilError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	assert.NilError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	assert.NilError(t, err)

	issue := func(cn string, serial int64, server bool) ([]byte, *ecdsa.PrivateKey) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		assert.NilError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
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
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		assert.NilError(t, err)
		return der, key
	}

	f := tlsFixture{
		caFile:     filepath.Join(dir, "ca.pem"),
		clientCert: filepath.Join(dir, "client.pem"),
		clientKey:  filepath.Join(dir, "client.key"),
	}
	pemFile(t, f.caFile, "CERTIFICATE", caDER)

	cDER, cKey := issue("client", 3, false)
	pemFile(t, f.clientCert, "CERTIFICATE", cDER)
	kb, err := x509.MarshalECPrivateKey(cKey)
	assert.NilError(t, err)
	pemFile(t, f.clientKey, "EC PRIVATE KEY", kb)

	sDER, sKey := issue("server", 2, true)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	cfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{sDER}, PrivateKey: sKey}}}
	if mutual {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = pool
	}

	backend := s3mem.New()
	assert.NilError(t, backend.CreateBucket("bucket"))
	faker := gofakes3.New(backend, gofakes3.WithTimeSkewLimit(0))
	srv := httptest.NewUnstartedServer(faker.Server())
	srv.EnableHTTP2 = true
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func tlsS5cmd(t *testing.T, f tlsFixture, args ...string) *icmd.Result {
	t.Helper()
	dir := fs.NewDir(t, t.Name())
	cmd := s5cmd(dir.Path(), f.url)(append([]string{"--retry-count", "0"}, args...)...)
	// an inherited bundle must not decide the outcome of these tests
	cmd.Env = append(cmd.Env, "AWS_CA_BUNDLE=", "AWS_SDK_GO_CLIENT_TLS_CERT=", "AWS_SDK_GO_CLIENT_TLS_KEY=")
	return icmd.RunCmd(cmd)
}

func TestMutualTLS(t *testing.T) {
	t.Parallel()
	f := newTLSServer(t, true)

	t.Run("client certificate accepted", func(t *testing.T) {
		res := tlsS5cmd(t, f, "--ca-cert", f.caFile, "--client-cert", f.clientCert, "--client-key", f.clientKey, "ls", "s3://bucket")
		res.Assert(t, icmd.Success)
	})

	t.Run("missing client certificate rejected", func(t *testing.T) {
		res := tlsS5cmd(t, f, "--ca-cert", f.caFile, "ls", "s3://bucket")
		res.Assert(t, icmd.Expected{ExitCode: 1})
	})

	t.Run("untrusted server rejected", func(t *testing.T) {
		res := tlsS5cmd(t, f, "--client-cert", f.clientCert, "--client-key", f.clientKey, "ls", "s3://bucket")
		res.Assert(t, icmd.Expected{ExitCode: 1})
	})

	t.Run("cert without key", func(t *testing.T) {
		res := tlsS5cmd(t, f, "--ca-cert", f.caFile, "--client-cert", f.clientCert, "ls", "s3://bucket")
		res.Assert(t, icmd.Expected{ExitCode: 1, Err: "--client-cert requires --client-key"})
	})

	t.Run("ca-cert wins over AWS_CA_BUNDLE", func(t *testing.T) {
		dir := fs.NewDir(t, t.Name())
		cmd := s5cmd(dir.Path(), f.url)("--retry-count", "0", "--ca-cert", f.caFile, "--client-cert", f.clientCert, "--client-key", f.clientKey, "ls", "s3://bucket")
		cmd.Env = append(cmd.Env, "AWS_CA_BUNDLE="+filepath.Join(t.TempDir(), "missing.pem"))
		icmd.RunCmd(cmd).Assert(t, icmd.Success)
	})

	t.Run("copy through mutual TLS", func(t *testing.T) {
		dir := fs.NewDir(t, t.Name(), fs.WithFile("a.txt", "hello"))
		args := []string{"--retry-count", "0", "--ca-cert", f.caFile, "--client-cert", f.clientCert, "--client-key", f.clientKey}
		cmd := s5cmd(dir.Path(), f.url)(append(args, "cp", dir.Join("a.txt"), "s3://bucket/a.txt")...)
		cmd.Env = append(cmd.Env, "AWS_CA_BUNDLE=")
		icmd.RunCmd(cmd).Assert(t, icmd.Success)
		cmd = s5cmd(dir.Path(), f.url)(append(args, "cat", "s3://bucket/a.txt")...)
		cmd.Env = append(cmd.Env, "AWS_CA_BUNDLE=")
		icmd.RunCmd(cmd).Assert(t, icmd.Expected{Out: "hello"})
	})
}

func TestTransportFlag(t *testing.T) {
	t.Parallel()
	f := newTLSServer(t, false)

	for _, tc := range []struct{ mode, proto string }{
		{"auto", "HTTP/2.0"},
		{"http1", "HTTP/1.1"},
		{"http2", "HTTP/2.0"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			level := "debug"
			if tc.mode == "auto" {
				level = "trace" // auto reports the protocol at trace level only
			}
			res := tlsS5cmd(t, f, "--log", level, "--ca-cert", f.caFile, "--transport", tc.mode, "ls", "s3://bucket")
			res.Assert(t, icmd.Expected{ExitCode: 0, Err: "negotiated protocol: " + tc.proto})
		})
	}

	t.Run("http3 needs a build with support", func(t *testing.T) {
		res := tlsS5cmd(t, f, "--ca-cert", f.caFile, "--transport", "http3", "ls", "s3://bucket")
		res.Assert(t, icmd.Expected{ExitCode: 1, Err: "HTTP/3"})
	})

	t.Run("unknown value", func(t *testing.T) {
		res := tlsS5cmd(t, f, "--transport", "spdy", "ls", "s3://bucket")
		res.Assert(t, icmd.Expected{ExitCode: 1, Err: "allowed values"})
	})
}
