package storage

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"gotest.tools/v3/assert"

	"github.com/peak/s5cmd/v2/storage/url"
)

func TestParseChecksumAlgorithm(t *testing.T) {
	tests := []struct {
		in      string
		want    ChecksumAlgorithm
		wantErr string
	}{
		{in: "", want: ""},
		{in: "CRC32C", want: ChecksumCRC32C},
		{in: "crc32c", want: ChecksumCRC32C},
		{in: "Crc32", want: ChecksumCRC32},
		{in: "sha256", want: ChecksumSHA256},
		{in: "SHA1", want: ChecksumSHA1},
		{in: "CRC64NVME", wantErr: "not supported"},
		{in: "md5", wantErr: "invalid checksum algorithm"},
	}
	for _, tc := range tests {
		got, err := ParseChecksumAlgorithm(tc.in)
		if tc.wantErr != "" {
			assert.ErrorContains(t, err, tc.wantErr)
			continue
		}
		assert.NilError(t, err)
		assert.Equal(t, got, tc.want)
	}
}

func TestHashReaderKnownValues(t *testing.T) {
	// computed independently of this package.
	want := map[ChecksumAlgorithm]string{
		ChecksumCRC32:  "DUoRhQ==",
		ChecksumCRC32C: "yZRlqg==",
		ChecksumSHA256: "uU0nuZNNPgilLlLX2n2r+sSE7+N6U4DukIj3rOLvzek=",
		ChecksumSHA1:   "Kq5sNclPz7QV2+lfQIuc6R7oRu0=",
	}
	for alg, w := range want {
		got, err := HashReader(context.Background(), alg, strings.NewReader("hello world"))
		assert.NilError(t, err)
		assert.Equal(t, got, w, string(alg))
	}
}

func TestHashReaderCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := HashReader(ctx, ChecksumCRC32C, strings.NewReader("data"))
	assert.ErrorIs(t, err, context.Canceled)
}

func TestObjectChecksumComposite(t *testing.T) {
	c := ObjectChecksum{Algorithm: ChecksumCRC32C, Value: "yZRlqg==-12"}
	assert.Assert(t, c.Composite())
	assert.Equal(t, c.Parts(), int64(12))

	c = ObjectChecksum{Algorithm: ChecksumCRC32C, Value: "yZRlqg=="}
	assert.Assert(t, !c.Composite())
	assert.Equal(t, c.Parts(), int64(0))
}

func TestUniformPartSizes(t *testing.T) {
	assert.DeepEqual(t, uniformPartSizes(25, 10, 3), []int64{10, 10, 5})
	assert.DeepEqual(t, uniformPartSizes(20, 10, 2), []int64{10, 10})
	assert.DeepEqual(t, uniformPartSizes(0, 10, 1), []int64{0})
	assert.Assert(t, uniformPartSizes(25, 10, 2) == nil)
	assert.Assert(t, uniformPartSizes(25, 10, 4) == nil)
	assert.Assert(t, uniformPartSizes(25, 0, 3) == nil)
}

// fakeUploadServer is a tiny S3 endpoint that records the checksum headers it
// receives and, like S3, rejects a request whose checksum does not match the
// body.
type fakeUploadServer struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	puts       []http.Header // PutObject requests
	partSums   map[int]string
	partHeader map[int]http.Header
	createHdr  http.Header
	completed  completeRequest
	rejected   int
}

type completeRequest struct {
	Parts []struct {
		PartNumber     int    `xml:"PartNumber"`
		ETag           string `xml:"ETag"`
		ChecksumCRC32  string `xml:"ChecksumCRC32"`
		ChecksumCRC32C string `xml:"ChecksumCRC32C"`
		ChecksumSHA1   string `xml:"ChecksumSHA1"`
		ChecksumSHA256 string `xml:"ChecksumSHA256"`
	} `xml:"Part"`
}

func (f *fakeUploadServer) verify(h http.Header, body []byte) bool {
	for hdr, alg := range map[string]ChecksumAlgorithm{
		"X-Amz-Checksum-Crc32":  ChecksumCRC32,
		"X-Amz-Checksum-Crc32c": ChecksumCRC32C,
		"X-Amz-Checksum-Sha256": ChecksumSHA256,
		"X-Amz-Checksum-Sha1":   ChecksumSHA1,
	} {
		if got := h.Get(hdr); got != "" {
			want, _ := HashReader(context.Background(), alg, strings.NewReader(string(body)))
			return got == want
		}
	}
	return true
}

func newFakeUploadServer(t *testing.T) *fakeUploadServer {
	f := &fakeUploadServer{t: t, partSums: map[int]string{}, partHeader: map[int]http.Header{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := r.URL.Query()
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			f.createHdr = r.Header.Clone()
			io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>up1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && q.Get("partNumber") != "":
			n, _ := strconv.Atoi(q.Get("partNumber"))
			f.partHeader[n] = r.Header.Clone()
			if !f.verify(r.Header, body) {
				f.rejected++
				http.Error(w, "BadDigest", http.StatusBadRequest)
				return
			}
			w.Header().Set("ETag", `"etag`+q.Get("partNumber")+`"`)
		case r.Method == http.MethodPost && q.Get("uploadId") != "":
			xml.Unmarshal(body, &f.completed)
			io.WriteString(w, `<CompleteMultipartUploadResult><Location>l</Location><Bucket>b</Bucket><Key>k</Key><ETag>"x-3"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == http.MethodPut:
			f.puts = append(f.puts, r.Header.Clone())
			if !f.verify(r.Header, body) {
				f.rejected++
				http.Error(w, "BadDigest", http.StatusBadRequest)
				return
			}
			w.Header().Set("ETag", `"abc"`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUploadServer) storage(t *testing.T) *S3 {
	sess, err := session.NewSession(aws.NewConfig().
		WithEndpoint(f.srv.URL).
		WithRegion("us-east-1").
		WithS3ForcePathStyle(true).
		WithMaxRetries(0).
		WithCredentials(credentials.NewStaticCredentials("id", "key", "")))
	assert.NilError(t, err)
	return &S3{uploader: s3manager.NewUploader(sess)}
}

func checksumHeaderName(alg ChecksumAlgorithm) string {
	return "X-Amz-Checksum-" + strings.ToLower(string(alg))
}

// readerAtOnly mimics what cp passes to Put: it can be read at an offset, and
// reads that must not count as transferred bytes go through ReadAtNoProgress.
type progressReader struct {
	*os.File
	counted   atomic.Int64
	uncounted atomic.Int64
}

func (p *progressReader) ReadAt(b []byte, off int64) (int, error) {
	n, err := p.File.ReadAt(b, off)
	p.counted.Add(int64(n))
	return n, err
}

func (p *progressReader) ReadAtNoProgress(b []byte, off int64) (int, error) {
	n, err := p.File.ReadAt(b, off)
	p.uncounted.Add(int64(n))
	return n, err
}

func TestPutSinglePartSendsChecksum(t *testing.T) {
	u, err := url.New("s3://bucket/key")
	assert.NilError(t, err)

	for _, alg := range ChecksumAlgorithms {
		alg := alg
		t.Run(string(alg), func(t *testing.T) {
			f := newFakeUploadServer(t)
			data := "single part body"
			err := f.storage(t).Put(context.Background(), strings.NewReader(data), u,
				Metadata{ChecksumAlgorithm: string(alg)}, 1, 5<<20)
			assert.NilError(t, err)

			assert.Equal(t, len(f.puts), 1)
			want, err := HashReader(context.Background(), alg, strings.NewReader(data))
			assert.NilError(t, err)
			assert.Equal(t, f.puts[0].Get(checksumHeaderName(alg)), want)
			assert.Equal(t, f.puts[0].Get("X-Amz-Sdk-Checksum-Algorithm"), string(alg))
			assert.Equal(t, f.rejected, 0)
		})
	}
}

func TestPutWithoutChecksumAlgorithmSendsNone(t *testing.T) {
	u, _ := url.New("s3://bucket/key")
	f := newFakeUploadServer(t)
	err := f.storage(t).Put(context.Background(), strings.NewReader("data"), u, Metadata{}, 1, 5<<20)
	assert.NilError(t, err)
	assert.Equal(t, len(f.puts), 1)
	for k := range f.puts[0] {
		assert.Assert(t, !strings.HasPrefix(strings.ToLower(k), "x-amz-checksum"), k)
		assert.Assert(t, !strings.HasPrefix(strings.ToLower(k), "x-amz-sdk-checksum"), k)
	}
}

func TestPutMultipartSendsPartChecksums(t *testing.T) {
	u, _ := url.New("s3://bucket/key")

	// 11 MiB with 5 MiB parts gives three parts: 5, 5 and 1 MiB.
	data := make([]byte, 11<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	path := filepath.Join(t.TempDir(), "big")
	assert.NilError(t, os.WriteFile(path, data, 0o644))

	for _, alg := range ChecksumAlgorithms {
		alg := alg
		t.Run(string(alg), func(t *testing.T) {
			f := newFakeUploadServer(t)
			file, err := os.Open(path)
			assert.NilError(t, err)
			defer file.Close()
			pr := &progressReader{File: file}

			err = f.storage(t).Put(context.Background(), pr, u,
				Metadata{ChecksumAlgorithm: string(alg)}, 3, 5<<20)
			assert.NilError(t, err)
			assert.Equal(t, f.rejected, 0)

			// the multipart upload is created with the algorithm.
			assert.Equal(t, f.createHdr.Get("X-Amz-Checksum-Algorithm"), string(alg))

			// every part carries the checksum of its own bytes.
			assert.Equal(t, len(f.partHeader), 3)
			bounds := [][2]int{{0, 5 << 20}, {5 << 20, 10 << 20}, {10 << 20, 11 << 20}}
			for i, b := range bounds {
				want, err := HashReader(context.Background(), alg, strings.NewReader(string(data[b[0]:b[1]])))
				assert.NilError(t, err)
				assert.Equal(t, f.partHeader[i+1].Get(checksumHeaderName(alg)), want, "part %d", i+1)
			}

			// and the completion lists them.
			assert.Equal(t, len(f.completed.Parts), 3)
			for i, p := range f.completed.Parts {
				want, _ := HashReader(context.Background(), alg, strings.NewReader(string(data[bounds[i][0]:bounds[i][1]])))
				got := map[ChecksumAlgorithm]string{
					ChecksumCRC32: p.ChecksumCRC32, ChecksumCRC32C: p.ChecksumCRC32C,
					ChecksumSHA1: p.ChecksumSHA1, ChecksumSHA256: p.ChecksumSHA256,
				}[alg]
				assert.Equal(t, got, want, "completed part %d", p.PartNumber)
			}

			// hashing reads are not reported as transferred bytes.
			assert.Assert(t, pr.uncounted.Load() >= int64(len(data)), "hashing read %d bytes", pr.uncounted.Load())
		})
	}
}

func TestHashBodyRestoresPosition(t *testing.T) {
	r := strings.NewReader("hello world")
	sum, err := hashBody(ChecksumCRC32, r)
	assert.NilError(t, err)
	assert.Equal(t, sum, "DUoRhQ==")
	rest, _ := io.ReadAll(r)
	assert.Equal(t, string(rest), "hello world")
}

func TestPartDigestsAndComposite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	assert.NilError(t, os.WriteFile(path, []byte("aaaabbbbcc"), 0o644))

	digests, err := partDigests(context.Background(), ChecksumSHA256.newHash, path, []int64{4, 4, 2})
	assert.NilError(t, err)
	assert.Equal(t, len(digests), 3)

	want := compositeValue(ChecksumSHA256.newHash, [][]byte{
		mustDigest(t, ChecksumSHA256, "aaaa"), mustDigest(t, ChecksumSHA256, "bbbb"), mustDigest(t, ChecksumSHA256, "cc"),
	})
	assert.Equal(t, compositeValue(ChecksumSHA256.newHash, digests), want)
	assert.Assert(t, strings.HasSuffix(want, "-3"))

	_, err = partDigests(context.Background(), ChecksumSHA256.newHash, path, []int64{8, 8})
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func mustDigest(t *testing.T, alg ChecksumAlgorithm, s string) []byte {
	t.Helper()
	h := alg.newHash()
	h.Write([]byte(s))
	return h.Sum(nil)
}
