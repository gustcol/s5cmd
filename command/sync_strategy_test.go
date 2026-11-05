package command

import (
	"testing"
	"time"

	errorpkg "github.com/peak/s5cmd/v2/error"
	"github.com/peak/s5cmd/v2/storage"
)

func TestSizeAndModificationStrategy_ShouldSync(t *testing.T) {
	ft := time.Now()
	timePtr := func(tt time.Time) *time.Time {
		return &tt
	}
	testcases := []struct {
		name     string
		src      *storage.Object
		dst      *storage.Object
		expected error
	}{

		{
			//	time: src > dst       size: src != dst
			name:     "source is newer, sizes are different",
			src:      &storage.Object{ModTime: timePtr(ft.Add(time.Minute)), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft), Size: 5},
			expected: nil,
		},

		{
			//	time: src > dst       size: src = dst
			name:     "source is newer, sizes are same",
			src:      &storage.Object{ModTime: timePtr(ft.Add(time.Minute)), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			expected: nil,
		},

		{
			//	time: src < dst       size: src != dst
			name:     "source is older, sizes are different",
			src:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft.Add(time.Minute)), Size: 5},
			expected: nil,
		},

		{
			//	time: src < dst       size: src == dst
			name:     "source is older, sizes are same",
			src:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft.Add(time.Minute)), Size: 10},
			expected: errorpkg.ErrObjectIsNewerAndSizesMatch,
		},

		{
			//	time: src = dst       size: src != dst
			name:     "files have same age, sizes are different",
			src:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft), Size: 5},
			expected: nil,
		},

		{
			//	time: src = dst       size: src == dst
			name:     "files have same age, sizes are same",
			src:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			expected: errorpkg.ErrObjectIsNewerAndSizesMatch,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			strategy := &SizeAndModificationStrategy{}
			if got := strategy.ShouldSync(tc.src, tc.dst); got != tc.expected {
				t.Fatalf("expected: %q(%T), got: %q(%T)", tc.expected, tc.expected, got, got)
			}
		})
	}
}

func TestSizeOnlyStrategy_ShouldSync(t *testing.T) {
	ft := time.Now()
	timePtr := func(tt time.Time) *time.Time {
		return &tt
	}
	testcases := []struct {
		name     string
		src      *storage.Object
		dst      *storage.Object
		expected error
	}{

		{
			//	time: src > dst       size: src != dst
			name:     "source is newer, sizes are different",
			src:      &storage.Object{ModTime: timePtr(ft.Add(time.Minute)), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft), Size: 5},
			expected: nil,
		},

		{
			//	time: src > dst       size: src = dst
			name:     "source is newer, sizes are same",
			src:      &storage.Object{ModTime: timePtr(ft.Add(time.Minute)), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			expected: errorpkg.ErrObjectSizesMatch,
		},

		{
			//	time: src < dst       size: src != dst
			name:     "source is older, sizes are different",
			src:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft.Add(time.Minute)), Size: 5},
			expected: nil,
		},

		{
			//	time: src < dst       size: src == dst
			name:     "source is older, sizes are same",
			src:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft.Add(time.Minute)), Size: 10},
			expected: errorpkg.ErrObjectSizesMatch,
		},

		{
			//	time: src = dst       size: src != dst
			name:     "files have same age, sizes are different",
			src:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft), Size: 5},
			expected: nil,
		},

		{
			//	time: src = dst       size: src == dst
			name:     "files have same age, sizes are same",
			src:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			dst:      &storage.Object{ModTime: timePtr(ft), Size: 10},
			expected: errorpkg.ErrObjectSizesMatch,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			strategy := &SizeOnlyStrategy{}
			if got := strategy.ShouldSync(tc.src, tc.dst); got != tc.expected {
				t.Fatalf("expected: %q(%T), got: %q(%T)", tc.expected, tc.expected, got, got)
			}
		})
	}
}

func TestNewStrategy(t *testing.T) {
	if _, ok := NewStrategy(false, false).(*SizeAndModificationStrategy); !ok {
		t.Error("default strategy should compare size and modification time")
	}
	if _, ok := NewStrategy(true, false).(*SizeOnlyStrategy); !ok {
		t.Error("--size-only should compare sizes")
	}
	if _, ok := NewStrategy(false, true).(*ChecksumStrategy); !ok {
		t.Error("--checksum should use the checksum strategy")
	}
}

// The checksum strategy never decides: the copy command compares the content
// in a worker, because the comparison reads the local file.
func TestChecksumStrategyDefersToCopy(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Hour)
	for _, tc := range []struct{ srcSize, dstSize int64 }{{1, 1}, {1, 2}} {
		src := &storage.Object{Size: tc.srcSize, ModTime: &older}
		dst := &storage.Object{Size: tc.dstSize, ModTime: &now}
		if err := (&ChecksumStrategy{}).ShouldSync(src, dst); err != nil {
			t.Errorf("sizes %d and %d: got %v, want nil", tc.srcSize, tc.dstSize, err)
		}
	}
}

func TestContentVerdict(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Hour)
	obj := func(size int64, mod time.Time) *storage.Object { return &storage.Object{Size: size, ModTime: &mod} }

	tests := []struct {
		name     string
		cmp      storage.Comparison
		src, dst *storage.Object
		want     bool
	}{
		{"equal content with a newer source", storage.Comparison{Result: storage.CompareEqual, Method: "CRC32C"}, obj(5, now), obj(5, older), true},
		{"different content with an older source", storage.Comparison{Result: storage.CompareDifferent, Method: "CRC32C"}, obj(5, older), obj(5, now), false},
		{"unknown, source not newer", storage.Comparison{Detail: "kms"}, obj(5, older), obj(5, now), true},
		{"unknown, source newer", storage.Comparison{Detail: "kms"}, obj(5, now), obj(5, older), false},
		{"unknown, different size", storage.Comparison{Detail: "kms"}, obj(5, older), obj(6, now), false},
	}
	for _, tc := range tests {
		got, msg := contentVerdict(tc.cmp, tc.src, tc.dst)
		if got != tc.want {
			t.Errorf("%s: got %v, want %v (%s)", tc.name, got, tc.want, msg)
		}
		if msg == "" {
			t.Errorf("%s: empty message", tc.name)
		}
	}
}
