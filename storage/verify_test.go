package storage

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"gotest.tools/v3/assert"

	"github.com/peak/s5cmd/v2/storage/url"
)

// fakeObject answers HeadIntegrity like S3 does for an object that was
// uploaded in parts.
type fakeObject struct {
	info      Integrity
	partSizes []int64
	partSums  [][]ObjectChecksum
	heads     []int64
	noParts   bool // the server ignores part numbers
}

func (f *fakeObject) HeadIntegrity(_ context.Context, _ *url.URL, n int64) (*Integrity, error) {
	f.heads = append(f.heads, n)
	if n == 0 || f.noParts || len(f.partSizes) == 0 {
		cp := f.info
		return &cp, nil
	}
	cp := Integrity{
		Size:       f.partSizes[n-1],
		ETag:       f.info.ETag,
		PartsCount: int64(len(f.partSizes)),
	}
	if int(n-1) < len(f.partSums) {
		cp.Checksums = f.partSums[n-1]
	}
	return &cp, nil
}

func writeTemp(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	assert.NilError(t, os.WriteFile(p, []byte(data), 0o644))
	return p
}

func hexMD5(s string) string {
	d := md5.Sum([]byte(s))
	return hex.EncodeToString(d[:])
}

func sumOf(t *testing.T, alg ChecksumAlgorithm, s string) string {
	t.Helper()
	v, err := HashReader(context.Background(), alg, strings.NewReader(s))
	assert.NilError(t, err)
	return v
}

func compare(t *testing.T, f *fakeObject, data string, opts CompareOptions) Comparison {
	t.Helper()
	u, _ := url.New("s3://bucket/key")
	c, err := CompareFile(context.Background(), f, u, writeTemp(t, data), opts)
	assert.NilError(t, err)
	return c
}

func TestCompareFileSize(t *testing.T) {
	f := &fakeObject{info: Integrity{Size: 3, ETag: hexMD5("abc")}}
	c := compare(t, f, "abcd", CompareOptions{})
	assert.Equal(t, c.Result, CompareDifferent)
	assert.Equal(t, c.Method, "size")
}

func TestCompareFileSingleChecksum(t *testing.T) {
	for _, alg := range ChecksumAlgorithms {
		f := &fakeObject{info: Integrity{Size: 5, ETag: "irrelevant", Checksums: []ObjectChecksum{{alg, sumOf(t, alg, "hello")}}}}
		c := compare(t, f, "hello", CompareOptions{})
		assert.Equal(t, c.Result, CompareEqual, string(alg))
		assert.Equal(t, c.Method, string(alg))

		c = compare(t, f, "hellp", CompareOptions{})
		assert.Equal(t, c.Result, CompareDifferent, string(alg))
		assert.Assert(t, strings.Contains(c.Detail, "the object has"), c.Detail)
	}
}

func TestCompareFilePrefersStrongestChecksum(t *testing.T) {
	// the CRC32C is wrong on purpose, SHA256 must be the one that is used.
	f := &fakeObject{info: Integrity{Size: 5, Checksums: []ObjectChecksum{
		{ChecksumCRC32C, sumOf(t, ChecksumCRC32C, "other")},
		{ChecksumSHA256, sumOf(t, ChecksumSHA256, "hello")},
	}}}
	c := compare(t, f, "hello", CompareOptions{})
	assert.Equal(t, c.Result, CompareEqual)
	assert.Equal(t, c.Method, "SHA256")
}

func TestCompareFileMD5ETag(t *testing.T) {
	f := &fakeObject{info: Integrity{Size: 5, ETag: hexMD5("hello")}}
	assert.Equal(t, compare(t, f, "hello", CompareOptions{}).Result, CompareEqual)

	c := compare(t, f, "hellp", CompareOptions{})
	assert.Equal(t, c.Result, CompareDifferent)
	assert.Equal(t, c.Method, "MD5 ETag")

	// uppercase ETags are the same ETags.
	f.info.ETag = strings.ToUpper(hexMD5("hello"))
	assert.Equal(t, compare(t, f, "hello", CompareOptions{}).Result, CompareEqual)
}

func TestCompareFileETagThatIsNotMD5(t *testing.T) {
	tests := []struct {
		name string
		info Integrity
	}{
		{"kms", Integrity{Size: 5, ETag: hexMD5("hello"), ServerSideEncryption: "aws:kms"}},
		{"kms dsse", Integrity{Size: 5, ETag: hexMD5("hello"), ServerSideEncryption: "aws:kms:dsse"}},
		{"customer key", Integrity{Size: 5, ETag: hexMD5("hello"), SSECustomer: true}},
		{"not hex", Integrity{Size: 5, ETag: "not-an-md5"}},
		{"empty", Integrity{Size: 5}},
	}
	for _, tc := range tests {
		f := &fakeObject{info: tc.info}
		// the content is different on purpose: it must not be reported as such.
		c := compare(t, f, "hellp", CompareOptions{})
		assert.Equal(t, c.Result, CompareUnknown, tc.name)
		assert.Assert(t, c.Detail != "", tc.name)
	}
}

// multipart builds a fake object for data uploaded in parts of partSize bytes.
func multipart(t *testing.T, data string, partSize int, alg ChecksumAlgorithm, noParts bool) *fakeObject {
	t.Helper()
	f := &fakeObject{noParts: noParts}
	var raw [][]byte
	var etagInput []byte
	for off := 0; off < len(data); off += partSize {
		end := off + partSize
		if end > len(data) {
			end = len(data)
		}
		part := data[off:end]
		f.partSizes = append(f.partSizes, int64(len(part)))
		if alg != "" {
			f.partSums = append(f.partSums, []ObjectChecksum{{alg, sumOf(t, alg, part)}})
			h := alg.newHash()
			h.Write([]byte(part))
			raw = append(raw, h.Sum(nil))
		}
		d := md5.Sum([]byte(part))
		etagInput = append(etagInput, d[:]...)
	}
	n := len(f.partSizes)
	d := md5.Sum(etagInput)
	f.info = Integrity{Size: int64(len(data)), ETag: fmt.Sprintf("%s-%d", hex.EncodeToString(d[:]), n)}
	if alg != "" {
		f.info.Checksums = []ObjectChecksum{{alg, compositeValue(alg.newHash, raw)}}
	}
	return f
}

func TestCompareFileCompositeChecksum(t *testing.T) {
	data := strings.Repeat("0123456789", 25) // 250 bytes
	f := multipart(t, data, 100, ChecksumCRC32C, false)

	c := compare(t, f, data, CompareOptions{})
	assert.Equal(t, c.Result, CompareEqual)
	assert.Equal(t, c.Method, "CRC32C of parts")
	assert.DeepEqual(t, f.heads, []int64{0, 1}) // no per part requests needed

	// a changed byte does not match, the parts are checked one by one and
	// the part is named.
	bad := data[:150] + "X" + data[151:]
	f = multipart(t, data, 100, ChecksumCRC32C, false)
	c = compare(t, f, bad, CompareOptions{})
	assert.Equal(t, c.Result, CompareDifferent)
	assert.DeepEqual(t, f.heads, []int64{0, 1, 2, 3}) // part 1 is not asked twice
	assert.Assert(t, strings.HasPrefix(c.Detail, "part 2:"), c.Detail)
}

func TestCompareFileCompositeIrregularParts(t *testing.T) {
	data := "aaaaabbbbbbbbbcc"
	f := &fakeObject{partSizes: []int64{5, 9, 2}}
	var raw [][]byte
	off := 0
	for _, n := range f.partSizes {
		part := data[off : off+int(n)]
		off += int(n)
		f.partSums = append(f.partSums, []ObjectChecksum{{ChecksumSHA256, sumOf(t, ChecksumSHA256, part)}})
		h := ChecksumSHA256.newHash()
		h.Write([]byte(part))
		raw = append(raw, h.Sum(nil))
	}
	f.info = Integrity{Size: int64(len(data)), Checksums: []ObjectChecksum{{ChecksumSHA256, compositeValue(ChecksumSHA256.newHash, raw)}}}

	// parts of different sizes are not reproduced from equal parts, so every
	// part is checked.
	c := compare(t, f, data, CompareOptions{})
	assert.Equal(t, c.Result, CompareEqual)
	assert.DeepEqual(t, f.heads[len(f.heads)-3:], []int64{1, 2, 3})
}

func TestCompareFileCompositeWithoutPartSupport(t *testing.T) {
	data := strings.Repeat("x", 250)
	f := multipart(t, data, 100, ChecksumCRC32, true)

	// the server cannot report part sizes: the configured part size is used.
	c := compare(t, f, data, CompareOptions{PartSizes: []int64{100}})
	assert.Equal(t, c.Result, CompareEqual)

	// without the right hint it is unknown, and not different.
	c = compare(t, f, data, CompareOptions{PartSizes: []int64{70}})
	assert.Equal(t, c.Result, CompareUnknown)
	c = compare(t, f, data, CompareOptions{})
	assert.Equal(t, c.Result, CompareUnknown)
}

func TestCompareFileMultipartETag(t *testing.T) {
	data := strings.Repeat("0123456789", 25)
	f := multipart(t, data, 100, "", false)

	c := compare(t, f, data, CompareOptions{})
	assert.Equal(t, c.Result, CompareEqual)
	assert.Equal(t, c.Method, "multipart ETag")

	// the server tells the size of every part, so the layout is known and
	// a mismatch is a real one.
	bad := "X" + data[1:]
	c = compare(t, f, bad, CompareOptions{})
	assert.Equal(t, c.Result, CompareDifferent)

	// The part size from the configuration is used when the server does not
	// report part sizes.
	f = multipart(t, data, 100, "", true)
	assert.Equal(t, compare(t, f, data, CompareOptions{PartSizes: []int64{100}}).Result, CompareEqual)
	c = compare(t, f, data, CompareOptions{})
	assert.Equal(t, c.Result, CompareUnknown)
	assert.Assert(t, strings.Contains(c.Detail, "part size is not known"), c.Detail)

	// and a server that ignores part numbers never gives a definite answer
	// for a layout it cannot see.
	c = compare(t, f, "X"+data[1:], CompareOptions{PartSizes: []int64{100}})
	assert.Equal(t, c.Result, CompareUnknown)
}

func TestCompareFileMultipartETagIrregularParts(t *testing.T) {
	data := "aaaaabbbbbbbbbcc"
	f := &fakeObject{partSizes: []int64{5, 9, 2}}
	var etagInput []byte
	off := 0
	for _, n := range f.partSizes {
		d := md5.Sum([]byte(data[off : off+int(n)]))
		off += int(n)
		etagInput = append(etagInput, d[:]...)
	}
	d := md5.Sum(etagInput)
	f.info = Integrity{Size: int64(len(data)), ETag: hex.EncodeToString(d[:]) + "-3"}

	assert.Equal(t, compare(t, f, data, CompareOptions{}).Result, CompareEqual)
	assert.Equal(t, compare(t, f, "X"+data[1:], CompareOptions{}).Result, CompareDifferent)
}

func TestCompareFileEmpty(t *testing.T) {
	f := &fakeObject{info: Integrity{Size: 0, ETag: hexMD5("")}}
	assert.Equal(t, compare(t, f, "", CompareOptions{}).Result, CompareEqual)
}

// TestHeadIntegrityParsesHeaders checks that S3.HeadIntegrity asks for the
// checksums and reads them from the response.
func TestHeadIntegrityParsesHeaders(t *testing.T) {
	var gotMode, gotPart string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMode = r.Header.Get("x-amz-checksum-mode")
		gotPart = r.URL.Query().Get("partNumber")
		w.Header().Set("Content-Length", "42")
		w.Header().Set("ETag", `"abc-3"`)
		w.Header().Set("x-amz-checksum-crc32c", "yZRlqg==-3")
		w.Header().Set("x-amz-checksum-sha256", "c2hh")
		w.Header().Set("x-amz-mp-parts-count", "3")
		w.Header().Set("x-amz-server-side-encryption", "aws:kms")
	}))
	defer srv.Close()

	sess, err := session.NewSession(aws.NewConfig().WithEndpoint(srv.URL).WithRegion("us-east-1").
		WithS3ForcePathStyle(true).WithMaxRetries(0).
		WithCredentials(credentials.NewStaticCredentials("id", "key", "")))
	assert.NilError(t, err)
	st := &S3{api: s3.New(sess)}
	u, _ := url.New("s3://bucket/key")

	info, err := st.HeadIntegrity(context.Background(), u, 2)
	assert.NilError(t, err)
	assert.Equal(t, gotMode, "ENABLED")
	assert.Equal(t, gotPart, strconv.Itoa(2))
	assert.Equal(t, info.Size, int64(42))
	assert.Equal(t, info.ETag, "abc-3")
	assert.Equal(t, info.PartsCount, int64(3))
	assert.Equal(t, info.ServerSideEncryption, "aws:kms")
	assert.DeepEqual(t, info.Checksums, []ObjectChecksum{
		{ChecksumCRC32C, "yZRlqg==-3"},
		{ChecksumSHA256, "c2hh"},
	})

	_, err = st.HeadIntegrity(context.Background(), u, 0)
	assert.NilError(t, err)
	assert.Equal(t, gotPart, "")
}

// A server that cannot report the part layout gives an unknown result for a
// mismatch, unless the caller says that a wrong "different" is acceptable.
func TestCompareFileAssumeLayout(t *testing.T) {
	data := strings.Repeat("x", 250)
	bad := "y" + data[1:]

	f := multipart(t, data, 100, ChecksumCRC32, true)
	assert.Equal(t, compare(t, f, bad, CompareOptions{PartSizes: []int64{100}}).Result, CompareUnknown)
	assert.Equal(t, compare(t, f, bad, CompareOptions{PartSizes: []int64{100}, AssumeLayout: true}).Result, CompareDifferent)
	assert.Equal(t, compare(t, f, data, CompareOptions{PartSizes: []int64{100}, AssumeLayout: true}).Result, CompareEqual)
	// a part size that does not give the number of parts of the object is no basis for a verdict.
	assert.Equal(t, compare(t, f, bad, CompareOptions{PartSizes: []int64{70}, AssumeLayout: true}).Result, CompareUnknown)

	e := multipart(t, data, 100, "", true)
	assert.Equal(t, compare(t, e, bad, CompareOptions{PartSizes: []int64{100}}).Result, CompareUnknown)
	assert.Equal(t, compare(t, e, bad, CompareOptions{PartSizes: []int64{100}, AssumeLayout: true}).Result, CompareDifferent)
	assert.Equal(t, compare(t, e, data, CompareOptions{PartSizes: []int64{100}, AssumeLayout: true}).Result, CompareEqual)
	assert.Equal(t, compare(t, e, bad, CompareOptions{AssumeLayout: true}).Result, CompareUnknown)
}
