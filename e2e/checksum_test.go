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
	"time"

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

func TestCopyDownloadVerifyStrictCannotVerify(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	layer.setSSEKMS()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	workdir := fs.NewDir(t, bucket, fs.WithFile("obj", "content"), fs.WithDir("out"))
	defer workdir.Remove()
	icmd.RunCmd(s5cmd("cp", filepath.ToSlash(workdir.Join("obj")), fmt.Sprintf("s3://%v/obj", bucket))).Assert(t, icmd.Success)

	// nothing to compare with: --verify warns, --verify-strict fails and leaves no file.
	result := icmd.RunCmd(s5cmd("cp", "--verify-strict", fmt.Sprintf("s3://%v/obj", bucket), filepath.ToSlash(workdir.Join("out"))+"/"))
	result.Assert(t, icmd.Expected{ExitCode: 1})
	assert.Assert(t, strings.Contains(result.Stderr(), "cannot verify the download"), result.Stderr())
	assert.DeepEqual(t, downloadedFiles(t, workdir.Join("out")), []string(nil))

	// an existing destination is kept.
	assert.NilError(t, os.WriteFile(workdir.Join("out", "obj"), []byte("old"), 0o644))
	icmd.RunCmd(s5cmd("cp", "--verify-strict", fmt.Sprintf("s3://%v/obj", bucket), filepath.ToSlash(workdir.Join("out"))+"/")).Assert(t, icmd.Expected{ExitCode: 1})
	data, err := os.ReadFile(workdir.Join("out", "obj"))
	assert.NilError(t, err)
	assert.Equal(t, string(data), "old")
}

func TestMoveDownloadVerifyStrictCannotVerifyKeepsSource(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	layer.setSSEKMS()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	putFile(t, s3client, bucket, "obj", "some content")

	workdir := fs.NewDir(t, bucket)
	defer workdir.Remove()

	icmd.RunCmd(s5cmd("mv", "--verify-strict", fmt.Sprintf("s3://%v/obj", bucket), filepath.ToSlash(workdir.Path())+"/")).Assert(t, icmd.Expected{ExitCode: 1})
	assert.Assert(t, ensureS3Object(s3client, bucket, "obj", "some content"))
	assert.DeepEqual(t, downloadedFiles(t, workdir.Path()), []string(nil))
}

func TestCopyDownloadVerifyStrictSucceedsWhenVerifiable(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	workdir := fs.NewDir(t, bucket, fs.WithFile("obj", "content"), fs.WithDir("out"))
	defer workdir.Remove()
	icmd.RunCmd(s5cmd("cp", "--checksum-algorithm", "CRC32C", filepath.ToSlash(workdir.Join("obj")), fmt.Sprintf("s3://%v/obj", bucket))).Assert(t, icmd.Success)

	icmd.RunCmd(s5cmd("cp", "--verify-strict", fmt.Sprintf("s3://%v/obj", bucket), filepath.ToSlash(workdir.Join("out"))+"/")).Assert(t, icmd.Success)
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
	assert.Assert(t, strings.Contains(result.Stderr(), "--verify and --verify-strict only apply to downloads"), result.Stderr())
}

// setMtime sets the modification time of a file.
func setMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	assert.NilError(t, os.Chtimes(path, mtime, mtime))
}

// TestSyncChecksum covers the case that motivates --checksum: a file that was
// changed without changing its size, with a modification time that is not
// newer than the object.
func TestSyncChecksumUpload(t *testing.T) {
	t.Parallel()

	for _, alg := range []string{"", "CRC32C", "SHA256"} {
		alg := alg
		t.Run("algorithm="+alg, func(t *testing.T) {
			t.Parallel()

			layer := newChecksumLayer()
			s3client, s5cmd := setup(t, withChecksumLayer(layer))
			bucket := s3BucketFromTestName(t)
			createBucket(t, s3client, bucket)

			workdir := fs.NewDir(t, bucket,
				fs.WithFile("same.txt", "same content"),
				fs.WithFile("changed.txt", "aaaa"),
				fs.WithFile("touched.txt", "touched"),
				fs.WithFile("grown.txt", "short"))
			defer workdir.Remove()
			dir := filepath.ToSlash(workdir.Path()) + "/"
			dst := fmt.Sprintf("s3://%v/", bucket)

			first := []string{"sync"}
			if alg != "" {
				first = append(first, "--checksum-algorithm", alg)
			}
			icmd.RunCmd(s5cmd(append(first, dir, dst)...)).Assert(t, icmd.Success)

			// changed.txt gets other content of the same size, with an old
			// modification time; touched.txt only gets a newer one; grown.txt
			// changes size.
			old := time.Now().Add(-2 * time.Hour)
			assert.NilError(t, os.WriteFile(workdir.Join("changed.txt"), []byte("bbbb"), 0o644))
			setMtime(t, workdir.Join("changed.txt"), old)
			setMtime(t, workdir.Join("touched.txt"), time.Now().Add(time.Hour))
			assert.NilError(t, os.WriteFile(workdir.Join("grown.txt"), []byte("much longer"), 0o644))
			setMtime(t, workdir.Join("grown.txt"), old)
			setMtime(t, workdir.Join("same.txt"), old)

			// the default strategy misses the change, and copies the touched
			// file and the one that changed size.
			result := icmd.RunCmd(s5cmd("--log", "debug", "sync", dir, dst))
			result.Assert(t, icmd.Success)
			assert.Assert(t, ensureS3Object(s3client, bucket, "changed.txt", "aaaa"))
			assert.Assert(t, strings.Contains(result.Stdout(), "touched.txt"), result.Stdout())

			// reset the object of touched.txt so both runs start the same.
			putFile(t, s3client, bucket, "touched.txt", "touched")

			result = icmd.RunCmd(s5cmd("--log", "debug", "sync", "--checksum", dir, dst))
			result.Assert(t, icmd.Success)

			assert.Assert(t, ensureS3Object(s3client, bucket, "changed.txt", "bbbb"))
			assert.Assert(t, ensureS3Object(s3client, bucket, "grown.txt", "much longer"))
			assert.Assert(t, ensureS3Object(s3client, bucket, "same.txt", "same content"))
			assert.Assert(t, ensureS3Object(s3client, bucket, "touched.txt", "touched"))

			out := result.Stdout()
			assert.Assert(t, strings.Contains(out, "changed.txt"), out)
			// same content: not copied, even though touched.txt is newer.
			assert.Assert(t, !strings.Contains(out, "cp "+workdir.Join("same.txt")), out)
			assert.Assert(t, !strings.Contains(out, "cp "+workdir.Join("touched.txt")), out)
			assert.Assert(t, strings.Contains(result.Stderr(), "content is the same"), result.Stderr())
		})
	}
}

func TestSyncChecksumDownload(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	putFile(t, s3client, bucket, "changed.txt", "aaaa")
	putFile(t, s3client, bucket, "same.txt", "same")

	workdir := fs.NewDir(t, bucket, fs.WithFile("changed.txt", "bbbb"), fs.WithFile("same.txt", "same"))
	defer workdir.Remove()
	future := time.Now().Add(time.Hour)
	setMtime(t, workdir.Join("changed.txt"), future)
	setMtime(t, workdir.Join("same.txt"), future)
	dir := filepath.ToSlash(workdir.Path()) + "/"

	// the local files are newer, so the default strategy keeps them.
	icmd.RunCmd(s5cmd("sync", fmt.Sprintf("s3://%v/*", bucket), dir)).Assert(t, icmd.Success)
	got, _ := os.ReadFile(workdir.Join("changed.txt"))
	assert.Equal(t, string(got), "bbbb")

	result := icmd.RunCmd(s5cmd("sync", "--checksum", fmt.Sprintf("s3://%v/*", bucket), dir))
	result.Assert(t, icmd.Success)
	got, _ = os.ReadFile(workdir.Join("changed.txt"))
	assert.Equal(t, string(got), "aaaa")
	assert.Assert(t, !strings.Contains(result.Stdout(), "same.txt"), result.Stdout())
}

func TestSyncChecksumRemoteToRemote(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	src, dst := s3BucketFromTestNameWithPrefix(t, "src"), s3BucketFromTestNameWithPrefix(t, "dst")
	createBucket(t, s3client, src)
	createBucket(t, s3client, dst)
	putFile(t, s3client, src, "changed.txt", "aaaa")
	putFile(t, s3client, src, "same.txt", "same")
	time.Sleep(1100 * time.Millisecond) // the destination is newer
	putFile(t, s3client, dst, "changed.txt", "bbbb")
	putFile(t, s3client, dst, "same.txt", "same")

	result := icmd.RunCmd(s5cmd("sync", "--checksum", fmt.Sprintf("s3://%v/*", src), fmt.Sprintf("s3://%v/", dst)))
	result.Assert(t, icmd.Success)

	assert.Assert(t, ensureS3Object(s3client, dst, "changed.txt", "aaaa"))
	assert.Assert(t, !strings.Contains(result.Stdout(), "same.txt"), result.Stdout())
}

// A multipart object is compared with the checksum of its parts, or with its
// multipart ETag.
func TestSyncChecksumMultipart(t *testing.T) {
	t.Parallel()

	for _, alg := range []string{"", "CRC32C"} {
		alg := alg
		t.Run("algorithm="+alg, func(t *testing.T) {
			t.Parallel()

			layer := newChecksumLayer()
			s3client, s5cmd := setup(t, withChecksumLayer(layer))
			bucket := s3BucketFromTestName(t)
			createBucket(t, s3client, bucket)

			content := randomContent(12 << 20)
			workdir := fs.NewDir(t, bucket, fs.WithFile("big.bin", content))
			defer workdir.Remove()
			dir := filepath.ToSlash(workdir.Path()) + "/"
			dst := fmt.Sprintf("s3://%v/", bucket)

			up := []string{"sync", "--part-size", "5"}
			if alg != "" {
				up = append(up, "--checksum-algorithm", alg)
			}
			icmd.RunCmd(s5cmd(append(up, dir, dst)...)).Assert(t, icmd.Success)

			old := time.Now().Add(-2 * time.Hour)
			setMtime(t, workdir.Join("big.bin"), old)

			// unchanged: skipped.
			result := icmd.RunCmd(s5cmd("sync", "--checksum", "--part-size", "5", dir, dst))
			result.Assert(t, icmd.Success)
			assert.Assert(t, !strings.Contains(result.Stdout(), "big.bin"), result.Stdout())

			// one byte in the middle part changed, same size, old mtime.
			changed := []byte(content)
			changed[7<<20] ^= 0xff
			assert.NilError(t, os.WriteFile(workdir.Join("big.bin"), changed, 0o644))
			setMtime(t, workdir.Join("big.bin"), old)

			result = icmd.RunCmd(s5cmd("sync", "--part-size", "5", dir, dst))
			result.Assert(t, icmd.Success)
			assert.Assert(t, !strings.Contains(result.Stdout(), "big.bin"), "the default strategy cannot see it: "+result.Stdout())

			result = icmd.RunCmd(s5cmd("sync", "--checksum", "--part-size", "5", dir, dst))
			result.Assert(t, icmd.Success)
			assert.Assert(t, strings.Contains(result.Stdout(), "big.bin"), result.Stdout())
			assert.Assert(t, ensureS3Object(s3client, bucket, "big.bin", string(changed)))
		})
	}
}

// When the object cannot be compared, sync --checksum falls back to size and
// modification time, and says so in the debug log.
func TestSyncChecksumFallback(t *testing.T) {
	t.Parallel()

	layer := newChecksumLayer()
	layer.setSSEKMS()
	s3client, s5cmd := setup(t, withChecksumLayer(layer))
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)

	workdir := fs.NewDir(t, bucket, fs.WithFile("a.txt", "aaaa"), fs.WithFile("b.txt", "bbbb"))
	defer workdir.Remove()
	dir := filepath.ToSlash(workdir.Path()) + "/"
	dst := fmt.Sprintf("s3://%v/", bucket)
	icmd.RunCmd(s5cmd("sync", dir, dst)).Assert(t, icmd.Success)

	// a.txt: same size, old mtime. b.txt: same size, new mtime.
	assert.NilError(t, os.WriteFile(workdir.Join("a.txt"), []byte("cccc"), 0o644))
	setMtime(t, workdir.Join("a.txt"), time.Now().Add(-2*time.Hour))
	setMtime(t, workdir.Join("b.txt"), time.Now().Add(time.Hour))

	result := icmd.RunCmd(s5cmd("--log", "debug", "sync", "--checksum", dir, dst))
	result.Assert(t, icmd.Success)
	assert.Assert(t, strings.Contains(result.Stderr(), "cannot compare the content"), result.Stderr())
	assert.Assert(t, ensureS3Object(s3client, bucket, "a.txt", "aaaa"), "falls back to the default strategy")
	assert.Assert(t, strings.Contains(result.Stdout(), "b.txt"), result.Stdout())
}

func TestSyncChecksumAndSizeOnlyAreExclusive(t *testing.T) {
	t.Parallel()

	_, s5cmd := setup(t)
	result := icmd.RunCmd(s5cmd("sync", "--checksum", "--size-only", "s3://bucket/*", "."))
	result.Assert(t, icmd.Expected{ExitCode: 1})
	assert.Assert(t, strings.Contains(result.Stderr(), "cannot be used together"), result.Stderr())
}

// --no-clobber never overwrites, also when --checksum finds different content.
func TestCopyChecksumDoesNotOverrideNoClobber(t *testing.T) {
	t.Parallel()

	s3client, s5cmd := setup(t)
	bucket := s3BucketFromTestName(t)
	createBucket(t, s3client, bucket)
	putFile(t, s3client, bucket, "obj", "aaaa")

	workdir := fs.NewDir(t, bucket, fs.WithFile("obj", "bbbb"))
	defer workdir.Remove()
	src := filepath.ToSlash(workdir.Join("obj"))
	dst := fmt.Sprintf("s3://%v/obj", bucket)

	icmd.RunCmd(s5cmd("cp", "--checksum", "--no-clobber", src, dst)).Assert(t, icmd.Success)
	assert.Assert(t, ensureS3Object(s3client, bucket, "obj", "aaaa"))

	// without --no-clobber the different content of the same size is copied.
	icmd.RunCmd(s5cmd("cp", "--checksum", src, dst)).Assert(t, icmd.Success)
	assert.Assert(t, ensureS3Object(s3client, bucket, "obj", "bbbb"))
}
