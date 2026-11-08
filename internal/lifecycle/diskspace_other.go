//go:build !unix

package lifecycle

import "errors"

// statDiskSpace has no statfs to read off unix. The brain only runs on Linux;
// this keeps the package building elsewhere, and the snapshot treats the error
// as "space unknown" and relies on the write failing when the disk is full.
func statDiskSpace(string) (free, total uint64, err error) {
	return 0, 0, errors.New("disk space is not known on this platform")
}
