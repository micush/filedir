package filesync

import (
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Hash is the hash of a block (HashName).
type Hash [32]byte

func (h Hash) MarshalText() ([]byte, error) { return []byte(hex.EncodeToString(h[:])), nil }
func (h *Hash) UnmarshalText(b []byte) error {
	if len(b) != 2*len(Hash{}) {
		return errors.New("filesync: a block hash is 64 hex digits")
	}
	_, err := hex.Decode(h[:], b)
	return err
}

// Kind is what a path is.
type Kind uint8

const (
	KFile Kind = iota
	KDir
	KSymlink
)

// Block is one piece of a file.
type Block struct {
	Off  int64 `json:"o"`
	Size int32 `json:"s"`
	Hash Hash  `json:"h"`
}

// FileInfo is what the index holds for one path.  A deleted path stays as a tombstone, so the deletion reaches nodes that were away.
type FileInfo struct {
	Name    string  `json:"name"` // slash-separated, relative to the folder, never starting with / or holding ..
	Kind    Kind    `json:"kind"`
	Size    int64   `json:"size,omitempty"`
	Mode    uint32  `json:"mode,omitempty"` // permission bits only
	ModNs   int64   `json:"mod"`            // modification time, unix nanoseconds: it picks a winner between concurrent changes and nothing else
	Target  string  `json:"target,omitempty"`
	Deleted bool    `json:"del,omitempty"`
	Purge   bool    `json:"purge,omitempty"` // a deletion made with Shift+Delete: the nodes it reaches do not keep a copy in their trash
	Version Vector  `json:"ver"`
	By      NodeID  `json:"by"`            // the node that made this change
	Seq     int64   `json:"seq"`           // position in the sending node's index: what a peer asks "after"
	Blocks  []Block `json:"blk,omitempty"` // files only
}

// Limits on what a peer may send.
const (
	maxNameLen = 4096
	// MetaDir is the folder's own directory inside it: the marker that says the folder is really there, the trash, and partial files.
	// It is never scanned and never accepted from a peer.
	MetaDir = ".filedir"
)

var (
	ErrBadName     = errors.New("filesync: a file name that is not allowed")
	ErrBadInfo     = errors.New("filesync: an index entry that does not add up")
	ErrChanged     = errors.New("filesync: the file changed on disk")
	ErrNoMarker    = errors.New("filesync: the folder's marker is missing (is it mounted?): nothing was scanned")
	ErrMassDelete  = errors.New("filesync: the scan would delete most of the folder: nothing was changed")
	ErrLocalChange = errors.New("filesync: the file changed here since the last scan")
)

// ValidName says whether a peer may name this path: clean, relative, inside the folder, outside the folder's own directory.
func ValidName(name string) error {
	if name == "" || len(name) > maxNameLen || strings.ContainsRune(name, 0) || strings.ContainsRune(name, '\\') {
		return ErrBadName
	}
	if path.Clean(name) != name || strings.HasPrefix(name, "/") || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return ErrBadName
	}
	if name == MetaDir || strings.HasPrefix(name, MetaDir+"/") {
		return ErrBadName
	}
	return nil
}

// Check says whether an entry from a peer is well formed: a valid name, and blocks that cover exactly the size.
func (fi *FileInfo) Check(maxSize int64) error {
	if err := ValidName(fi.Name); err != nil {
		return err
	}
	if len(fi.Version) == 0 {
		return fmt.Errorf("%w: %s has no version", ErrBadInfo, fi.Name)
	}
	if fi.Deleted {
		return nil
	}
	switch fi.Kind {
	case KDir:
		return nil
	case KSymlink:
		if fi.Target == "" || strings.ContainsRune(fi.Target, 0) {
			return fmt.Errorf("%w: %s has no link target", ErrBadInfo, fi.Name)
		}
		return nil
	case KFile:
	default:
		return fmt.Errorf("%w: %s has kind %d", ErrBadInfo, fi.Name, fi.Kind)
	}
	if fi.Size < 0 || (maxSize > 0 && fi.Size > maxSize) {
		return fmt.Errorf("%w: %s is %d bytes", ErrBadInfo, fi.Name, fi.Size)
	}
	var off int64
	for _, b := range fi.Blocks {
		if b.Off != off || b.Size <= 0 {
			return fmt.Errorf("%w: %s has blocks that are not contiguous", ErrBadInfo, fi.Name)
		}
		off += int64(b.Size)
	}
	if off != fi.Size {
		return fmt.Errorf("%w: %s has blocks for %d of %d bytes", ErrBadInfo, fi.Name, off, fi.Size)
	}
	return nil
}

// sameContent says whether two entries describe the same thing on disk, whatever their versions.
func sameContent(a, b *FileInfo, ignorePerms bool) bool {
	if a.Deleted != b.Deleted {
		return false
	}
	if a.Deleted {
		return true
	}
	if a.Kind != b.Kind {
		return false
	}
	if !ignorePerms && a.Mode != b.Mode {
		return false
	}
	switch a.Kind {
	case KSymlink:
		return a.Target == b.Target
	case KDir:
		return true
	}
	if a.Size != b.Size || len(a.Blocks) != len(b.Blocks) {
		return false
	}
	for i := range a.Blocks {
		if a.Blocks[i].Hash != b.Blocks[i].Hash {
			return false
		}
	}
	return true
}
