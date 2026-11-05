package e2e

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// checksumLayer wraps an S3 handler with the parts of S3 checksum handling
// that gofakes3 does not have: it verifies the x-amz-checksum-* headers of
// uploads, remembers the checksums per object and returns them for requests
// that ask for them with x-amz-checksum-mode, including HEAD requests for a
// single part. It also gives multipart objects the ETag S3 gives them.
type checksumLayer struct {
	next http.Handler

	mu      sync.Mutex
	objects map[string]*layerObject
	uploads map[string]*layerUpload
	// verified counts the checksums that were checked on upload, per
	// "<operation>:<algorithm>", for example "PutObject:CRC32C".
	verified map[string]int
	// rejected counts the uploads refused because of a wrong checksum.
	rejected int
	// heads counts HEAD requests for a part, so tests can see that a
	// verification used them.
	partHeads int
	sseKMS    bool
	// corruptGets changes the body of GET responses.
	corruptGets bool
}

type layerObject struct {
	sums      map[string]string // algorithm -> base64 value, composite for multipart
	partSums  []map[string]string
	partSizes []int64
	etag      string
}

type layerUpload struct {
	alg   string
	parts map[int]*layerPart
}

type layerPart struct {
	sums map[string]string
	md5  []byte
	size int64
}

func newChecksumLayer() *checksumLayer {
	return &checksumLayer{
		objects:  map[string]*layerObject{},
		uploads:  map[string]*layerUpload{},
		verified: map[string]int{},
	}
}

// layerAlgorithms maps the header suffix to the algorithm name.
var layerAlgorithms = map[string]string{
	"crc32":  "CRC32",
	"crc32c": "CRC32C",
	"sha256": "SHA256",
	"sha1":   "SHA1",
}

func layerHash(alg string) hash.Hash {
	switch alg {
	case "CRC32":
		return crc32.NewIEEE()
	case "CRC32C":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "SHA256":
		return sha256.New()
	case "SHA1":
		return sha1.New()
	}
	panic("unknown algorithm " + alg)
}

func layerSum(alg string, data []byte) string {
	h := layerHash(alg)
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (l *checksumLayer) counts() (map[string]int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for k, v := range l.verified {
		out[k] = v
	}
	return out, l.rejected
}

func (l *checksumLayer) partHeadCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.partHeads
}

// setSSEKMS makes HEAD and GET report aws:kms encryption, with the ETag
// that S3 gives such objects (not an MD5).
func (l *checksumLayer) setSSEKMS() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sseKMS = true
}

// corruptDownloads makes GET responses carry a changed first byte, as if the
// data was damaged on the way.
func (l *checksumLayer) corruptDownloads(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.corruptGets = on
}

func layerError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

// requestChecksums returns the algorithm -> value pairs in the headers.
func requestChecksums(h http.Header) map[string]string {
	out := map[string]string{}
	for suffix, alg := range layerAlgorithms {
		if v := h.Get("X-Amz-Checksum-" + suffix); v != "" {
			out[alg] = v
		}
	}
	return out
}

func (l *checksumLayer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := r.URL.Path

	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		rec := httptest.NewRecorder()
		l.next.ServeHTTP(rec, r)
		var res struct {
			UploadID string `xml:"UploadId"`
		}
		_ = xml.Unmarshal(rec.Body.Bytes(), &res)
		if res.UploadID != "" {
			l.mu.Lock()
			l.uploads[res.UploadID] = &layerUpload{
				alg:   r.Header.Get("X-Amz-Checksum-Algorithm"),
				parts: map[int]*layerPart{},
			}
			l.mu.Unlock()
		}
		copyRecorded(w, rec)

	case r.Method == http.MethodPut && q.Get("partNumber") != "" && q.Get("uploadId") != "":
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		n, _ := strconv.Atoi(q.Get("partNumber"))
		sums, ok := l.verifyBody("UploadPart", r.Header, body)
		if !ok {
			layerError(w, "BadDigest")
			return
		}
		md := md5.Sum(body)
		l.mu.Lock()
		if up := l.uploads[q.Get("uploadId")]; up != nil {
			up.parts[n] = &layerPart{sums: sums, md5: md[:], size: int64(len(body))}
		}
		l.mu.Unlock()
		l.next.ServeHTTP(w, r)

	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		obj, ok := l.complete(q.Get("uploadId"), body)
		if !ok {
			layerError(w, "InvalidPart")
			return
		}
		rec := httptest.NewRecorder()
		l.next.ServeHTTP(rec, r)
		if rec.Code == http.StatusOK {
			l.mu.Lock()
			l.objects[path] = obj
			l.mu.Unlock()
		}
		copyRecorded(w, rec)

	case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") == "":
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		sums, ok := l.verifyBody("PutObject", r.Header, body)
		if !ok {
			layerError(w, "BadDigest")
			return
		}
		rec := httptest.NewRecorder()
		l.next.ServeHTTP(rec, r)
		if rec.Code == http.StatusOK {
			md := md5.Sum(body)
			l.mu.Lock()
			l.objects[path] = &layerObject{sums: sums, etag: `"` + hex.EncodeToString(md[:]) + `"`, partSizes: []int64{int64(len(body))}}
			l.mu.Unlock()
		}
		copyRecorded(w, rec)

	case (r.Method == http.MethodHead || r.Method == http.MethodGet) && q.Get("uploadId") == "":
		rec := httptest.NewRecorder()
		l.next.ServeHTTP(rec, r)
		l.decorate(rec, r)
		copyRecorded(w, rec)

	default:
		l.next.ServeHTTP(w, r)
	}
}

// verifyBody checks the checksum headers of an upload against the body, the
// way S3 does. It returns the checksums that were sent.
func (l *checksumLayer) verifyBody(op string, h http.Header, body []byte) (map[string]string, bool) {
	sums := requestChecksums(h)
	l.mu.Lock()
	defer l.mu.Unlock()
	for alg, v := range sums {
		if layerSum(alg, body) != v {
			l.rejected++
			return nil, false
		}
		l.verified[op+":"+alg]++
	}
	return sums, true
}

type completeBody struct {
	Parts []struct {
		PartNumber int    `xml:"PartNumber"`
		CRC32      string `xml:"ChecksumCRC32"`
		CRC32C     string `xml:"ChecksumCRC32C"`
		SHA1       string `xml:"ChecksumSHA1"`
		SHA256     string `xml:"ChecksumSHA256"`
	} `xml:"Part"`
}

// complete checks the part list of a CompleteMultipartUpload request and
// builds the object entry with the composite checksum.
func (l *checksumLayer) complete(uploadID string, body []byte) (*layerObject, bool) {
	var req completeBody
	if err := xml.Unmarshal(body, &req); err != nil {
		return nil, false
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	up := l.uploads[uploadID]
	if up == nil {
		return &layerObject{sums: map[string]string{}}, true
	}

	sort.Slice(req.Parts, func(i, j int) bool { return req.Parts[i].PartNumber < req.Parts[j].PartNumber })

	obj := &layerObject{sums: map[string]string{}}
	var etagInput []byte
	raw := map[string][]byte{}
	for _, p := range req.Parts {
		part := up.parts[p.PartNumber]
		if part == nil {
			return nil, false
		}
		given := map[string]string{"CRC32": p.CRC32, "CRC32C": p.CRC32C, "SHA1": p.SHA1, "SHA256": p.SHA256}
		for alg, v := range given {
			if v != "" && part.sums[alg] != v {
				l.rejected++
				return nil, false
			}
		}
		for alg, v := range part.sums {
			d, _ := base64.StdEncoding.DecodeString(v)
			raw[alg] = append(raw[alg], d...)
		}
		obj.partSums = append(obj.partSums, part.sums)
		obj.partSizes = append(obj.partSizes, part.size)
		etagInput = append(etagInput, part.md5...)
	}
	// the upload was started with an algorithm, so every part has to carry it.
	if up.alg != "" {
		for _, p := range obj.partSums {
			if p[up.alg] == "" {
				l.rejected++
				return nil, false
			}
		}
	}
	for alg, concat := range raw {
		h := layerHash(alg)
		h.Write(concat)
		obj.sums[alg] = base64.StdEncoding.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(req.Parts))
		l.verified["CompleteMultipartUpload:"+alg]++
	}
	md := md5.Sum(etagInput)
	obj.etag = fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(md[:]), len(req.Parts))
	return obj, true
}

// decorate adds the stored checksums and ETag to a HEAD or GET response.
func (l *checksumLayer) decorate(rec *httptest.ResponseRecorder, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	obj := l.objects[r.URL.Path]
	if obj == nil || rec.Code != http.StatusOK {
		return
	}
	h := rec.Header()
	if l.corruptGets && r.Method == http.MethodGet && !r.URL.Query().Has("list-type") && rec.Body.Len() > 0 {
		b := rec.Body.Bytes()
		b[0] ^= 0xff
	}
	h.Set("ETag", obj.etag)
	if l.sseKMS {
		h.Set("X-Amz-Server-Side-Encryption", "aws:kms")
		h.Set("ETag", `"`+strings.Repeat("ab", 16)+`"`)
	}

	if pn := r.URL.Query().Get("partNumber"); pn != "" && r.Method == http.MethodHead {
		n, err := strconv.Atoi(pn)
		if err != nil || n < 1 || n > len(obj.partSizes) {
			rec.Code = http.StatusRequestedRangeNotSatisfiable
			return
		}
		l.partHeads++
		h.Set("Content-Length", strconv.FormatInt(obj.partSizes[n-1], 10))
		h.Set("X-Amz-Mp-Parts-Count", strconv.Itoa(len(obj.partSizes)))
		if r.Header.Get("X-Amz-Checksum-Mode") == "ENABLED" && n-1 < len(obj.partSums) {
			for alg, v := range obj.partSums[n-1] {
				h.Set("X-Amz-Checksum-"+strings.ToLower(alg), v)
			}
		}
		return
	}

	if r.Header.Get("X-Amz-Checksum-Mode") == "ENABLED" {
		for alg, v := range obj.sums {
			h.Set("X-Amz-Checksum-"+strings.ToLower(alg), v)
		}
	}
}

func copyRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, vv := range rec.Header() {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes())
}
