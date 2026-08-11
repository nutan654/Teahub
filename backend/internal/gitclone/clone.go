// Package gitclone wraps shelling out to `git clone` for public repos.
//
// For an MVP this is simpler and more reliable than reimplementing the git
// protocol or depending on go-git; it's the same approach real internal
// tools (e.g. code search indexers) commonly use for a first version.
package gitclone

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var validRepoURL = regexp.MustCompile(`^https://github\.com/[\w.-]+/[\w.-]+/?$`)

// Normalize validates and cleans a GitHub repo URL (or "owner/repo" shorthand).
func Normalize(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("empty repo url")
	}
	// allow "owner/repo" shorthand
	if !strings.Contains(input, "://") && strings.Count(input, "/") == 1 {
		input = "https://github.com/" + input
	}
	input = strings.TrimSuffix(input, ".git")
	u, err := url.Parse(input)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if u.Host != "github.com" {
		return "", fmt.Errorf("only github.com repositories are supported in this MVP")
	}
	clean := fmt.Sprintf("https://github.com%s", strings.TrimSuffix(u.Path, "/"))
	if !validRepoURL.MatchString(clean + "/") {
		return "", fmt.Errorf("doesn't look like a valid github repo url: %s", input)
	}
	return clean, nil
}

// CloneInto shallow-clones repoURL into baseDir/repoID, creating
// directories as needed. Used for production imports so the clone lives
// at a stable, predictable path (baseDir is REPO_STORAGE_DIR — see
// config.Config.RepoStorageDir) that survives process restarts as long as
// the underlying volume does. On Cloud Run's default (ephemeral,
// non-persistent local disk), this path disappears when the instance is
// recycled; the api layer detects that (LocalPath missing) and
// transparently re-clones from repoURL rather than failing — see
// api.Server.runtimeRepo. Mounting a persistent volume (e.g. Cloud Run
// with a GCS FUSE or Filestore mount) removes the need for that fallback;
// see deploy/cloudrun.md.
func CloneInto(ctx context.Context, repoURL, baseDir, repoID string) (string, error) {
	dir := filepath.Join(baseDir, repoID)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clearing destination dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", fmt.Errorf("creating repo storage dir: %w", err)
	}
	if err := cloneInto(ctx, repoURL, dir); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// Remove deletes a clone's working tree from disk. Safe to call on a
// path that no longer exists.
func Remove(localPath string) error {
	if localPath == "" {
		return nil
	}
	return os.RemoveAll(localPath)
}

func cloneInto(ctx context.Context, repoURL, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--single-branch", repoURL, dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	// Drop .git to save space/avoid indexing it; we only need the working tree.
	os.RemoveAll(filepath.Join(dir, ".git"))
	return nil
}
