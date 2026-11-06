package storage

import (
	"context"
	"crypto/tls"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/peak/s5cmd/v2/log"
	"github.com/peak/s5cmd/v2/storage/url"
)

// The payload options travel through NewRemoteClient, which copies the options
// field by field, so this goes through the whole stack.
func TestUploadPayloadOptions(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CA_BUNDLE", "")
	t.Setenv("AWS_SDK_LOAD_CONFIG", "0")
	pki := newTestPKI(t)

	const unsigned = "UNSIGNED-PAYLOAD"
	tests := []struct {
		name     string
		tls      bool
		opts     Options
		unsigned bool
		wantMD5  bool
	}{
		{"default over https", true, Options{}, false, true},
		{"unsigned payload over https", true, Options{UnsignedPayload: true}, true, true},
		{"no content md5 over https", true, Options{NoContentMD5: true}, false, false},
		{"both over https", true, Options{UnsignedPayload: true, NoContentMD5: true}, true, false},
		{"unsigned payload is ignored over http", false, Options{UnsignedPayload: true}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var sha, md5 string
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					mu.Lock()
					sha = r.Header.Get("X-Amz-Content-Sha256")
					md5 = r.Header.Get("Content-Md5")
					mu.Unlock()
				}
				w.Header().Set("ETag", `"abc"`)
			})
			var srv *httptest.Server
			if tc.tls {
				srv = httptest.NewUnstartedServer(h)
				srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.serverCert}}
				srv.StartTLS()
			} else {
				srv = httptest.NewServer(h)
			}
			defer srv.Close()

			opts := tc.opts
			opts.Endpoint = srv.URL
			opts.LogLevel = log.LevelError
			opts.region = "us-east-1"
			if tc.tls {
				opts.CACert = pki.caFile
			}
			u, err := url.New("s3://bucket/key")
			if err != nil {
				t.Fatal(err)
			}
			cli, err := NewRemoteClient(context.Background(), u, opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := cli.Put(context.Background(), strings.NewReader("hello"), u, Metadata{}, 1, 5<<20); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()
			if got := sha == unsigned; got != tc.unsigned {
				t.Errorf("X-Amz-Content-Sha256 = %q, unsigned = %v, want %v", sha, got, tc.unsigned)
			}
			if got := md5 != ""; got != tc.wantMD5 {
				t.Errorf("Content-Md5 = %q, want present = %v", md5, tc.wantMD5)
			}
		})
	}
}

// Content-MD5 is mandatory for DeleteObjects and a few other operations, so
// --no-content-md5 may only affect the body of uploads.
func TestNoContentMD5OnlyAffectsUploads(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CA_BUNDLE", "")
	t.Setenv("AWS_SDK_LOAD_CONFIG", "0")

	for _, noMD5 := range []bool{false, true} {
		sc := &SessionCache{sessions: map[Options]*session.Session{}}
		sess, err := sc.newSession(context.Background(), Options{
			Endpoint: "http://127.0.0.1:1", NoContentMD5: noMD5, LogLevel: log.LevelError, region: "us-east-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		c := s3.New(sess)

		del, _ := c.DeleteObjectsRequest(&s3.DeleteObjectsInput{
			Bucket: aws.String("b"),
			Delete: &s3.Delete{Objects: []*s3.ObjectIdentifier{{Key: aws.String("k")}}},
		})
		if err := del.Build(); err != nil {
			t.Fatal(err)
		}
		if del.HTTPRequest.Header.Get("Content-Md5") == "" {
			t.Errorf("noMD5=%v: DeleteObjects lost its Content-MD5", noMD5)
		}

		put, _ := c.PutObjectRequest(&s3.PutObjectInput{
			Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("data"),
		})
		if err := put.Build(); err != nil {
			t.Fatal(err)
		}
		if got := put.HTTPRequest.Header.Get("Content-Md5") != ""; got == noMD5 {
			t.Errorf("noMD5=%v: PutObject has Content-MD5 = %v", noMD5, got)
		}

		part, _ := c.UploadPartRequest(&s3.UploadPartInput{
			Bucket: aws.String("b"), Key: aws.String("k"), UploadId: aws.String("u"),
			PartNumber: aws.Int64(1), Body: strings.NewReader("data"),
		})
		if err := part.Build(); err != nil {
			t.Fatal(err)
		}
		if got := part.HTTPRequest.Header.Get("Content-Md5") != ""; got == noMD5 {
			t.Errorf("noMD5=%v: UploadPart has Content-MD5 = %v", noMD5, got)
		}
	}
}
