package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSetupProofPersistsAndHonorsEnvironment(t *testing.T) {
	t.Setenv("NEWNEXUS_SETUP_TOKEN", "")
	dir := t.TempDir()
	token, e := loadSetupToken(dir)
	if e != nil || len(token) != 43 {
		t.Fatal("failed to generate random setup proof", e)
	}
	again, e := loadSetupToken(dir)
	if e != nil || again != token {
		t.Fatal("setup proof changed across restart", e)
	}
	info, e := os.Stat(filepath.Join(dir, "bootstrap.token"))
	if e != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("setup proof has unsafe permissions", e)
	}
	t.Setenv("NEWNEXUS_SETUP_TOKEN", strings.Repeat("x", 32))
	fromEnv, e := loadSetupToken(dir)
	if e != nil || fromEnv != strings.Repeat("x", 32) {
		t.Fatal("explicit setup proof was ignored", e)
	}
	for _, invalid := range []string{"short", strings.Repeat("x", 32) + "\n", strings.Repeat("x", 513)} {
		t.Setenv("NEWNEXUS_SETUP_TOKEN", invalid)
		if _, e = loadSetupToken(dir); e == nil {
			t.Fatal("unsafe environment setup proof accepted")
		}
	}
}

func TestSetupProofRejectsCorruptExistingFile(t *testing.T) {
	t.Setenv("NEWNEXUS_SETUP_TOKEN", "")
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "bootstrap.token"), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := loadSetupToken(dir); e == nil {
		t.Fatal("invalid existing proof was silently replaced")
	}
}
