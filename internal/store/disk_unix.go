//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package store

import (
	"syscall"
)

func DiskUsage(path string) (free, total uint64, err error) {
	var stat syscall.Statfs_t
	if err = syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), uint64(stat.Blocks) * uint64(stat.Bsize), nil
}
