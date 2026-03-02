package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================
// Test helpers
// ============================================================

// testRepo is a temporary git repository for use in tests.
type testRepo struct {
	dir string
	t   *testing.T
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	r := &testRepo{dir: dir, t: t}
	r.mustGit("init")
	// Set initial branch to 'main' without requiring git >= 2.28 (-b flag).
	r.mustGit("symbolic-ref", "HEAD", "refs/heads/main")
	r.mustGit("config", "user.email", "test@example.com")
	r.mustGit("config", "user.name", "Test")
	return r
}

func (r *testRepo) mustGit(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *testRepo) writeFile(rel, content string) {
	r.t.Helper()
	full := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", rel, err)
	}
}

func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	r.mustGit("add", ".")
	r.mustGit("commit", "-m", msg)
	return r.mustGit("rev-parse", "HEAD")
}

func (r *testRepo) branch(name string) {
	r.t.Helper()
	r.mustGit("checkout", "-b", name)
}

func (r *testRepo) checkout(ref string) {
	r.t.Helper()
	r.mustGit("checkout", ref)
}

// cdRepo changes the process working directory for the duration of the test.
// Tests using cdRepo must NOT call t.Parallel() — CWD is process-wide.
func cdRepo(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

// captureStdout captures everything written to os.Stdout during fn.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = pw
	fn()
	pw.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, pr)
	pr.Close()
	return buf.String()
}

// kustomizeApp returns a self-contained kustomization that generates a
// ConfigMap from literals. No external resource files needed.
func kustomizeApp(version string) string {
	return fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
configMapGenerator:
  - name: app-config
    literals:
      - VERSION=%s
`, version)
}

// ============================================================
// Unit tests — no external dependencies, safe to parallelise
// ============================================================

func TestSafeFilename(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"env/prod", "env_prod"},
		{"env/staging/v2", "env_staging_v2"},
		{"already_safe", "already_safe"},
		{"dots.are.ok", "dots.are.ok"},
		{"with spaces", "with_spaces"},
		{"123numbers", "123numbers"},
		{"MiXeD-CaSe", "MiXeD_CaSe"},
		{"", ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			if got := safeFilename(c.in); got != c.want {
				t.Errorf("safeFilename(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestHasHelmCharts(t *testing.T) {
	t.Parallel()

	t.Run("present", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(t.TempDir(), "kustomization.yaml")
		os.WriteFile(f, []byte("helmCharts:\n  - name: nginx\n"), 0o644)
		if !hasHelmCharts(f) {
			t.Error("want true, got false")
		}
	})

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(t.TempDir(), "kustomization.yaml")
		os.WriteFile(f, []byte(kustomizeApp("1.0")), 0o644)
		if hasHelmCharts(f) {
			t.Error("want false, got true")
		}
	})

	t.Run("missing file returns false", func(t *testing.T) {
		t.Parallel()
		if hasHelmCharts("/does/not/exist.yaml") {
			t.Error("want false for missing file, got true")
		}
	})
}

func TestEnvOrDefault(t *testing.T) {
	t.Run("env set", func(t *testing.T) {
		t.Setenv("_KUSTDIFF_TEST_KEY", "hello")
		if got := envOrDefault("_KUSTDIFF_TEST_KEY", "default"); got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})
	t.Run("env unset", func(t *testing.T) {
		if got := envOrDefault("_KUSTDIFF_DEFINITELY_UNSET", "default"); got != "default" {
			t.Errorf("got %q, want %q", got, "default")
		}
	})
}

func TestEnvIntOrDefault(t *testing.T) {
	t.Run("valid integer", func(t *testing.T) {
		t.Setenv("_KUSTDIFF_INT", "5")
		if got := envIntOrDefault("_KUSTDIFF_INT", 2); got != 5 {
			t.Errorf("got %d, want 5", got)
		}
	})
	t.Run("non-integer falls back to default", func(t *testing.T) {
		t.Setenv("_KUSTDIFF_INT_BAD", "banana")
		if got := envIntOrDefault("_KUSTDIFF_INT_BAD", 2); got != 2 {
			t.Errorf("got %d, want 2", got)
		}
	})
	t.Run("unset returns default", func(t *testing.T) {
		if got := envIntOrDefault("_KUSTDIFF_INT_UNSET", 7); got != 7 {
			t.Errorf("got %d, want 7", got)
		}
	})
}

func TestFindTargets(t *testing.T) {
	t.Parallel()

	// Layout:
	//   root/
	//     app/kustomization.yaml          <- depth 1 from root
	//     app/other.yaml                  <- ignored
	//     overlays/prod/kustomization.yaml <- depth 2 from root
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	mustWrite("app/kustomization.yaml", "")
	mustWrite("app/other.yaml", "")
	mustWrite("overlays/prod/kustomization.yaml", "")

	t.Run("maxDepth=2 finds only depth-1 targets", func(t *testing.T) {
		t.Parallel()
		targets, err := findTargets(root, 2)
		if err != nil {
			t.Fatalf("findTargets: %v", err)
		}
		if len(targets) != 1 {
			t.Fatalf("got %d targets, want 1: %v", len(targets), targets)
		}
		if filepath.Base(targets[0]) != "app" {
			t.Errorf("unexpected target: %s", targets[0])
		}
	})

	t.Run("maxDepth=3 finds all targets", func(t *testing.T) {
		t.Parallel()
		targets, err := findTargets(root, 3)
		if err != nil {
			t.Fatalf("findTargets: %v", err)
		}
		if len(targets) != 2 {
			t.Errorf("got %d targets, want 2: %v", len(targets), targets)
		}
	})

	t.Run("maxDepth=1 finds nothing (root itself has no kustomization.yaml)", func(t *testing.T) {
		t.Parallel()
		targets, err := findTargets(root, 1)
		if err != nil {
			t.Fatalf("findTargets: %v", err)
		}
		if len(targets) != 0 {
			t.Errorf("got %d targets, want 0: %v", len(targets), targets)
		}
	})

	t.Run("nonexistent root returns error", func(t *testing.T) {
		t.Parallel()
		_, err := findTargets(filepath.Join(t.TempDir(), "ghost"), 2)
		if err == nil {
			t.Error("expected error for nonexistent root")
		}
	})
}

// ============================================================
// Kustomize library tests — no git needed, safe to parallelise
// ============================================================

func TestBuildTarget_ConfigMap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomizeApp("3.0")), 0o644)

	outFile := filepath.Join(t.TempDir(), "out.yaml")
	if err := buildTarget(dir, outFile); err != nil {
		t.Fatalf("buildTarget: %v", err)
	}
	data, _ := os.ReadFile(outFile)
	out := string(data)
	if !strings.Contains(out, "VERSION") {
		t.Errorf("expected VERSION in output:\n%s", out)
	}
	if !strings.Contains(out, "3.0") {
		t.Errorf("expected 3.0 in output:\n%s", out)
	}
	if !strings.Contains(out, "app-config") {
		t.Errorf("expected configmap name app-config in output:\n%s", out)
	}
}

func TestBuildTarget_InvalidKustomization(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Reference a resource that does not exist.
	os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - does-not-exist.yaml
`), 0o644)

	err := buildTarget(dir, filepath.Join(t.TempDir(), "out.yaml"))
	if err == nil {
		t.Error("expected error for missing resource, got nil")
	}
}

func TestBuildAll_MissingRoot(t *testing.T) {
	t.Parallel()
	// If rootDir doesn't exist in the worktree, buildAll should succeed silently.
	err := buildAll(t.TempDir(), "nonexistent-root", 2, 4, t.TempDir())
	if err != nil {
		t.Errorf("expected nil for missing root, got: %v", err)
	}
}

func TestBuildAll_MultipleTargets(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	outDir := t.TempDir()

	for _, env := range []string{"app", "worker"} {
		dir := filepath.Join(worktree, "kustomize", env)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomizeApp("1.0")), 0o644)
	}

	if err := buildAll(worktree, "kustomize", 2, 4, outDir); err != nil {
		t.Fatalf("buildAll: %v", err)
	}

	entries, _ := os.ReadDir(outDir)
	if len(entries) != 2 {
		t.Errorf("expected 2 output files, got %d", len(entries))
	}
}

// ============================================================
// Git integration tests — change CWD, do NOT parallelise
// ============================================================

func TestResolveRef(t *testing.T) {
	r := newTestRepo(t)
	r.writeFile("README.md", "init")
	sha := r.commit("initial commit")
	cdRepo(t, r.dir)

	t.Run("local branch resolves to branch name", func(t *testing.T) {
		got, err := resolveRef("main")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "main" {
			t.Errorf("got %q, want %q", got, "main")
		}
	})

	t.Run("full commit SHA resolves to itself", func(t *testing.T) {
		got, err := resolveRef(sha)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != sha {
			t.Errorf("got %q, want %q", got, sha)
		}
	})

	t.Run("unknown ref returns descriptive error", func(t *testing.T) {
		_, err := resolveRef("no-such-branch-xyz")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "no-such-branch-xyz") {
			t.Errorf("error should name the bad ref: %v", err)
		}
	})
}

// ============================================================
// End-to-end run() tests — change CWD, do NOT parallelise
// ============================================================

func TestRun_ValidationErrors(t *testing.T) {
	t.Run("missing base ref", func(t *testing.T) {
		err := run(config{headRef: "main", rootDir: "kustomize", maxDepth: 2})
		if err == nil || !strings.Contains(err.Error(), "base ref") {
			t.Errorf("expected base ref error, got: %v", err)
		}
	})

	t.Run("missing head ref", func(t *testing.T) {
		err := run(config{baseRef: "main", rootDir: "kustomize", maxDepth: 2})
		if err == nil || !strings.Contains(err.Error(), "head ref") {
			t.Errorf("expected head ref error, got: %v", err)
		}
	})
}

func TestRun_NoDifferences(t *testing.T) {
	r := newTestRepo(t)
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	r.commit("initial")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "main",
			rootDir: "kustomize", maxDepth: 2,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	if !strings.Contains(out, "No differences") {
		t.Errorf("expected no-differences message, got:\n%s", out)
	}
}

func TestRun_WithDifferences(t *testing.T) {
	r := newTestRepo(t)
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	r.commit("initial: v1.0")
	r.branch("feature")
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("2.0"))
	r.commit("bump to v2.0")
	r.checkout("main")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "feature",
			rootDir: "kustomize", maxDepth: 2,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	for _, want := range []string{"VERSION", "1.0", "2.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in diff output, got:\n%s", want, out)
		}
	}
}

func TestRun_NewTargetInHead(t *testing.T) {
	r := newTestRepo(t)
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	r.commit("initial")
	r.branch("feature")
	r.writeFile("kustomize/worker/kustomization.yaml", kustomizeApp("1.0"))
	r.commit("add worker environment")
	r.checkout("main")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "feature",
			rootDir: "kustomize", maxDepth: 2,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	// worker is a pure addition — it appears in the diff path
	if !strings.Contains(out, "worker") {
		t.Errorf("expected 'worker' in diff output (new environment), got:\n%s", out)
	}
}

func TestRun_RemovedTargetInHead(t *testing.T) {
	r := newTestRepo(t)
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	r.writeFile("kustomize/legacy/kustomization.yaml", kustomizeApp("1.0"))
	r.commit("initial with two environments")
	r.branch("feature")
	os.RemoveAll(filepath.Join(r.dir, "kustomize", "legacy"))
	r.commit("remove legacy environment")
	r.checkout("main")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "feature",
			rootDir: "kustomize", maxDepth: 2,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	// legacy appears as a pure deletion
	if !strings.Contains(out, "legacy") {
		t.Errorf("expected 'legacy' in diff output (removed environment), got:\n%s", out)
	}
}

func TestRun_NoKustomizeRoot(t *testing.T) {
	// rootDir doesn't exist in either tree — should succeed with no diff.
	r := newTestRepo(t)
	r.writeFile("README.md", "no kustomize here")
	r.commit("initial")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "main",
			rootDir: "kustomize", maxDepth: 2,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	if !strings.Contains(out, "No differences") {
		t.Errorf("expected no-differences message, got:\n%s", out)
	}
}

func TestRun_CommitSHAAsRef(t *testing.T) {
	r := newTestRepo(t)
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	baseSHA := r.commit("initial")
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("2.0"))
	headSHA := r.commit("bump version")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: baseSHA, headRef: headSHA,
			rootDir: "kustomize", maxDepth: 2,
		}); err != nil {
			t.Fatalf("run with SHA refs: %v", err)
		}
	})

	if !strings.Contains(out, "VERSION") {
		t.Errorf("expected VERSION in diff output, got:\n%s", out)
	}
}

// ============================================================
// E2E tests for narrowing and caching
// ============================================================

func TestRun_NarrowingSkipsUnchanged(t *testing.T) {
	r := newTestRepo(t)
	// Two targets: app and worker.
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	r.writeFile("kustomize/worker/kustomization.yaml", kustomizeApp("1.0"))
	r.commit("initial with two targets")
	r.branch("feature")
	// Only change app.
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("2.0"))
	r.commit("bump app version")
	r.checkout("main")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "feature",
			rootDir: "kustomize", maxDepth: 2,
			noCache: true,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	// Should show diff for app (version change).
	if !strings.Contains(out, "app") {
		t.Errorf("expected 'app' in diff output, got:\n%s", out)
	}
	// Worker shouldn't appear in diff since it's unchanged.
	if strings.Contains(out, "worker") {
		t.Errorf("worker should not appear in diff (unchanged), got:\n%s", out)
	}
}

func TestRun_SharedBaseTriggersOverlays(t *testing.T) {
	r := newTestRepo(t)

	// Shared base.
	r.writeFile("kustomize/base/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
configMapGenerator:
  - name: shared-config
    literals:
      - SHARED=v1
`)
	// Two overlays referencing the base.
	for _, env := range []string{"staging", "prod"} {
		r.writeFile("kustomize/"+env+"/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../base
`)
	}
	r.commit("initial with shared base and overlays")
	r.branch("feature")
	// Change the shared base.
	r.writeFile("kustomize/base/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
configMapGenerator:
  - name: shared-config
    literals:
      - SHARED=v2
`)
	r.commit("bump shared base")
	r.checkout("main")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "feature",
			rootDir: "kustomize", maxDepth: 3,
			noCache: true,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	// Both overlays should appear in the diff because their base changed.
	if !strings.Contains(out, "staging") {
		t.Errorf("expected 'staging' in diff output, got:\n%s", out)
	}
	if !strings.Contains(out, "prod") {
		t.Errorf("expected 'prod' in diff output, got:\n%s", out)
	}
}

func TestRun_FullBuildFlag(t *testing.T) {
	r := newTestRepo(t)
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	r.commit("initial")
	r.branch("feature")
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("2.0"))
	r.commit("bump version")
	r.checkout("main")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "feature",
			rootDir: "kustomize", maxDepth: 2,
			fullBuild: true, noCache: true,
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	if !strings.Contains(out, "VERSION") {
		t.Errorf("expected VERSION in diff output, got:\n%s", out)
	}
}

func TestRun_CacheHit(t *testing.T) {
	r := newTestRepo(t)
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	r.commit("initial")
	r.branch("feature")
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("2.0"))
	r.commit("bump version")
	r.checkout("main")
	cdRepo(t, r.dir)

	cacheDir := filepath.Join(t.TempDir(), "test-cache")

	// First run: populates cache.
	out1 := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "feature",
			rootDir: "kustomize", maxDepth: 2,
			cacheDir: cacheDir,
		}); err != nil {
			t.Fatalf("first run: %v", err)
		}
	})

	// Verify cache was populated (at least one file in the cache dir).
	matches, _ := filepath.Glob(filepath.Join(cacheDir, "v1", "*", "*.yaml"))
	if len(matches) == 0 {
		t.Fatal("expected cache to be populated after first run")
	}

	// Second run: should use cache and produce equivalent diff content.
	out2 := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "feature",
			rootDir: "kustomize", maxDepth: 2,
			cacheDir: cacheDir,
		}); err != nil {
			t.Fatalf("second run: %v", err)
		}
	})

	// Both runs should contain the same version change markers.
	for _, marker := range []string{"VERSION", "1.0", "2.0"} {
		if !strings.Contains(out1, marker) {
			t.Errorf("first run missing %q in output", marker)
		}
		if !strings.Contains(out2, marker) {
			t.Errorf("second run missing %q in output (cache hit)", marker)
		}
	}
}

func TestRun_WorkersFlag(t *testing.T) {
	r := newTestRepo(t)
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("1.0"))
	r.writeFile("kustomize/worker/kustomization.yaml", kustomizeApp("1.0"))
	r.commit("initial")
	r.branch("feature")
	r.writeFile("kustomize/app/kustomization.yaml", kustomizeApp("2.0"))
	r.writeFile("kustomize/worker/kustomization.yaml", kustomizeApp("2.0"))
	r.commit("bump both")
	r.checkout("main")
	cdRepo(t, r.dir)

	out := captureStdout(t, func() {
		if err := run(config{
			baseRef: "main", headRef: "feature",
			rootDir: "kustomize", maxDepth: 2,
			maxWorkers: 1, fullBuild: true, noCache: true,
		}); err != nil {
			t.Fatalf("run with workers=1: %v", err)
		}
	})

	if !strings.Contains(out, "VERSION") {
		t.Errorf("expected VERSION in diff output, got:\n%s", out)
	}
}

func TestFindTargets_AllFileNames(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	mustWrite("a/kustomization.yaml", "")
	mustWrite("b/kustomization.yml", "")
	mustWrite("c/Kustomization", "")
	mustWrite("d/not-a-kustomization.yaml", "")

	targets, err := findTargets(root, 2)
	if err != nil {
		t.Fatalf("findTargets: %v", err)
	}
	if len(targets) != 3 {
		t.Errorf("expected 3 targets (a, b, c), got %d: %v", len(targets), targets)
	}

	names := make(map[string]bool)
	for _, tgt := range targets {
		names[filepath.Base(tgt)] = true
	}
	for _, want := range []string{"a", "b", "c"} {
		if !names[want] {
			t.Errorf("expected target %q, found: %v", want, names)
		}
	}
}
