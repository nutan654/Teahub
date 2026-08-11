// Package parser walks a cloned repo on disk and produces:
//  1. A FileNode tree (for the UI's file explorer)
//  2. A map of path -> text content for files worth indexing (for AI Q&A retrieval)
package parser

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"teahub/backend/internal/store"
)

// skipDirs are directories we never descend into: build artifacts,
// dependency trees, and VCS metadata add noise without insight.
var skipDirs = map[string]bool{
	"node_modules": true, ".git": true, "vendor": true, "dist": true,
	"build": true, ".next": true, "__pycache__": true, ".venv": true,
	"venv": true, "target": true, ".idea": true, ".vscode": true,
}

// textExtensions is an allowlist of extensions we treat as readable source
// for the AI context. Anything else (images, binaries, lockfiles) is still
// shown in the tree but not read into memory.
var textExtensions = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".py": true, ".rb": true, ".java": true, ".kt": true, ".rs": true,
	".c": true, ".h": true, ".cpp": true, ".hpp": true, ".cs": true,
	".php": true, ".swift": true, ".md": true, ".mdx": true, ".yml": true,
	".yaml": true, ".json": true, ".toml": true, ".sql": true, ".sh": true,
	".html": true, ".css": true, ".scss": true, ".proto": true, ".graphql": true,
}

const maxFileBytes = 64 * 1024 // skip indexing anything bigger than 64KB
const maxIndexedFiles = 2000   // safety cap for very large repos

// Walk builds the tree and the indexed-file map for repoRoot.
func Walk(repoRoot string) (*store.FileNode, map[string]string, int, error) {
	root := &store.FileNode{Path: "", IsDir: true}
	nodeByDir := map[string]*store.FileNode{"": root}
	indexed := map[string]string{}
	fileCount := 0

	err := filepath.Walk(repoRoot, func(fullPath string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than failing the whole import
		}
		rel, _ := filepath.Rel(repoRoot, fullPath)
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			node := &store.FileNode{Path: rel, IsDir: true}
			nodeByDir[rel] = node
			parent := nodeByDir[filepath.Dir(rel)]
			if filepath.Dir(rel) == "." {
				parent = root
			}
			parent.Children = append(parent.Children, node)
			return nil
		}

		fileCount++
		node := &store.FileNode{Path: rel, Size: info.Size()}
		parentDir := filepath.Dir(rel)
		parent := nodeByDir[parentDir]
		if parentDir == "." {
			parent = root
		}
		if parent != nil {
			parent.Children = append(parent.Children, node)
		}

		ext := strings.ToLower(filepath.Ext(rel))
		if textExtensions[ext] && info.Size() > 0 && info.Size() <= maxFileBytes && len(indexed) < maxIndexedFiles {
			data, err := os.ReadFile(fullPath)
			if err == nil {
				indexed[rel] = string(data)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}

	sortTree(root)
	return root, indexed, fileCount, nil
}

func sortTree(n *store.FileNode) {
	sort.Slice(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if a.IsDir != b.IsDir {
			return a.IsDir // directories first
		}
		return a.Path < b.Path
	})
	for _, c := range n.Children {
		if c.IsDir {
			sortTree(c)
		}
	}
}
