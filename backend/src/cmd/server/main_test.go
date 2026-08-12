package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("file fallback", func(t *testing.T) {
		t.Setenv("TEST_SECRET", "")
		t.Setenv("TEST_SECRET_FILE", path)
		got, err := envSecret("TEST_SECRET", "TEST_SECRET_FILE")
		if err != nil {
			t.Fatal(err)
		}
		if got != "file-key" {
			t.Fatalf("secret = %q, want file-key", got)
		}
	})

	t.Run("environment wins", func(t *testing.T) {
		t.Setenv("TEST_SECRET", "env-key")
		t.Setenv("TEST_SECRET_FILE", path)
		got, err := envSecret("TEST_SECRET", "TEST_SECRET_FILE")
		if err != nil {
			t.Fatal(err)
		}
		if got != "env-key" {
			t.Fatalf("secret = %q, want env-key", got)
		}
	})
}
