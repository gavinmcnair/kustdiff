package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

type config struct {
	baseRef  string
	headRef  string
	rootDir  string
	maxDepth int
	debug    bool
}

func main() {
	cfg := parseConfig()
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func parseConfig() config {
	cfg := config{
		baseRef:  os.Getenv("INPUT_BASE_REF"),
		headRef:  os.Getenv("INPUT_HEAD_REF"),
		rootDir:  envOrDefault("INPUT_ROOT_DIR", "kustomize"),
		maxDepth: envIntOrDefault("INPUT_MAX_DEPTH", 2),
		debug:    os.Getenv("DEBUG") == "true",
	}
	flag.StringVar(&cfg.baseRef, "base", cfg.baseRef, "base git ref (branch, tag, or SHA)")
	flag.StringVar(&cfg.headRef, "head", cfg.headRef, "head git ref (branch, tag, or SHA)")
	flag.StringVar(&cfg.rootDir, "root", cfg.rootDir, "root directory containing kustomization files")
	flag.IntVar(&cfg.maxDepth, "depth", cfg.maxDepth, "max depth to search for kustomization.yaml")
	flag.BoolVar(&cfg.debug, "debug", cfg.debug, "enable debug logging")
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

	// Build both trees concurrently. Within each tree, all kustomize
	// targets are also built concurrently via a nested errgroup.
	var g errgroup.Group
	g.Go(func() error {
		debugf("building base (%s)", cfg.rootDir)
		return buildAll(baseTree, cfg.rootDir, cfg.maxDepth, baseOut)
	})
	g.Go(func() error {
		debugf("building head (%s)", cfg.rootDir)
		return buildAll(headTree, cfg.rootDir, cfg.maxDepth, headOut)
	})
	if err := g.Wait(); err != nil {
		return err
	}

	return runDiff(baseOut, headOut)
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
// that contains a kustomization.yaml, matching the behaviour of:
//
//	find <rootDir> -maxdepth <maxDepth> -name kustomization.yaml -exec dirname {} \;
func findTargets(rootDir string, maxDepth int) ([]string, error) {
	var targets []string
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
		if !d.IsDir() && d.Name() == "kustomization.yaml" {
			targets = append(targets, filepath.Dir(path))
		}
		return nil
	})
	return targets, err
}

// buildAll discovers kustomize targets inside worktreeDir/rootDir and builds
// them all concurrently, writing one YAML file per target into outDir.
func buildAll(worktreeDir, rootDir string, maxDepth int, outDir string) error {
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

// buildTarget runs kustomize on a single directory and writes the rendered
// YAML to outFile. Uses the krusty library directly — no subprocess.
func buildTarget(envPath, outFile string) error {
	opts := krusty.MakeDefaultOptions()
	if hasHelmCharts(filepath.Join(envPath, "kustomization.yaml")) {
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
