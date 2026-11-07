package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

func validateSetupToken(token string) error {
	if len(token) < 32 || len(token) > 512 || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return errors.New("setup token must contain 32 to 512 bytes without whitespace")
	}
	return nil
}

// loadSetupToken preserves the same proof across restarts. Its plaintext is
// available only to an operator with access to the environment or data volume.
func loadSetupToken(dataDir string) (string, error) {
	if token := os.Getenv("NEWNEXUS_SETUP_TOKEN"); token != "" {
		return token, validateSetupToken(token)
	}
	path := filepath.Join(dataDir, "bootstrap.token")
	read := func() (string, error) {
		file, e := os.Open(path)
		if e != nil {
			return "", e
		}
		defer file.Close()
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() > 512 {
			return "", errors.New("bootstrap.token must be a regular file containing a valid setup token")
		}
		// Restrict an existing file too; do not silently keep an operator-created
		// token world-readable on Unix. Windows enforces the data-directory ACL.
		if e = file.Chmod(0600); e != nil {
			return "", e
		}
		b := make([]byte, info.Size())
		n, e := file.ReadAt(b, 0)
		if e != nil && n != len(b) {
			return "", e
		}
		token := string(b)
		return token, validateSetupToken(token)
	}
	if _, e := os.Stat(path); e == nil {
		return read()
	} else if !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(e, os.ErrExist) {
		return read()
	}
	if e != nil {
		return "", fmt.Errorf("create bootstrap proof: %w", e)
	}
	if _, e = file.WriteString(token); e == nil {
		e = file.Sync()
	}
	closeError := file.Close()
	if e != nil {
		return "", e
	}
	if closeError != nil {
		return "", closeError
	}
	return token, nil
}
