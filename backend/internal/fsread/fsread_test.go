package fsread

import (
	"os"
	"path/filepath"
	"testing"
)

func setupRepo(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "fsread-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	if err := os.MkdirAll(filepath.Join(root, "src", "auth"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "auth", "jwt.go"), []byte("package auth\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# hi\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// A secret file OUTSIDE the repo root, to prove traversal can't reach it.
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "outside-secret.txt"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(filepath.Join(filepath.Dir(root), "outside-secret.txt")) })

	return root
}

func TestReadValidFile(t *testing.T) {
	root := setupRepo(t)
	fc, err := Read(root, "src/auth/jwt.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fc.Language != "go" {
		t.Errorf("expected language 'go', got %q", fc.Language)
	}
	if fc.Content != "package auth\n" {
		t.Errorf("unexpected content: %q", fc.Content)
	}
}

func TestReadTraversalBlocked(t *testing.T) {
	root := setupRepo(t)
	attacks := []string{
		"../outside-secret.txt",
		"../../outside-secret.txt",
		"src/../../outside-secret.txt",
		"/etc/passwd",
		"src/auth/../../../outside-secret.txt",
	}
	for _, a := range attacks {
		_, err := Read(root, a)
		if err == nil {
			t.Errorf("attack path %q was NOT blocked", a)
		}
	}
}

func TestReadDirReturnsError(t *testing.T) {
	root := setupRepo(t)
	_, err := Read(root, "src/auth")
	if err != ErrIsDir {
		t.Errorf("expected ErrIsDir, got %v", err)
	}
}

func TestReadMissingFile(t *testing.T) {
	root := setupRepo(t)
	_, err := Read(root, "does/not/exist.go")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestReadEmptyPath(t *testing.T) {
	root := setupRepo(t)
	if _, err := Read(root, ""); err != ErrOutsideRepo {
		t.Errorf("expected ErrOutsideRepo for empty path, got %v", err)
	}
}
