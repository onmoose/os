//go:build unix

package lifecycle

import "syscall"

// statDiskSpace reports the space free to an unprivileged writer and the size
// of the filesystem that holds path, in bytes (statfs). The pre-update
// snapshot reads it before it writes the tar (snapshot.go).
func statDiskSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) // int64 on linux, uint32 on darwin
	return st.Bavail * bs, st.Blocks * bs, nil
}
