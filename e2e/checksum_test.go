package e2e

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	"gotest.tools/v3/fs"
	"gotest.tools/v3/icmd"
)

// randomContent returns n bytes of content that does not repeat at the part
// boundaries, so a part that is sent in the wrong place changes the checksum.
func randomContent(n int) string {
	r := rand.New(rand.NewSource(int64(n)))
	b := make([]byte, n)
	r.Read(b)
	return string(b)
}

func TestCopyUploadWithChecksumAlgorithm(t *testing.T) {
	t.Parallel()

	for _, alg := range []string{"CRC32C", "CRC32", "SHA256", "SHA1", "crc32c"} {
		alg := alg
		t.Run(alg, func(t *testing.T) {
			t.Parallel()

			layer := newChecksumLayer()
			s3client, s5cmd := setup(t, withChecksumLayer(layer))
			bucket := s3BucketFromTestName(t)
			createBucket(t, s3client, bucket)

			// 12 MiB with 5 MiB parts is a multipart upload of three parts.
			big := randomContent(12 << 20)
			workdir := fs.NewDir(t, bucket, fs.WithFile("small", "small content"), fs.WithFile("big", big))
			defer workdir.Remove()

			for _, name := range []string{"small", "big"} {
				result := icmd.RunCmd(s5cmd("cp", "--checksum-algorithm", alg, "--part-size", "5",
					filepath.ToSlash(workdir.Join(name)), fmt.Sprintf("s3://%v/", bucket)))
				result.Assert(t, icmd.Success)
			}

			assert.Assert(t, ensureS3Object(s3client, bucket, "small", "small content"))
			assert.Assert(t, ensureS3Object(s3client, bucket, "big", big))

			verified, rejected := layer.counts()
			upper := strings.ToUpper(alg)
			assert.Equal(t, rejected, 0)
			assert.Equal(t, verified["PutObject:"+upper], 1, "%v", verified)
			assert.Equal(t, verified["UploadPart:"+upper], 3, "%v", verified)
			assert.Equal(t, verified["CompleteMultipartUpload:"+upper], 1, "%v", verified)
		})
	}
}

func TestCopyUploadWithoutChecksumAlgorithmSendsNone(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	workdir := fs.NewDir(t, bucket, fs.WithFile("file", "content"))
	defer workdir.Remove()

	result := icmd.RunCmd(s5cmd("cp", filepath.ToSlash(workdir.Join("file")), fmt.Sprintf("s3://%v/", bucket)))
	result.Assert(t, icmd.Success)

	verified, _ := layer.counts()
	assert.Equal(t, len(verified), 0, "%v", verified)
}

func TestCopyChecksumAlgorithmValidation(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	putFile(t, s3client, bucket, "object", "content")

	workdir := fs.NewDir(t, bucket, fs.WithFile("file", "content"))
	defer workdir.Remove()
	file := filepath.ToSlash(workdir.Join("file"))
	dst := fmt.Sprintf("s3://%v/", bucket)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown algorithm", []string{"cp", "--checksum-algorithm", "md5", file, dst}, `invalid checksum algorithm "md5"`},
		{"crc64nvme", []string{"cp", "--checksum-algorithm", "CRC64NVME", file, dst}, "CRC64NVME is not supported"},
		{"download", []string{"cp", "--checksum-algorithm", "CRC32C", fmt.Sprintf("s3://%v/object", bucket), filepath.ToSlash(workdir.Path())}, "only applies to uploads"},
		{"remote copy", []string{"cp", "--checksum-algorithm", "CRC32C", fmt.Sprintf("s3://%v/object", bucket), fmt.Sprintf("s3://%v/copy", bucket)}, "only applies to uploads"},
	}
	for _, tc := range tests {
		result := icmd.RunCmd(s5cmd(tc.args...))
		result.Assert(t, icmd.Expected{ExitCode: 1})
		assert.Assert(t, strings.Contains(result.Stderr(), tc.want), "%v: %q", tc.name, result.Stderr())
	}
}

func TestSyncUploadWithChecksumAlgorithm(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	big := randomContent(6 << 20)
	workdir := fs.NewDir(t, bucket, fs.WithFile("a.txt", "aaa"), fs.WithFile("b.txt", "bbb"), fs.WithFile("big.bin", big))
	defer workdir.Remove()

	result := icmd.RunCmd(s5cmd("sync", "--checksum-algorithm", "SHA256", "--part-size", "5",
		filepath.ToSlash(workdir.Path())+"/", fmt.Sprintf("s3://%v/", bucket)))
	result.Assert(t, icmd.Success)

	assert.Assert(t, ensureS3Object(s3client, bucket, "a.txt", "aaa"))
	assert.Assert(t, ensureS3Object(s3client, bucket, "big.bin", big))

	verified, rejected := layer.counts()
	assert.Equal(t, rejected, 0)
	assert.Equal(t, verified["PutObject:SHA256"], 2, "%v", verified)
	assert.Equal(t, verified["UploadPart:SHA256"], 2, "%v", verified)
}
