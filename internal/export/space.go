package export

import (
	"fmt"
	"math"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// SpaceChecker reports free space on the filesystem holding dir.
//
// It is an interface so "disk nearly full", "disk fills mid-write", and "over
// the cap" are ordinary table tests. A safety check only exercised in
// production is not a safety check (spec §13).
type SpaceChecker interface {
	Available(dir string) (avail, total int64, err error)
}

type statfsChecker struct{}

// NewStatfsChecker returns the production SpaceChecker.
func NewStatfsChecker() SpaceChecker { return statfsChecker{} }

// Available reports bytes available to an unprivileged user, and total size.
//
// An error here means "we do not know", which the caller must treat as "not
// enough" — never as permission to write (invariant 8).
func (statfsChecker) Available(dir string) (int64, int64, error) {
	var st unix.Statfs_t

	clean := filepath.Clean(dir)

	if err := unix.Statfs(clean, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", clean, err)
	}

	// Bavail, not Bfree: Bfree includes blocks reserved for root, which we
	// must not plan to consume even though we are running as root. Spending
	// the reserve is how a "successful" export takes the master down.
	return bytesFrom(st.Bavail, st.Bsize), bytesFrom(st.Blocks, st.Bsize), nil
}

// bytesFrom converts a statfs block count to bytes, saturating rather than
// wrapping. A wrapped negative here would read as "no space" on a healthy
// filesystem, or worse, as plenty on a full one.
func bytesFrom(blocks uint64, blockSize int64) int64 {
	if blockSize <= 0 {
		return 0
	}

	count := int64(math.MaxInt64)
	if blocks <= math.MaxInt64 {
		count = int64(blocks)
	}

	if count > math.MaxInt64/blockSize {
		return math.MaxInt64
	}

	return count * blockSize
}
