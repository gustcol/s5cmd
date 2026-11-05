package storage

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
)

// ChecksumAlgorithm names a checksum that S3 can store with an object and
// verify on upload.
type ChecksumAlgorithm string

// The checksum algorithms that can be used with --checksum-algorithm.
// CRC64NVME is not supported: the AWS SDK for Go v1 has no support for it
// (no request field, no enum value, no way to complete a multipart upload
// with a full object CRC64NVME).
const (
	ChecksumCRC32C ChecksumAlgorithm = s3.ChecksumAlgorithmCrc32c
	ChecksumCRC32  ChecksumAlgorithm = s3.ChecksumAlgorithmCrc32
	ChecksumSHA256 ChecksumAlgorithm = s3.ChecksumAlgorithmSha256
	ChecksumSHA1   ChecksumAlgorithm = s3.ChecksumAlgorithmSha1
)

// ChecksumAlgorithms lists the supported algorithms in the order that is used
// to pick one when an object has more than one checksum (strongest first).
var ChecksumAlgorithms = []ChecksumAlgorithm{
	ChecksumSHA256,
	ChecksumSHA1,
	ChecksumCRC32C,
	ChecksumCRC32,
}

// ParseChecksumAlgorithm converts a user supplied name, in any letter case,
// to a ChecksumAlgorithm. An empty string is valid and means no checksum.
func ParseChecksumAlgorithm(name string) (ChecksumAlgorithm, error) {
	if name == "" {
		return "", nil
	}
	upper := strings.ToUpper(name)
	for _, a := range ChecksumAlgorithms {
		if string(a) == upper {
			return a, nil
		}
	}
	if upper == "CRC64NVME" {
		return "", fmt.Errorf("checksum algorithm CRC64NVME is not supported, use one of CRC32C, CRC32, SHA256 or SHA1")
	}
	return "", fmt.Errorf("invalid checksum algorithm %q, use one of CRC32C, CRC32, SHA256 or SHA1", name)
}

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// newHash returns a new hash for the algorithm. Its Sum is the big endian
// digest that S3 base64 encodes.
func (a ChecksumAlgorithm) newHash() hash.Hash {
	switch a {
	case ChecksumCRC32C:
		return crc32.New(crc32cTable)
	case ChecksumCRC32:
		return crc32.NewIEEE()
	case ChecksumSHA256:
		return sha256.New()
	case ChecksumSHA1:
		return sha1.New()
	}
	return nil
}

// ObjectChecksum holds one checksum value of an object, as reported by S3.
type ObjectChecksum struct {
	Algorithm ChecksumAlgorithm
	// Value is the base64 digest. For a multipart upload it is the checksum
	// of the part checksums followed by "-<number of parts>".
	Value string
}

// Composite reports whether the value is a checksum of part checksums.
func (c ObjectChecksum) Composite() bool {
	return strings.Contains(c.Value, "-")
}

// Parts returns the number of parts of a composite checksum, or 0.
func (c ObjectChecksum) Parts() int64 {
	i := strings.LastIndexByte(c.Value, '-')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(c.Value[i+1:], 10, 64)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// ctxReader stops reading when the context is done, so that hashing a big
// file does not keep a cancelled command alive.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// HashReader reads r until EOF and returns the base64 encoded checksum.
func HashReader(ctx context.Context, alg ChecksumAlgorithm, r io.Reader) (string, error) {
	h := alg.newHash()
	if h == nil {
		return "", fmt.Errorf("unknown checksum algorithm %q", alg)
	}
	if _, err := io.Copy(h, ctxReader{ctx, r}); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

// HashFile returns the base64 encoded checksum of the file at path.
func HashFile(ctx context.Context, alg ChecksumAlgorithm, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return HashReader(ctx, alg, f)
}

// partDigests hashes the file in consecutive parts of the given sizes and
// returns the raw digest of each part. It fails if the file is shorter than
// the sum of the sizes.
func partDigests(ctx context.Context, newHash func() hash.Hash, path string, sizes []int64) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	digests := make([][]byte, 0, len(sizes))
	for _, size := range sizes {
		h := newHash()
		n, err := io.Copy(h, ctxReader{ctx, io.LimitReader(f, size)})
		if err != nil {
			return nil, err
		}
		if n != size {
			return nil, io.ErrUnexpectedEOF
		}
		digests = append(digests, h.Sum(nil))
	}
	return digests, nil
}

// uniformPartSizes splits size in parts of partSize bytes, the last one
// holding the remainder. It returns nil when that does not give exactly
// parts parts.
func uniformPartSizes(size, partSize, parts int64) []int64 {
	if partSize <= 0 || parts < 1 || size < 0 {
		return nil
	}
	if (size+partSize-1)/partSize != parts && !(size == 0 && parts == 1) {
		return nil
	}
	sizes := make([]int64, 0, parts)
	left := size
	for i := int64(0); i < parts; i++ {
		n := partSize
		if left < n {
			n = left
		}
		sizes = append(sizes, n)
		left -= n
	}
	return sizes
}

// compositeValue combines raw part digests the way S3 does for a multipart
// upload: the digest of all part digests, followed by the part count.
func compositeValue(newHash func() hash.Hash, digests [][]byte) string {
	h := newHash()
	for _, d := range digests {
		h.Write(d)
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(digests))
}

// multipartETag computes the ETag S3 gives to a multipart upload: the MD5 of
// the concatenated MD5 digests of the parts, followed by the part count.
func multipartETag(ctx context.Context, path string, sizes []int64) (string, error) {
	digests, err := partDigests(ctx, md5.New, path, sizes)
	if err != nil {
		return "", err
	}
	h := md5.New()
	for _, d := range digests {
		h.Write(d)
	}
	return hex.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(digests)), nil
}

// md5File returns the hex MD5 of the file.
func md5File(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// uncountedReaderAt is implemented by readers that count the bytes that are
// read from them for a progress bar. Hashing reads must not be counted as
// transferred bytes.
type uncountedReaderAt interface {
	ReadAtNoProgress(p []byte, off int64) (int, error)
}

type uncountedSectionReader struct {
	r uncountedReaderAt
}

func (u uncountedSectionReader) ReadAt(p []byte, off int64) (int, error) {
	return u.r.ReadAtNoProgress(p, off)
}

// hashBody returns the base64 checksum of everything in body and leaves it
// positioned at the start again.
func hashBody(alg ChecksumAlgorithm, body io.ReadSeeker) (string, error) {
	var src io.Reader = body
	if sr, ok := body.(*io.SectionReader); ok {
		if outer, off, n := sr.Outer(); outer != nil {
			if u, ok := outer.(uncountedReaderAt); ok {
				src = io.NewSectionReader(uncountedSectionReader{u}, off, n)
			}
		}
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	sum, err := HashReader(context.Background(), alg, src)
	if err != nil {
		return "", err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return sum, nil
}

// uploadChecksumOption returns a request option for s3manager that makes the
// uploader send checksums. The SDK does not compute them: the AWS SDK for Go
// v1 only passes through values that are set on the input, and ignores them
// for multipart uploads. The hashing is done here, in the request handlers,
// so each part is hashed by the goroutine that uploads it and the parts of
// one object are hashed in parallel.
//
// For a single part upload the checksum goes in the PutObject request. For a
// multipart upload every UploadPart request carries the checksum of its part
// and the parts are listed with their checksums when the upload is completed.
func uploadChecksumOption(alg ChecksumAlgorithm) request.Option {
	var (
		mu    sync.Mutex
		parts = map[int64]string{}
	)

	return func(r *request.Request) {
		switch r.Operation.Name {
		case "PutObject":
			r.Handlers.Build.PushFront(func(r *request.Request) {
				in, ok := r.Params.(*s3.PutObjectInput)
				if !ok || in.Body == nil {
					return
				}
				sum, err := hashBody(alg, in.Body)
				if err != nil {
					r.Error = err
					return
				}
				setPutChecksum(in, alg, sum)
			})
		case "UploadPart":
			r.Handlers.Build.PushFront(func(r *request.Request) {
				in, ok := r.Params.(*s3.UploadPartInput)
				if !ok || in.Body == nil || in.PartNumber == nil {
					return
				}
				sum, err := hashBody(alg, in.Body)
				if err != nil {
					r.Error = err
					return
				}
				setPartChecksum(in, alg, sum)
				mu.Lock()
				parts[*in.PartNumber] = sum
				mu.Unlock()
			})
		case "CompleteMultipartUpload":
			r.Handlers.Build.PushFront(func(r *request.Request) {
				in, ok := r.Params.(*s3.CompleteMultipartUploadInput)
				if !ok || in.MultipartUpload == nil {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, p := range in.MultipartUpload.Parts {
					if p.PartNumber == nil {
						continue
					}
					if sum, ok := parts[*p.PartNumber]; ok {
						setCompletedPartChecksum(p, alg, sum)
					}
				}
			})
		}
	}
}

func setPutChecksum(in *s3.PutObjectInput, alg ChecksumAlgorithm, sum string) {
	switch alg {
	case ChecksumCRC32C:
		in.ChecksumCRC32C = &sum
	case ChecksumCRC32:
		in.ChecksumCRC32 = &sum
	case ChecksumSHA256:
		in.ChecksumSHA256 = &sum
	case ChecksumSHA1:
		in.ChecksumSHA1 = &sum
	}
}

func setPartChecksum(in *s3.UploadPartInput, alg ChecksumAlgorithm, sum string) {
	switch alg {
	case ChecksumCRC32C:
		in.ChecksumCRC32C = &sum
	case ChecksumCRC32:
		in.ChecksumCRC32 = &sum
	case ChecksumSHA256:
		in.ChecksumSHA256 = &sum
	case ChecksumSHA1:
		in.ChecksumSHA1 = &sum
	}
}

func setCompletedPartChecksum(p *s3.CompletedPart, alg ChecksumAlgorithm, sum string) {
	switch alg {
	case ChecksumCRC32C:
		p.ChecksumCRC32C = &sum
	case ChecksumCRC32:
		p.ChecksumCRC32 = &sum
	case ChecksumSHA256:
		p.ChecksumSHA256 = &sum
	case ChecksumSHA1:
		p.ChecksumSHA1 = &sum
	}
}
