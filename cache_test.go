package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheGetMiss(t *testing.T) {
	t.Parallel()

	c, err := openCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatalf("openCache: %v", err)
	}
	data, ok := c.Get("nonexistent-key-abc123def456")
	if ok {
		t.Error("expected miss, got hit")
	}
	if data != nil {
		t.Errorf("expected nil data, got %d bytes", len(data))
	}
}

func TestCachePutGet(t *testing.T) {
	t.Parallel()

	c, err := openCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatalf("openCache: %v", err)
	}

	key := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	content := []byte("apiVersion: v1\nkind: ConfigMap\n")

	if err := c.Put(key, content); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := c.Get(key)
	if !ok {
		t.Fatal("expected hit, got miss")
	}
	if string(got) != string(content) {
		t.Errorf("got %q, want %q", string(got), string(content))
	}
}

func TestCacheNilSafe(t *testing.T) {
	t.Parallel()

	var c *cache

	// Get on nil should not panic and return false.
	data, ok := c.Get("somekey1234567890")
	if ok {
		t.Error("nil cache Get should return false")
	}
	if data != nil {
		t.Error("nil cache Get should return nil data")
	}

	// Put on nil should not panic.
	err := c.Put("somekey1234567890", []byte("data"))
	if err != nil {
		t.Errorf("nil cache Put should return nil, got: %v", err)
	}
}

func TestCachePathLayout(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "cache")
	c, err := openCache(dir)
	if err != nil {
		t.Fatalf("openCache: %v", err)
	}

	key := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	if err := c.Put(key, []byte("test")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Verify fan-out directory structure: v1/ab/<full-key>.yaml
	expected := filepath.Join(dir, "v1", "ab", key+".yaml")
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("expected cache file at %s, got error: %v", expected, err)
	}
}

func TestCacheKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	r := newTestRepo(t)
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	r.writeFile("kustomize/app/deployment.yaml", "apiVersion: apps/v1")
	r.commit("initial")
	cdRepo(t, r.dir)

	worktreeRoot := r.dir
	deps := []string{
		filepath.Join(r.dir, "kustomize", "app", "kustomization.yaml"),
		filepath.Join(r.dir, "kustomize", "app", "deployment.yaml"),
	}

	key1, err := cacheKey("main", "kustomize/app", deps, worktreeRoot)
	if err != nil {
		t.Fatalf("cacheKey: %v", err)
	}
	if key1 == "" {
		t.Fatal("expected non-empty key")
	}

	// Same inputs produce same key.
	key2, err := cacheKey("main", "kustomize/app", deps, worktreeRoot)
	if err != nil {
		t.Fatalf("cacheKey second call: %v", err)
	}
	if key1 != key2 {
		t.Errorf("same inputs should produce same key: %q != %q", key1, key2)
	}

	// Different content produces different key.
	r.writeFile("kustomize/app/deployment.yaml", "apiVersion: apps/v2")
	r.commit("change deployment")

	key3, err := cacheKey("main", "kustomize/app", deps, worktreeRoot)
	if err != nil {
		t.Fatalf("cacheKey after change: %v", err)
	}
	if key3 == key1 {
		t.Error("different content should produce different key")
	}
}

func TestOpenCache_EmptyDir(t *testing.T) {
	t.Parallel()

	c, err := openCache("")
	if err != nil {
		t.Fatalf("openCache with empty dir should not error: %v", err)
	}
	if c != nil {
		t.Error("expected nil cache for empty dir")
	}
}

func TestCacheKey_Format(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	r := newTestRepo(t)
	r.writeFile("file.txt", "hello")
	r.commit("init")
	cdRepo(t, r.dir)

	key, err := cacheKey("main", "file.txt", []string{filepath.Join(r.dir, "file.txt")}, r.dir)
	if err != nil {
		t.Fatalf("cacheKey: %v", err)
	}

	// Key should be a hex-encoded SHA256 (64 chars).
	if len(key) != 64 {
		t.Errorf("expected 64-char hex key, got %d chars: %s", len(key), key)
	}
	for _, c := range key {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("key contains non-hex char: %c", c)
		}
	}
}

func TestDefaultCacheDir(t *testing.T) {
	t.Run("uses XDG_CACHE_HOME if set", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "/tmp/xdg-test")
		got := defaultCacheDir()
		if got != "/tmp/xdg-test/kustdiff" {
			t.Errorf("got %q, want /tmp/xdg-test/kustdiff", got)
		}
	})

	t.Run("falls back to home/.cache", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "")
		got := defaultCacheDir()
		if !strings.HasSuffix(got, ".cache/kustdiff") {
			t.Errorf("expected path ending with .cache/kustdiff, got %q", got)
		}
	})
}
