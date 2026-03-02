package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// kustomizationFileNames lists all filenames recognized as kustomization entry points.
var kustomizationFileNames = []string{
	"kustomization.yaml",
	"kustomization.yml",
	"Kustomization",
}

type config struct {
	baseRef    string
	headRef    string
	rootDir    string
	maxDepth   int
	debug      bool
	maxWorkers int
	fullBuild  bool
	noCache    bool
	cacheDir   string
}

func main() {
	cfg := parseConfig()
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func defaultCacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "kustdiff")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "kustdiff")
}

func parseConfig() config {
	cfg := config{
		baseRef:    os.Getenv("INPUT_BASE_REF"),
		headRef:    os.Getenv("INPUT_HEAD_REF"),
		rootDir:    envOrDefault("INPUT_ROOT_DIR", "kustomize"),
		maxDepth:   envIntOrDefault("INPUT_MAX_DEPTH", 2),
		debug:      os.Getenv("DEBUG") == "true",
		maxWorkers: envIntOrDefault("INPUT_WORKERS", runtime.NumCPU()),
		fullBuild:  os.Getenv("INPUT_FULL_BUILD") == "true",
		noCache:    os.Getenv("INPUT_NO_CACHE") == "true",
		cacheDir:   envOrDefault("INPUT_CACHE_DIR", defaultCacheDir()),
	}
	flag.StringVar(&cfg.baseRef, "base", cfg.baseRef, "base git ref (branch, tag, or SHA)")
	flag.StringVar(&cfg.headRef, "head", cfg.headRef, "head git ref (branch, tag, or SHA)")
	flag.StringVar(&cfg.rootDir, "root", cfg.rootDir, "root directory containing kustomization files")
	flag.IntVar(&cfg.maxDepth, "depth", cfg.maxDepth, "max depth to search for kustomization.yaml")
	flag.BoolVar(&cfg.debug, "debug", cfg.debug, "enable debug logging")
	flag.IntVar(&cfg.maxWorkers, "workers", cfg.maxWorkers, "max concurrent kustomize builds")
	flag.BoolVar(&cfg.fullBuild, "full-build", cfg.fullBuild, "skip narrowing, build all targets")
	flag.BoolVar(&cfg.noCache, "no-cache", cfg.noCache, "disable cache reads and writes")
	flag.StringVar(&cfg.cacheDir, "cache-dir", cfg.cacheDir, "cache directory location")
	flag.Parse()
	return cfg
}

func run(cfg config) error {
	if cfg.baseRef == "" {
		return fmt.Errorf("base ref required (-base or INPUT_BASE_REF)")
	}
	if cfg.headRef == "" {
		return fmt.Errorf("head ref required (-head or INPUT_HEAD_REF)")
	}

	debugf := func(format string, args ...any) {
		if cfg.debug {
			fmt.Fprintf(os.Stderr, "[debug] "+format+"\n", args...)
		}
	}

	tmpDir, err := os.MkdirTemp("", "kustdiff-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	baseRef, err := resolveRef(cfg.baseRef)
	if err != nil {
		return err
	}
	debugf("base %q -> %q", cfg.baseRef, baseRef)

	headRef, err := resolveRef(cfg.headRef)
	if err != nil {
		return err
	}
	debugf("head %q -> %q", cfg.headRef, headRef)

	// Determine changed files for narrowing (unless --full-build).
	var changed []string
	if !cfg.fullBuild {
		changed, err = changedFiles(baseRef, headRef)
		if err != nil {
			debugf("changedFiles failed, falling back to full build: %v", err)
			changed = nil // fall back to full build
		} else {
			debugf("changed files: %v", changed)
		}
	}

	baseTree := filepath.Join(tmpDir, "base")
	headTree := filepath.Join(tmpDir, "head")

	// Worktrees are created sequentially — git's index lock makes
	// concurrent worktree adds unreliable on some versions.
	if err := createWorktree(baseRef, baseTree); err != nil {
		return fmt.Errorf("create base worktree: %w", err)
	}
	defer removeWorktree(baseTree)

	if err := createWorktree(headRef, headTree); err != nil {
		return fmt.Errorf("create head worktree: %w", err)
	}
	defer removeWorktree(headTree)

	baseOut := filepath.Join(tmpDir, "base-out")
	headOut := filepath.Join(tmpDir, "head-out")
	if err := os.MkdirAll(baseOut, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(headOut, 0o755); err != nil {
		return err
	}

	// Open cache (unless --no-cache).
	var c *cache
	if !cfg.noCache {
		c, err = openCache(cfg.cacheDir)
		if err != nil {
			debugf("cache open failed, continuing without cache: %v", err)
		}
	}

	// Discover targets in both trees.
	baseAbsRoot := filepath.Join(baseTree, cfg.rootDir)
	headAbsRoot := filepath.Join(headTree, cfg.rootDir)

	baseTargets, _ := findTargetsIfExists(baseAbsRoot, cfg.maxDepth)
	headTargets, _ := findTargetsIfExists(headAbsRoot, cfg.maxDepth)

	// Apply narrowing if we have changed files.
	if changed != nil && !cfg.fullBuild {
		filteredBase := filterTargets(baseTargets, baseTree, cfg.rootDir, changed)
		filteredHead := filterTargets(headTargets, headTree, cfg.rootDir, changed)

		// Add orphan targets (new/removed targets that must always be built).
		baseOrphans := orphanTargets(baseTargets, baseAbsRoot, headTargets, headAbsRoot)
		headOrphans := orphanTargets(headTargets, headAbsRoot, baseTargets, baseAbsRoot)

		baseTargets = dedup(append(filteredBase, baseOrphans...))
		headTargets = dedup(append(filteredHead, headOrphans...))

		debugf("narrowed base targets: %d, head targets: %d", len(baseTargets), len(headTargets))
	}

	// Build both trees concurrently.
	var g errgroup.Group
	g.Go(func() error {
		debugf("building base (%d targets)", len(baseTargets))
		return buildFiltered(baseTargets, baseAbsRoot, baseOut, cfg.maxWorkers, c, baseRef, baseTree, debugf)
	})
	g.Go(func() error {
		debugf("building head (%d targets)", len(headTargets))
		return buildFiltered(headTargets, headAbsRoot, headOut, cfg.maxWorkers, c, headRef, headTree, debugf)
	})
	if err := g.Wait(); err != nil {
		return err
	}

	return runDiff(baseOut, headOut)
}

// findTargetsIfExists calls findTargets if the root exists, otherwise returns nil.
func findTargetsIfExists(absRoot string, maxDepth int) ([]string, error) {
	if _, err := os.Stat(absRoot); os.IsNotExist(err) {
		return nil, nil
	}
	return findTargets(absRoot, maxDepth)
}

// dedup removes duplicate strings from a slice, preserving order.
func dedup(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	var result []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// buildFiltered builds the given targets with optional caching.
func buildFiltered(targets []string, absRoot, outDir string, workers int, c *cache, ref, worktreeRoot string, debugf func(string, ...any)) error {
	var g errgroup.Group
	if workers > 0 {
		g.SetLimit(workers)
	}
	for _, target := range targets {
		target := target
		rel, _ := filepath.Rel(absRoot, target)
		outFile := filepath.Join(outDir, safeFilename(rel)+".yaml")
		g.Go(func() error {
			// Try cache lookup.
			if c != nil {
				deps, err := collectDeps(target, make(map[string]bool))
				if err == nil {
					key, err := cacheKey(ref, rel, deps, worktreeRoot)
					if err == nil && key != "" {
						if data, ok := c.Get(key); ok {
							debugf("cache hit for %s", rel)
							return os.WriteFile(outFile, data, 0o644)
						}
						// Cache miss — build and store.
						if err := buildTarget(target, outFile); err != nil {
							return err
						}
						data, err := os.ReadFile(outFile)
						if err == nil {
							_ = c.Put(key, data)
						}
						return nil
					}
				}
				// Dep parse or cache key failed — fall through to plain build.
				debugf("cache key computation failed for %s, building fresh", rel)
			}
			return buildTarget(target, outFile)
		})
	}
	return g.Wait()
}

// resolveRef resolves a human-friendly ref to something git can checkout.
// Preference order: local branch → remote branch → commit SHA / tag.
func resolveRef(ref string) (string, error) {
	for _, c := range []struct{ gitRef, display string }{
		{"refs/heads/" + ref, ref},
		{"refs/remotes/origin/" + ref, "origin/" + ref},
	} {
		if exec.Command("git", "show-ref", "--verify", "--quiet", c.gitRef).Run() == nil {
			return c.display, nil
		}
	}
	if exec.Command("git", "cat-file", "-e", ref+"^{commit}").Run() == nil {
		return ref, nil
	}
	return "", fmt.Errorf("cannot resolve ref %q: not a local branch, remote branch, or commit SHA", ref)
}

func createWorktree(ref, path string) error {
	out, err := exec.Command("git", "worktree", "add", "--detach", "--quiet", path, ref).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeWorktree(path string) {
	exec.Command("git", "worktree", "remove", "--force", path).Run() //nolint:errcheck
}

// findTargets walks rootDir up to maxDepth levels and returns every directory
// that contains a kustomization file (kustomization.yaml, kustomization.yml,
// or Kustomization).
func findTargets(rootDir string, maxDepth int) ([]string, error) {
	var targets []string
	seen := make(map[string]bool)
	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(rootDir, path)
		if rel == "." {
			return nil
		}
		depth := len(strings.Split(rel, string(os.PathSeparator)))
		if d.IsDir() && depth >= maxDepth {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			for _, name := range kustomizationFileNames {
				if d.Name() == name {
					dir := filepath.Dir(path)
					if !seen[dir] {
						seen[dir] = true
						targets = append(targets, dir)
					}
					break
				}
			}
		}
		return nil
	})
	return targets, err
}

// buildAll discovers kustomize targets inside worktreeDir/rootDir and builds
// them all concurrently, writing one YAML file per target into outDir.
func buildAll(worktreeDir, rootDir string, maxDepth, workers int, outDir string) error {
	absRoot := filepath.Join(worktreeDir, rootDir)
	if _, err := os.Stat(absRoot); os.IsNotExist(err) {
		// rootDir doesn't exist in this tree (e.g. PR introduces it for the first time).
		return nil
	}

	targets, err := findTargets(absRoot, maxDepth)
	if err != nil {
		return fmt.Errorf("find targets in %s: %w", absRoot, err)
	}

	var g errgroup.Group
	if workers > 0 {
		g.SetLimit(workers)
	}
	for _, target := range targets {
		target := target
		rel, _ := filepath.Rel(absRoot, target)
		outFile := filepath.Join(outDir, safeFilename(rel)+".yaml")
		g.Go(func() error {
			return buildTarget(target, outFile)
		})
	}
	return g.Wait()
}

// findKustomizationFile returns the path to the kustomization file in dir,
// or an empty string if none is found.
func findKustomizationFile(dir string) string {
	for _, name := range kustomizationFileNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// buildTarget runs kustomize on a single directory and writes the rendered
// YAML to outFile. Uses the krusty library directly — no subprocess.
func buildTarget(envPath, outFile string) error {
	opts := krusty.MakeDefaultOptions()
	if kf := findKustomizationFile(envPath); kf != "" && hasHelmCharts(kf) {
		opts.PluginConfig.HelmConfig.Enabled = true
		opts.PluginConfig.HelmConfig.Command = "helm"
	}

	k := krusty.MakeKustomizer(opts)
	m, err := k.Run(filesys.MakeFsOnDisk(), envPath)
	if err != nil {
		return fmt.Errorf("kustomize build %s: %w", envPath, err)
	}
	yaml, err := m.AsYaml()
	if err != nil {
		return fmt.Errorf("serialize output for %s: %w", envPath, err)
	}
	return os.WriteFile(outFile, yaml, 0o644)
}

func hasHelmCharts(path string) bool {
	data, _ := os.ReadFile(path)
	return strings.Contains(string(data), "helmCharts:")
}

// safeFilename replaces any character that isn't alphanumeric or '.' with '_',
// so that a relative path like "env/prod" becomes "env_prod" as a filename.
func safeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' {
			return r
		}
		return '_'
	}, s)
}

// runDiff execs git diff --no-index and streams the result to stdout.
// git diff exits 1 when differences are found, which is not an error here.
func runDiff(baseOut, headOut string) error {
	cmd := exec.Command("git", "diff", "--no-index", baseOut, headOut)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return err
	}
	fmt.Println("No differences found.")
	return nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}
