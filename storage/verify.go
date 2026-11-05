package storage

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/peak/s5cmd/v2/storage/url"
)

// Integrity holds what an object reports about its content: size, ETag and
// the additional checksums it was uploaded with.
type Integrity struct {
	Size int64
	// ETag is the ETag without quotes.
	ETag string
	// ServerSideEncryption is the encryption method of the object, if any.
	ServerSideEncryption string
	// SSECustomer is true if the object is encrypted with a customer key.
	SSECustomer bool
	// PartsCount is the number of parts of a multipart upload. It is set
	// only when the server answered a request for a part number.
	PartsCount int64
	Checksums  []ObjectChecksum
}

// checksum returns the strongest additional checksum of the object.
func (i *Integrity) checksum() *ObjectChecksum {
	for _, alg := range ChecksumAlgorithms {
		for idx := range i.Checksums {
			if i.Checksums[idx].Algorithm == alg {
				return &i.Checksums[idx]
			}
		}
	}
	return nil
}

func (i *Integrity) checksumFor(alg ChecksumAlgorithm) (string, bool) {
	for _, c := range i.Checksums {
		if c.Algorithm == alg {
			return c.Value, true
		}
	}
	return "", false
}

// IntegrityProvider returns the integrity data of a remote object. With a
// part number greater than zero it describes that part of a multipart
// object: its size and checksums.
type IntegrityProvider interface {
	HeadIntegrity(ctx context.Context, u *url.URL, partNumber int64) (*Integrity, error)
}

// HeadIntegrity implements IntegrityProvider. It asks the server to return
// the checksums of the object.
func (s *S3) HeadIntegrity(ctx context.Context, u *url.URL, partNumber int64) (*Integrity, error) {
	input := &s3.HeadObjectInput{
		Bucket:       aws.String(u.Bucket),
		Key:          aws.String(u.Path),
		ChecksumMode: aws.String(s3.ChecksumModeEnabled),
		RequestPayer: s.RequestPayer(),
	}
	if u.VersionID != "" {
		input.SetVersionId(u.VersionID)
	}
	if partNumber > 0 {
		input.SetPartNumber(partNumber)
	}

	out, err := s.api.HeadObjectWithContext(ctx, input)
	if err != nil {
		if errHasCode(err, "NotFound") {
			return nil, &ErrGivenObjectNotFound{ObjectAbsPath: u.Absolute()}
		}
		return nil, err
	}

	info := &Integrity{
		Size:                 aws.Int64Value(out.ContentLength),
		ETag:                 strings.Trim(aws.StringValue(out.ETag), `"`),
		ServerSideEncryption: aws.StringValue(out.ServerSideEncryption),
		SSECustomer:          out.SSECustomerAlgorithm != nil,
		PartsCount:           aws.Int64Value(out.PartsCount),
	}
	for _, c := range []struct {
		alg ChecksumAlgorithm
		val *string
	}{
		{ChecksumCRC32C, out.ChecksumCRC32C},
		{ChecksumCRC32, out.ChecksumCRC32},
		{ChecksumSHA256, out.ChecksumSHA256},
		{ChecksumSHA1, out.ChecksumSHA1},
	} {
		if v := aws.StringValue(c.val); v != "" {
			info.Checksums = append(info.Checksums, ObjectChecksum{Algorithm: c.alg, Value: v})
		}
	}
	return info, nil
}

// CompareResult is the outcome of comparing a local file with an object.
type CompareResult int

const (
	// CompareUnknown means the content could not be compared with what the
	// object reports, for example a multipart ETag whose part layout is not
	// known. It is not evidence that the content is different.
	CompareUnknown CompareResult = iota
	// CompareEqual means the content matches.
	CompareEqual
	// CompareDifferent means the content is different.
	CompareDifferent
)

// Comparison describes how a local file compares with an object.
type Comparison struct {
	Result CompareResult
	// Method names what was compared, for example "CRC32C" or "MD5 ETag".
	Method string
	// Detail explains a different or unknown result.
	Detail string
}

// CompareOptions tune CompareFile.
type CompareOptions struct {
	// PartSizes are part sizes that the multipart layout of the object may
	// have, in addition to the one that the server reports. It is used when
	// the server cannot tell the size of a part, typically the part size
	// configured for the transfer.
	PartSizes []int64
}

// CompareFile compares the local file at path with the remote object u using
// what the object reports about itself, in this order:
//
//  1. the additional checksum it was uploaded with (CRC32C, CRC32, SHA256 or
//     SHA1), strongest first;
//  2. its ETag, when that is an MD5: a single part upload that is not
//     encrypted with KMS or a customer key, or a multipart upload whose part
//     layout can be reproduced.
//
// A multipart object is first compared assuming parts of equal size (the size
// of the first part, which the server reports, or the sizes in opts). If that
// does not match, the size and checksum of every part is requested, which
// takes one request per part but is exact for any part layout, so a different
// result is a real one. An unknown result is returned, instead of a guess,
// when none of the checks can be used.
func CompareFile(ctx context.Context, p IntegrityProvider, u *url.URL, path string, opts CompareOptions) (Comparison, error) {
	info, err := p.HeadIntegrity(ctx, u, 0)
	if err != nil {
		return Comparison{}, err
	}

	st, err := os.Stat(path)
	if err != nil {
		return Comparison{}, err
	}
	if st.Size() != info.Size {
		return Comparison{
			Result: CompareDifferent,
			Method: "size",
			Detail: fmt.Sprintf("local file has %d bytes, object has %d", st.Size(), info.Size),
		}, nil
	}

	if cs := info.checksum(); cs != nil {
		if cs.Composite() {
			return compareComposite(ctx, p, u, path, info, cs, opts)
		}
		got, err := HashFile(ctx, cs.Algorithm, path)
		if err != nil {
			return Comparison{}, err
		}
		return decide(string(cs.Algorithm), cs.Value, got), nil
	}

	return compareETag(ctx, p, u, path, info, opts)
}

func decide(method, want, got string) Comparison {
	if want == got {
		return Comparison{Result: CompareEqual, Method: method}
	}
	return Comparison{
		Result: CompareDifferent,
		Method: method,
		Detail: fmt.Sprintf("%s of the local file is %s, the object has %s", method, got, want),
	}
}

// partInfo asks for one part, and reports whether the server answered for
// the part number.
func partInfo(ctx context.Context, p IntegrityProvider, u *url.URL, n, parts int64) (*Integrity, bool, error) {
	pi, err := p.HeadIntegrity(ctx, u, n)
	if err != nil {
		return nil, false, err
	}
	return pi, pi.PartsCount == parts, nil
}

func compareComposite(ctx context.Context, p IntegrityProvider, u *url.URL, path string, info *Integrity, cs *ObjectChecksum, opts CompareOptions) (Comparison, error) {
	parts := cs.Parts()
	if parts < 1 {
		return Comparison{Method: string(cs.Algorithm), Detail: fmt.Sprintf("cannot read the number of parts from checksum %q", cs.Value)}, nil
	}
	method := string(cs.Algorithm) + " of parts"

	first, supported, err := partInfo(ctx, p, u, 1, parts)
	if err != nil {
		return Comparison{}, err
	}

	candidates := append([]int64(nil), opts.PartSizes...)
	if supported {
		candidates = append([]int64{first.Size}, candidates...)
	}

	local := ""
	for _, size := range candidates {
		sizes := uniformPartSizes(info.Size, size, parts)
		if sizes == nil {
			continue
		}
		digests, err := partDigests(ctx, cs.Algorithm.newHash, path, sizes)
		if err != nil {
			return Comparison{}, err
		}
		local = compositeValue(cs.Algorithm.newHash, digests)
		if local == cs.Value {
			return Comparison{Result: CompareEqual, Method: method}, nil
		}
	}

	if !supported {
		detail := "the part layout of the object is not known"
		if local != "" {
			detail = fmt.Sprintf("the %s of the local file split in parts of equal size is %s, the object has %s", method, local, cs.Value)
		}
		return Comparison{Method: method, Detail: detail}, nil
	}

	// compare part by part, with the sizes and checksums of each part.
	sizes := make([]int64, 0, parts)
	want := make([]string, 0, parts)
	var total int64
	for n := int64(1); n <= parts; n++ {
		pi := first
		if n > 1 {
			pi, _, err = partInfo(ctx, p, u, n, parts)
			if err != nil {
				return Comparison{}, err
			}
		}
		v, ok := pi.checksumFor(cs.Algorithm)
		if !ok {
			return Comparison{Method: method, Detail: fmt.Sprintf("the server did not return the %s of part %d", cs.Algorithm, n)}, nil
		}
		sizes = append(sizes, pi.Size)
		want = append(want, v)
		total += pi.Size
	}
	if total != info.Size {
		return Comparison{Method: method, Detail: fmt.Sprintf("the parts add up to %d bytes, the object has %d", total, info.Size)}, nil
	}

	digests, err := partDigests(ctx, cs.Algorithm.newHash, path, sizes)
	if err != nil {
		return Comparison{}, err
	}
	for i, d := range digests {
		if got := base64.StdEncoding.EncodeToString(d); got != want[i] {
			c := decide(method, want[i], got)
			c.Detail = fmt.Sprintf("part %d: %s", i+1, c.Detail)
			return c, nil
		}
	}
	return Comparison{Result: CompareEqual, Method: method}, nil
}

func isMD5Hex(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func compareETag(ctx context.Context, p IntegrityProvider, u *url.URL, path string, info *Integrity, opts CompareOptions) (Comparison, error) {
	switch {
	case info.SSECustomer:
		return Comparison{Method: "MD5 ETag", Detail: "the ETag of an object encrypted with a customer key is not an MD5"}, nil
	case strings.HasPrefix(info.ServerSideEncryption, "aws:kms"):
		return Comparison{Method: "MD5 ETag", Detail: "the ETag of an object encrypted with KMS is not an MD5 and the object has no additional checksum"}, nil
	}

	etag := strings.ToLower(info.ETag)
	if isMD5Hex(etag) {
		got, err := md5File(ctx, path)
		if err != nil {
			return Comparison{}, err
		}
		return decide("MD5 ETag", etag, got), nil
	}

	i := strings.LastIndexByte(etag, '-')
	if i != 32 || !isMD5Hex(etag[:i]) {
		return Comparison{Method: "ETag", Detail: fmt.Sprintf("the ETag %q is not an MD5", info.ETag)}, nil
	}
	parts, err := strconv.ParseInt(etag[i+1:], 10, 64)
	if err != nil || parts < 1 {
		return Comparison{Method: "ETag", Detail: fmt.Sprintf("the ETag %q is not an MD5", info.ETag)}, nil
	}

	method := "multipart ETag"
	candidates := append([]int64(nil), opts.PartSizes...)
	first, supported, err := partInfo(ctx, p, u, 1, parts)
	if err != nil {
		return Comparison{}, err
	}
	if supported {
		candidates = append([]int64{first.Size}, candidates...)
	}

	tried := false
	for _, size := range candidates {
		sizes := uniformPartSizes(info.Size, size, parts)
		if sizes == nil {
			continue
		}
		tried = true
		got, err := multipartETag(ctx, path, sizes)
		if err != nil {
			return Comparison{}, err
		}
		if got == etag {
			return Comparison{Result: CompareEqual, Method: method}, nil
		}
	}

	if supported {
		// the server reports the size of every part, so the ETag can be
		// reproduced for any part layout and a mismatch is a real one.
		sizes := make([]int64, 0, parts)
		var total int64
		for n := int64(1); n <= parts; n++ {
			pi := first
			if n > 1 {
				if pi, _, err = partInfo(ctx, p, u, n, parts); err != nil {
					return Comparison{}, err
				}
			}
			sizes = append(sizes, pi.Size)
			total += pi.Size
		}
		if total == info.Size {
			got, err := multipartETag(ctx, path, sizes)
			if err != nil {
				return Comparison{}, err
			}
			c := decide(method, etag, got)
			return c, nil
		}
	}

	detail := fmt.Sprintf("the object was uploaded in %d parts and the part size is not known", parts)
	if tried {
		detail = fmt.Sprintf("the ETag of an upload in %d parts could not be reproduced, the parts may have different sizes", parts)
	}
	return Comparison{Method: method, Detail: detail}, nil
}

var _ IntegrityProvider = (*S3)(nil)

// CompareObjects compares two remote objects using what they report about
// themselves, without reading their content: an additional checksum that
// both have, or equal ETags. Different multipart checksums or ETags are not
// evidence of different content, because the same content can be split in
// parts in different ways, so they give an unknown result.
func CompareObjects(ctx context.Context, srcP IntegrityProvider, srcURL *url.URL, dstP IntegrityProvider, dstURL *url.URL) (Comparison, error) {
	a, err := srcP.HeadIntegrity(ctx, srcURL, 0)
	if err != nil {
		return Comparison{}, err
	}
	b, err := dstP.HeadIntegrity(ctx, dstURL, 0)
	if err != nil {
		return Comparison{}, err
	}
	if a.Size != b.Size {
		return Comparison{
			Result: CompareDifferent,
			Method: "size",
			Detail: fmt.Sprintf("the objects have %d and %d bytes", a.Size, b.Size),
		}, nil
	}

	for _, alg := range ChecksumAlgorithms {
		av, aok := a.checksumFor(alg)
		bv, bok := b.checksumFor(alg)
		if !aok || !bok {
			continue
		}
		if av == bv {
			return Comparison{Result: CompareEqual, Method: string(alg)}, nil
		}
		if strings.Contains(av, "-") || strings.Contains(bv, "-") {
			return Comparison{Method: string(alg), Detail: "the objects have different checksums of parts, they may be split in parts differently"}, nil
		}
		return Comparison{
			Result: CompareDifferent,
			Method: string(alg),
			Detail: fmt.Sprintf("%s is %s and %s", alg, av, bv),
		}, nil
	}

	notMD5 := func(i *Integrity) bool {
		return i.SSECustomer || strings.HasPrefix(i.ServerSideEncryption, "aws:kms")
	}
	if notMD5(a) || notMD5(b) {
		return Comparison{Method: "ETag", Detail: "the ETag of an object encrypted with KMS or a customer key is not an MD5"}, nil
	}
	if a.ETag == "" || b.ETag == "" {
		return Comparison{Method: "ETag", Detail: "an object has no ETag"}, nil
	}
	if strings.EqualFold(a.ETag, b.ETag) {
		return Comparison{Result: CompareEqual, Method: "ETag"}, nil
	}
	if isMD5Hex(strings.ToLower(a.ETag)) && isMD5Hex(strings.ToLower(b.ETag)) {
		return Comparison{
			Result: CompareDifferent,
			Method: "MD5 ETag",
			Detail: fmt.Sprintf("ETag is %s and %s", a.ETag, b.ETag),
		}, nil
	}
	return Comparison{Method: "ETag", Detail: "the ETags differ and at least one is of a multipart upload"}, nil
}
