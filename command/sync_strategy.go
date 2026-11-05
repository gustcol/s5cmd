package command

import (
	errorpkg "github.com/peak/s5cmd/v2/error"
	"github.com/peak/s5cmd/v2/storage"
)

// SyncStrategy is the interface to make decision whether given source object should be synced
// to destination object
type SyncStrategy interface {
	ShouldSync(srcObject, dstObject *storage.Object) error
}

func NewStrategy(sizeOnly, checksum bool) SyncStrategy {
	switch {
	case checksum:
		return &ChecksumStrategy{}
	case sizeOnly:
		return &SizeOnlyStrategy{}
	default:
		return &SizeAndModificationStrategy{}
	}
}

// ChecksumStrategy hands every object that exists on both sides to the copy
// command, which compares the content when it runs (cp --checksum) and skips
// the object if it is the same. The comparison reads the local file, so it is
// not done here: the planner is a single goroutine, and the copy commands run
// in parallel.
type ChecksumStrategy struct{}

func (s *ChecksumStrategy) ShouldSync(srcObj, dstObj *storage.Object) error {
	return nil
}

// SizeOnlyStrategy determines to sync based on objects' file sizes.
type SizeOnlyStrategy struct{}

func (s *SizeOnlyStrategy) ShouldSync(srcObj, dstObj *storage.Object) error {
	if srcObj.Size == dstObj.Size {
		return errorpkg.ErrObjectSizesMatch
	}
	return nil
}

// SizeAndModificationStrategy determines to sync based on objects' both sizes and modification times.
// It treats source object as the source-of-truth;
//
//	time: src > dst        size: src != dst    should sync: yes
//	time: src > dst        size: src == dst    should sync: yes
//	time: src <= dst       size: src != dst    should sync: yes
//	time: src <= dst       size: src == dst    should sync: no
type SizeAndModificationStrategy struct{}

func (sm *SizeAndModificationStrategy) ShouldSync(srcObj, dstObj *storage.Object) error {
	srcMod, dstMod := srcObj.ModTime, dstObj.ModTime
	if srcMod.After(*dstMod) {
		return nil
	}

	if srcObj.Size != dstObj.Size {
		return nil
	}

	return errorpkg.ErrObjectIsNewerAndSizesMatch
}
