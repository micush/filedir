package filesync

import "filedir/filesync/blake3"

// HashName names the hash of a block.  It was SHA-256 up to version 80, and an index that was made with that is converted once (see
// MigrateIndex); a node that still uses it is not synced with (the peers say which they use, fd_cluster.go).  BLAKE3 is as sound for
// this, and on a processor without the SHA instructions SHA-256 needs the CPU for several times as long (see blake3).
const HashName = "blake3"

// sumBlock is the hash of a block.
func sumBlock(b []byte) Hash { return Hash(blake3.Sum256(b)) }
