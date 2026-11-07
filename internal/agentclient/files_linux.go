//go:build linux

package agentclient

import (
	"os"
	"path/filepath"
	"syscall"
)

func lockDirectory(dir string) (func() error, error) {
	f, err := os.OpenFile(filepath.Join(dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() error { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); return f.Close() }, nil
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
