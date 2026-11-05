package e2e

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/fs"
	"gotest.tools/v3/icmd"

	"github.com/peak/s5cmd/v2/storage"
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

func downloadedFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	assert.NilError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCopyDownloadVerify(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		algorithm string // used for the upload, empty for none
		size      int
		partSize  string
	}{
		{"single part with checksum", "CRC32C", 1 << 20, "5"},
		{"single part with sha256", "SHA256", 1 << 20, "5"},
		{"multipart with checksum", "CRC32C", 12 << 20, "5"},
		{"single part ETag", "", 1 << 20, "5"},
		{"multipart ETag", "", 12 << 20, "5"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			layer := newChecksumLayer()
			s3client, s5cmd := setup(t, withChecksumLayer(layer))
			bucket := s3BucketFromTestName(t)
			createBucket(t, s3client, bucket)

			content := randomContent(tc.size)
			workdir := fs.NewDir(t, bucket, fs.WithFile("obj", content), fs.WithDir("out"))
			defer workdir.Remove()

			up := []string{"cp", "--part-size", tc.partSize}
			if tc.algorithm != "" {
				up = append(up, "--checksum-algorithm", tc.algorithm)
			}
			up = append(up, filepath.ToSlash(workdir.Join("obj")), fmt.Sprintf("s3://%v/obj", bucket))
			icmd.RunCmd(s5cmd(up...)).Assert(t, icmd.Success)

			dst := filepath.ToSlash(workdir.Join("out")) + "/"
			result := icmd.RunCmd(s5cmd("--log", "debug", "cp", "--verify", "--part-size", tc.partSize,
				fmt.Sprintf("s3://%v/obj", bucket), dst))
			result.Assert(t, icmd.Success)
			assert.Assert(t, strings.Contains(result.Stderr(), "verified with"), result.Stderr())
			assert.Assert(t, !strings.Contains(result.Stderr(), "cannot verify"), result.Stderr())

			got, err := os.ReadFile(workdir.Join("out", "obj"))
			assert.NilError(t, err)
			assert.Assert(t, string(got) == content)
		})
	}
}

func TestCopyDownloadVerifyFailureRemovesFile(t *testing.T) {
	t.Parallel()

	for _, alg := range []string{"", "CRC32C"} {
		for _, size := range []int{1 << 20, 12 << 20} {
			alg, size := alg, size
			t.Run(fmt.Sprintf("alg=%q size=%d", alg, size), func(t *testing.T) {
				t.Parallel()

				layer := newChecksumLayer()
				s3client, s5cmd := setup(t, withChecksumLayer(layer))
				bucket := s3BucketFromTestName(t)
				createBucket(t, s3client, bucket)

				workdir := fs.NewDir(t, bucket, fs.WithFile("obj", randomContent(size)), fs.WithDir("out"))
				defer workdir.Remove()

				up := []string{"cp", "--part-size", "5"}
				if alg != "" {
					up = append(up, "--checksum-algorithm", alg)
				}
				up = append(up, filepath.ToSlash(workdir.Join("obj")), fmt.Sprintf("s3://%v/obj", bucket))
				icmd.RunCmd(s5cmd(up...)).Assert(t, icmd.Success)

				layer.corruptDownloads(true)

				// without --verify the bad data is kept, there is nothing to tell.
				result := icmd.RunCmd(s5cmd("cp", "--part-size", "5", fmt.Sprintf("s3://%v/obj", bucket),
					filepath.ToSlash(workdir.Join("out"))+"/"))
				result.Assert(t, icmd.Success)
				assert.NilError(t, os.Remove(workdir.Join("out", "obj")))

				result = icmd.RunCmd(s5cmd("cp", "--verify", "--part-size", "5", fmt.Sprintf("s3://%v/obj", bucket),
					filepath.ToSlash(workdir.Join("out"))+"/"))
				result.Assert(t, icmd.Expected{ExitCode: 1})
				assert.Assert(t, strings.Contains(result.Stderr(), "verify failed"), result.Stderr())

				// neither the file nor a temporary file is left.
				assert.DeepEqual(t, downloadedFiles(t, workdir.Join("out")), []string(nil))
			})
		}
	}
}

func TestCopyDownloadVerifyKeepsExistingFileOnFailure(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	workdir := fs.NewDir(t, bucket, fs.WithFile("obj", "new content"), fs.WithFile("out.txt", "old content"))
	defer workdir.Remove()

	icmd.RunCmd(s5cmd("cp", "--checksum-algorithm", "CRC32C", filepath.ToSlash(workdir.Join("obj")),
		fmt.Sprintf("s3://%v/obj", bucket))).Assert(t, icmd.Success)
	layer.corruptDownloads(true)

	result := icmd.RunCmd(s5cmd("cp", "--verify", fmt.Sprintf("s3://%v/obj", bucket), filepath.ToSlash(workdir.Join("out.txt"))))
	result.Assert(t, icmd.Expected{ExitCode: 1})

	got, err := os.ReadFile(workdir.Join("out.txt"))
	assert.NilError(t, err)
	assert.Equal(t, string(got), "old content")
}

func TestMoveDownloadVerifyFailureKeepsSource(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	putFile(t, s3client, bucket, "obj", "some content")
	layer.corruptDownloads(true)

	workdir := fs.NewDir(t, bucket)
	defer workdir.Remove()

	result := icmd.RunCmd(s5cmd("mv", "--verify", fmt.Sprintf("s3://%v/obj", bucket), filepath.ToSlash(workdir.Path())+"/"))
	result.Assert(t, icmd.Expected{ExitCode: 1})

	layer.corruptDownloads(false)
	assert.Assert(t, ensureS3Object(s3client, bucket, "obj", "some content"))
	assert.DeepEqual(t, downloadedFiles(t, workdir.Path()), []string(nil))
}

func TestCopyDownloadVerifyCannotVerify(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	layer.setSSEKMS()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	workdir := fs.NewDir(t, bucket, fs.WithFile("obj", "content"), fs.WithDir("out"))
	defer workdir.Remove()
	icmd.RunCmd(s5cmd("cp", filepath.ToSlash(workdir.Join("obj")), fmt.Sprintf("s3://%v/obj", bucket))).Assert(t, icmd.Success)

	// the ETag of a KMS object is not an MD5 and there is no checksum: it is
	// reported, but it is not an error.
	result := icmd.RunCmd(s5cmd("cp", "--verify", fmt.Sprintf("s3://%v/obj", bucket), filepath.ToSlash(workdir.Join("out"))+"/"))
	result.Assert(t, icmd.Success)
	assert.Assert(t, strings.Contains(result.Stderr(), "cannot verify the download"), result.Stderr())
	assert.DeepEqual(t, downloadedFiles(t, workdir.Join("out")), []string{"obj"})
}

// TestCopyDownloadVerifyIrregularParts uploads an object in parts of
// different sizes, which s5cmd never does, and checks that the download is
// still verified, part by part.
func TestCopyDownloadVerifyIrregularParts(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	const mib = 1 << 20
	content := randomContent(5*mib + 6*mib + mib)
	sizes := []int{5 * mib, 6 * mib, mib}

	create, err := s3client.CreateMultipartUpload(&s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String("obj"), ChecksumAlgorithm: aws.String("CRC32C"),
	})
	assert.NilError(t, err)
	var parts []*s3.CompletedPart
	off := 0
	for i, n := range sizes {
		body := []byte(content[off : off+n])
		off += n
		sum, err := storage.HashReader(context.Background(), storage.ChecksumCRC32C, bytes.NewReader(body))
		assert.NilError(t, err)
		out, err := s3client.UploadPart(&s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String("obj"), UploadId: create.UploadId,
			PartNumber: aws.Int64(int64(i + 1)), Body: bytes.NewReader(body), ChecksumCRC32C: aws.String(sum),
		})
		assert.NilError(t, err)
		parts = append(parts, &s3.CompletedPart{ETag: out.ETag, PartNumber: aws.Int64(int64(i + 1)), ChecksumCRC32C: aws.String(sum)})
	}
	_, err = s3client.CompleteMultipartUpload(&s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String("obj"), UploadId: create.UploadId,
		MultipartUpload: &s3.CompletedMultipartUpload{Parts: parts},
	})
	assert.NilError(t, err)

	workdir := fs.NewDir(t, bucket, fs.WithDir("out"))
	defer workdir.Remove()
	dst := filepath.ToSlash(workdir.Join("out")) + "/"

	result := icmd.RunCmd(s5cmd("--log", "debug", "cp", "--verify", fmt.Sprintf("s3://%v/obj", bucket), dst))
	result.Assert(t, icmd.Success)
	assert.Assert(t, strings.Contains(result.Stderr(), "verified with CRC32C of parts"), result.Stderr())
	assert.Assert(t, layer.partHeadCount() >= 3, "part heads: %d", layer.partHeadCount())

	layer.corruptDownloads(true)
	assert.NilError(t, os.Remove(workdir.Join("out", "obj")))
	result = icmd.RunCmd(s5cmd("cp", "--verify", fmt.Sprintf("s3://%v/obj", bucket), dst))
	result.Assert(t, icmd.Expected{ExitCode: 1})
	assert.Assert(t, strings.Contains(result.Stderr(), "part 1:"), result.Stderr())
}

func TestSyncDownloadVerify(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	putFile(t, s3client, bucket, "a.txt", "aaa")
	putFile(t, s3client, bucket, "dir/b.txt", "bbb")

	workdir := fs.NewDir(t, bucket)
	defer workdir.Remove()
	dst := filepath.ToSlash(workdir.Path()) + "/"

	icmd.RunCmd(s5cmd("sync", "--verify", fmt.Sprintf("s3://%v/*", bucket), dst)).Assert(t, icmd.Success)
	assert.DeepEqual(t, downloadedFiles(t, workdir.Path()), []string{"a.txt", "dir"})

	layer.corruptDownloads(true)
	os.RemoveAll(workdir.Join("dir"))
	os.Remove(workdir.Join("a.txt"))
	result := icmd.RunCmd(s5cmd("sync", "--verify", fmt.Sprintf("s3://%v/*", bucket), dst))
	result.Assert(t, icmd.Expected{ExitCode: 1})
	assert.Assert(t, strings.Contains(result.Stderr(), "verify failed"), result.Stderr())
	assert.DeepEqual(t, downloadedFiles(t, workdir.Path()), []string{"dir"}) // dir is created empty
}

func TestVerifyOnlyAppliesToDownloads(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	workdir := fs.NewDir(t, bucket, fs.WithFile("file", "content"))
	defer workdir.Remove()

	result := icmd.RunCmd(s5cmd("cp", "--verify", filepath.ToSlash(workdir.Join("file")), fmt.Sprintf("s3://%v/", bucket)))
	result.Assert(t, icmd.Expected{ExitCode: 1})
	assert.Assert(t, strings.Contains(result.Stderr(), "--verify only applies to downloads"), result.Stderr())
}
