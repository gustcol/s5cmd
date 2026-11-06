package storage

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/peak/s5cmd/v2/log"
	"github.com/peak/s5cmd/v2/storage/url"
)

// --checksum-algorithm sends the checksum as a header computed before the
// request is signed, so it has to work together with --unsigned-payload and
// --no-content-md5: with both of them, the checksum is the only end-to-end
// check of the body that is left. aws-sdk-go v1 cannot send trailing
// checksums with an unsigned payload, so a header is the only way.
func TestChecksumWithPayloadOptions(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CA_BUNDLE", "")
	t.Setenv("AWS_SDK_LOAD_CONFIG", "0")
	pki := newTestPKI(t)

	const partSize = 5 << 20
	// 2 parts of partSize plus a short last part.
	body := strings.Repeat("0123456789abcdef", (2*partSize+1000)/16)
	sumOf := func(s string) string {
		h := crc32.New(crc32cTable)
		io.WriteString(h, s)
		return base64.StdEncoding.EncodeToString(h.Sum(nil))
	}

	for _, unsigned := range []bool{false, true} {
		for _, noMD5 := range []bool{false, true} {
			for _, multipart := range []bool{false, true} {
				name := map[bool]string{true: "unsigned", false: "signed"}[unsigned] + "/" +
					map[bool]string{true: "nomd5", false: "md5"}[noMD5] + "/" +
					map[bool]string{true: "multipart", false: "single"}[multipart]
				t.Run(name, func(t *testing.T) {
					var mu sync.Mutex
					type put struct{ sha, md5, crc, body string }
					var puts []put
					var completeBody string

					srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						data, _ := io.ReadAll(r.Body)
						mu.Lock()
						defer mu.Unlock()
						switch {
						case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
							io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>key</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
						case r.Method == http.MethodPost && r.URL.Query().Has("uploadId"):
							completeBody = string(data)
							io.WriteString(w, `<CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>key</Key><ETag>"x-3"</ETag></CompleteMultipartUploadResult>`)
						case r.Method == http.MethodPut:
							puts = append(puts, put{
								sha:  r.Header.Get("X-Amz-Content-Sha256"),
								md5:  r.Header.Get("Content-Md5"),
								crc:  r.Header.Get("X-Amz-Checksum-Crc32c"),
								body: string(data),
							})
							w.Header().Set("ETag", `"abc"`)
						}
					}))
					srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCert}}
					srv.StartTLS()
					defer srv.Close()

					u, _ := url.New("s3://bucket/key")
					cli, err := NewRemoteClient(context.Background(), u, Options{
						Endpoint: srv.URL, LogLevel: log.LevelError, region: "us-east-1", CACert: pki.caFile,
						UnsignedPayload: unsigned, NoContentMD5: noMD5,
					})
					if err != nil {
						t.Fatal(err)
					}

					data := body
					if !multipart {
						data = "hello"
					}
					err = cli.Put(context.Background(), strings.NewReader(data), u,
						Metadata{ChecksumAlgorithm: string(ChecksumCRC32C)}, 2, partSize)
					if err != nil {
						t.Fatal(err)
					}

					mu.Lock()
					defer mu.Unlock()
					wantPuts := 1
					if multipart {
						wantPuts = 3
					}
					if len(puts) != wantPuts {
						t.Fatalf("got %d PUT requests, want %d", len(puts), wantPuts)
					}
					for i, p := range puts {
						if got := p.sha == "UNSIGNED-PAYLOAD"; got != unsigned {
							t.Errorf("put %d: X-Amz-Content-Sha256 = %q, unsigned = %v", i, p.sha, unsigned)
						}
						if got := p.md5 != ""; got == noMD5 {
							t.Errorf("put %d: Content-Md5 = %q, noMD5 = %v", i, p.md5, noMD5)
						}
						if p.crc == "" || p.crc != sumOf(p.body) {
							t.Errorf("put %d: checksum header %q does not match the body (%q)", i, p.crc, sumOf(p.body))
						}
					}
					if multipart {
						for _, p := range puts {
							if !strings.Contains(completeBody, "<ChecksumCRC32C>"+p.crc+"</ChecksumCRC32C>") {
								t.Errorf("part checksum %s is not in the completion request: %s", p.crc, completeBody)
							}
						}
					}
				})
			}
		}
	}
}
