//go:build windows

package agentclient

import (
	"path/filepath"
	"syscall"
)

func lockDirectory(dir string) (func() error, error) {
	path, err := syscall.UTF16PtrFromString(filepath.Join(dir, "agent.lock"))
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return func() error { return syscall.CloseHandle(h) }, nil
}

// Windows does not permit fsync on directory handles; atomic file FlushFileBuffers
// + ReplaceFile semantics are used by os.File.Sync and os.Rename instead.
func syncDirectory(string) error { return nil }
