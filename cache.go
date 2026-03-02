package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// cache provides content-addressed caching for rendered kustomize output.
// A nil *cache is safe to use — Get returns false and Put is a no-op.
type cache struct {
	dir string
}

// openCache creates (if needed) and returns a cache rooted at dir.
// Returns nil if dir is empty.
func openCache(dir string) (*cache, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Join(dir, "v1"), 0o755); err != nil {
		return nil, err
	}
	return &cache{dir: dir}, nil
}

// path returns the filesystem path for a cache key, using two-char fan-out.
func (c *cache) path(key string) string {
	return filepath.Join(c.dir, "v1", key[:2], key+".yaml")
}

// Get returns cached data for the given key, or (nil, false) on miss.
// Safe to call on a nil receiver.
func (c *cache) Get(key string) ([]byte, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, false
	}
	return data, true
}

// Put stores data under the given key. Safe to call on a nil receiver.
func (c *cache) Put(key string, data []byte) error {
	if c == nil || key == "" {
		return nil
	}
	p := c.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// cacheKey computes a content-addressed key for a target at a specific git ref.
// It hashes the sorted list of "path:hash" pairs where each hash comes from
// git rev-parse ref:path (tree hash for dirs, blob hash for files).
func cacheKey(ref, targetRepoPath string, deps []string, worktreeRoot string) (string, error) {
	// Deduplicate and collect repo-relative paths.
	seen := make(map[string]bool)
	var relPaths []string
	for _, dep := range deps {
		rel, err := filepath.Rel(worktreeRoot, dep)
		if err != nil {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		relPaths = append(relPaths, rel)
	}
	sort.Strings(relPaths)

	h := sha256.New()
	for _, rel := range relPaths {
		out, err := exec.Command("git", "rev-parse", ref+":"+rel).Output()
		if err != nil {
			// Skip paths that don't exist in the git tree (e.g. generated files).
			continue
		}
		hash := strings.TrimSpace(string(out))
		fmt.Fprintf(h, "%s:%s\n", rel, hash)
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
