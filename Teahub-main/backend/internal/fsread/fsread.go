// Package fsread provides safe, read-only access to files inside a cloned
// repo's local directory — used by the Interactive Repository Explorer to
// serve file content to the frontend.
//
// The one security property this package exists to guarantee: no request
// can ever read a path outside the repo's own clone directory, regardless
// of ".." segments, symlinks, or absolute-path tricks in the query param.
package fsread

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var ErrOutsideRepo = errors.New("path escapes repository root")
var ErrNotFound = errors.New("file not found")
var ErrIsDir = errors.New("path is a directory, not a file")

const maxReadBytes = 512 * 1024 // 512KB safety cap for the explorer view

// languageByExt is used to tell the frontend how to syntax-highlight a file.
// Kept intentionally small — extend as needed rather than trying to be exhaustive.
var languageByExt = map[string]string{
	".go": "go", ".ts": "typescript", ".tsx": "tsx", ".js": "javascript",
	".jsx": "jsx", ".py": "python", ".rb": "ruby", ".java": "java",
	".kt": "kotlin", ".rs": "rust", ".c": "c", ".h": "c", ".cpp": "cpp",
	".hpp": "cpp", ".cs": "csharp", ".php": "php", ".swift": "swift",
	".md": "markdown", ".mdx": "markdown", ".yml": "yaml", ".yaml": "yaml",
	".json": "json", ".toml": "toml", ".sql": "sql", ".sh": "bash",
	".html": "html", ".css": "css", ".scss": "scss", ".proto": "protobuf",
	".graphql": "graphql",
}

type FileContent struct {
	Path      string `json:"path"`
	Language  string `json:"language"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

// Resolve safely joins repoRoot and relPath, guaranteeing the result stays
// within repoRoot. This is the security-critical function in this package.
func Resolve(repoRoot, relPath string) (string, error) {
	// Reject absolute paths and empty input outright.
	if relPath == "" || filepath.IsAbs(relPath) {
		return "", ErrOutsideRepo
	}
	cleanRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	cleanRoot, err = filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(cleanRoot, relPath)
	// filepath.Join already collapses ".." where possible, but we still
	// verify the final path is a descendant of cleanRoot before touching disk.
	rel, err := filepath.Rel(cleanRoot, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrOutsideRepo
	}

	// Resolve symlinks on the final path too, then re-check containment,
	// so a symlink inside the repo can't point back out of it.
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	relResolved, err := filepath.Rel(cleanRoot, resolved)
	if err != nil || relResolved == ".." || strings.HasPrefix(relResolved, ".."+string(filepath.Separator)) {
		return "", ErrOutsideRepo
	}
	return resolved, nil
}

// Read returns the content of relPath within repoRoot, capped at maxReadBytes.
func Read(repoRoot, relPath string) (*FileContent, error) {
	full, err := Resolve(repoRoot, relPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, ErrIsDir
	}

	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, maxReadBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 && info.Size() > 0 {
		return nil, err
	}
	truncated := info.Size() > int64(n)

	return &FileContent{
		Path:      relPath,
		Language:  languageByExt[strings.ToLower(filepath.Ext(relPath))],
		Content:   string(buf[:n]),
		Size:      info.Size(),
		Truncated: truncated,
	}, nil
}
