//go:build windows

package store

import (
	"golang.org/x/sys/windows"
	"path/filepath"
)

func DiskUsage(path string) (free, total uint64, err error) {
	absolute, e := filepath.Abs(path)
	if e != nil {
		return 0, 0, e
	}
	name, e := windows.UTF16PtrFromString(absolute)
	if e != nil {
		return 0, 0, e
	}
	var available uint64
	err = windows.GetDiskFreeSpaceEx(name, &available, &total, &free)
	return free, total, err
}
