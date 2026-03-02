package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindKustomizationFile(t *testing.T) {
	t.Parallel()

	t.Run("kustomization.yaml", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(""), 0o644)
		got := findKustomizationFile(dir)
		if filepath.Base(got) != "kustomization.yaml" {
			t.Errorf("got %q, want kustomization.yaml", got)
		}
	})

	t.Run("kustomization.yml", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "kustomization.yml"), []byte(""), 0o644)
		got := findKustomizationFile(dir)
		if filepath.Base(got) != "kustomization.yml" {
			t.Errorf("got %q, want kustomization.yml", got)
		}
	})

	t.Run("Kustomization", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "Kustomization"), []byte(""), 0o644)
		got := findKustomizationFile(dir)
		if filepath.Base(got) != "Kustomization" {
			t.Errorf("got %q, want Kustomization", got)
		}
	})

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		got := findKustomizationFile(dir)
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})

	t.Run("prefers yaml over yml", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(""), 0o644)
		os.WriteFile(filepath.Join(dir, "kustomization.yml"), []byte(""), 0o644)
		got := findKustomizationFile(dir)
		if filepath.Base(got) != "kustomization.yaml" {
			t.Errorf("got %q, want kustomization.yaml (first in priority)", got)
		}
	})
}

func TestLoadKustomization(t *testing.T) {
	t.Parallel()

	t.Run("basic fields", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		content := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
patches:
  - path: patch.yaml
`
		path := filepath.Join(dir, "kustomization.yaml")
		os.WriteFile(path, []byte(content), 0o644)
		k, err := loadKustomization(path)
		if err != nil {
			t.Fatalf("loadKustomization: %v", err)
		}
		if len(k.Resources) != 1 || k.Resources[0] != "deployment.yaml" {
			t.Errorf("resources = %v, want [deployment.yaml]", k.Resources)
		}
		if len(k.Patches) != 1 || k.Patches[0].Path != "patch.yaml" {
			t.Errorf("patches = %v, want [{Path: patch.yaml}]", k.Patches)
		}
	})

	t.Run("FixKustomization merges bases into resources", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		content := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
bases:
  - ../base
`
		path := filepath.Join(dir, "kustomization.yaml")
		os.WriteFile(path, []byte(content), 0o644)
		k, err := loadKustomization(path)
		if err != nil {
			t.Fatalf("loadKustomization: %v", err)
		}
		if len(k.Resources) != 2 {
			t.Errorf("resources = %v, want 2 entries (deployment.yaml + ../base)", k.Resources)
		}
		if len(k.Bases) != 0 {
			t.Errorf("bases should be nil after FixKustomization, got %v", k.Bases)
		}
	})
}

func TestCollectDeps_Simple(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
patches:
  - path: patch.yaml
configMapGenerator:
  - name: config
    files:
      - config.json
    envs:
      - .env
`), 0o644)
	// Create the referenced files so they resolve.
	os.WriteFile(filepath.Join(dir, "deployment.yaml"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "patch.yaml"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte(""), 0o644)

	deps, err := collectDeps(dir, make(map[string]bool))
	if err != nil {
		t.Fatalf("collectDeps: %v", err)
	}

	// Should contain: kustomization.yaml, deployment.yaml, patch.yaml, config.json, .env
	want := []string{"kustomization.yaml", "deployment.yaml", "patch.yaml", "config.json", ".env"}
	for _, w := range want {
		found := false
		for _, d := range deps {
			if filepath.Base(d) == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected dep %q not found in %v", w, deps)
		}
	}
}

func TestCollectDeps_WithBase(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Create base directory.
	baseDir := filepath.Join(root, "base")
	os.MkdirAll(baseDir, 0o755)
	os.WriteFile(filepath.Join(baseDir, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
`), 0o644)
	os.WriteFile(filepath.Join(baseDir, "deployment.yaml"), []byte(""), 0o644)

	// Create overlay directory referencing base.
	overlayDir := filepath.Join(root, "overlay")
	os.MkdirAll(overlayDir, 0o755)
	os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../base
`), 0o644)

	deps, err := collectDeps(overlayDir, make(map[string]bool))
	if err != nil {
		t.Fatalf("collectDeps: %v", err)
	}

	// Should include both overlay and base deps.
	hasBaseDep := false
	hasBaseKustomization := false
	for _, d := range deps {
		if filepath.Base(d) == "deployment.yaml" && strings.Contains(d, "base") {
			hasBaseDep = true
		}
		if filepath.Base(d) == "kustomization.yaml" && strings.Contains(d, "base") {
			hasBaseKustomization = true
		}
	}
	if !hasBaseDep {
		t.Errorf("expected base/deployment.yaml in deps: %v", deps)
	}
	if !hasBaseKustomization {
		t.Errorf("expected base/kustomization.yaml in deps: %v", deps)
	}
}

func TestCollectDeps_CycleDetection(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	os.MkdirAll(dirA, 0o755)
	os.MkdirAll(dirB, 0o755)

	os.WriteFile(filepath.Join(dirA, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../b
`), 0o644)
	os.WriteFile(filepath.Join(dirB, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../a
`), 0o644)

	// Should terminate without error (cycle detection).
	_, err := collectDeps(dirA, make(map[string]bool))
	if err != nil {
		t.Fatalf("collectDeps should handle cycles gracefully, got: %v", err)
	}
}

func TestCollectDeps_HelmChart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
helmCharts:
  - name: nginx
    valuesFile: values.yaml
    additionalValuesFiles:
      - values-prod.yaml
`), 0o644)
	os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "values-prod.yaml"), []byte(""), 0o644)
	os.MkdirAll(filepath.Join(dir, "charts", "nginx"), 0o755)

	deps, err := collectDeps(dir, make(map[string]bool))
	if err != nil {
		t.Fatalf("collectDeps: %v", err)
	}

	wantPaths := []string{"charts/nginx", "values.yaml", "values-prod.yaml"}
	for _, w := range wantPaths {
		found := false
		for _, d := range deps {
			if strings.HasSuffix(d, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in deps: %v", w, deps)
		}
	}
}

func TestCollectDeps_URLsSkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://github.com/example/repo
  - local.yaml
`), 0o644)
	os.WriteFile(filepath.Join(dir, "local.yaml"), []byte(""), 0o644)

	deps, err := collectDeps(dir, make(map[string]bool))
	if err != nil {
		t.Fatalf("collectDeps: %v", err)
	}

	for _, d := range deps {
		if strings.Contains(d, "://") {
			t.Errorf("URL should not appear in deps: %s", d)
		}
	}
	hasLocal := false
	for _, d := range deps {
		if filepath.Base(d) == "local.yaml" {
			hasLocal = true
		}
	}
	if !hasLocal {
		t.Errorf("expected local.yaml in deps: %v", deps)
	}
}

func TestCollectDeps_GeneratorFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
configMapGenerator:
  - name: cm1
    files:
      - mykey=data.txt
    envs:
      - env.properties
secretGenerator:
  - name: sec1
    files:
      - secret.txt
    envs:
      - secret.env
`), 0o644)
	for _, f := range []string{"data.txt", "env.properties", "secret.txt", "secret.env"} {
		os.WriteFile(filepath.Join(dir, f), []byte(""), 0o644)
	}

	deps, err := collectDeps(dir, make(map[string]bool))
	if err != nil {
		t.Fatalf("collectDeps: %v", err)
	}

	want := []string{"data.txt", "env.properties", "secret.txt", "secret.env"}
	for _, w := range want {
		found := false
		for _, d := range deps {
			if filepath.Base(d) == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in deps: %v", w, deps)
		}
	}
}

func TestCollectDeps_PatchesStrategicMerge(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
patchesStrategicMerge:
  - patch-file.yaml
  - |-
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: test
    spec:
      replicas: 3
`), 0o644)
	os.WriteFile(filepath.Join(dir, "patch-file.yaml"), []byte(""), 0o644)

	deps, err := collectDeps(dir, make(map[string]bool))
	if err != nil {
		t.Fatalf("collectDeps: %v", err)
	}

	hasPatchFile := false
	for _, d := range deps {
		if filepath.Base(d) == "patch-file.yaml" {
			hasPatchFile = true
		}
	}
	if !hasPatchFile {
		t.Errorf("expected patch-file.yaml in deps: %v", deps)
	}
}

func TestFilterTargets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	root := t.TempDir()

	// Create a mini repo structure.
	// Target A depends on resources/a.yaml
	targetA := filepath.Join(root, "kustomize", "a")
	os.MkdirAll(targetA, 0o755)
	os.WriteFile(filepath.Join(targetA, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
`), 0o644)
	os.WriteFile(filepath.Join(targetA, "deployment.yaml"), []byte(""), 0o644)

	// Target B depends on resources/b.yaml
	targetB := filepath.Join(root, "kustomize", "b")
	os.MkdirAll(targetB, 0o755)
	os.WriteFile(filepath.Join(targetB, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
`), 0o644)
	os.WriteFile(filepath.Join(targetB, "deployment.yaml"), []byte(""), 0o644)

	targets := []string{targetA, targetB}
	// Only A's deployment changed.
	changed := []string{"kustomize/a/deployment.yaml"}

	result := filterTargets(targets, root, "kustomize", changed)

	if len(result) != 1 {
		t.Fatalf("expected 1 filtered target, got %d: %v", len(result), result)
	}
	if result[0] != targetA {
		t.Errorf("expected target A, got %s", result[0])
	}
}

func TestFilterTargets_FailOpen(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	root := t.TempDir()
	targetBad := filepath.Join(root, "kustomize", "bad")
	os.MkdirAll(targetBad, 0o755)
	// Write an invalid kustomization file.
	os.WriteFile(filepath.Join(targetBad, "kustomization.yaml"), []byte("this is not valid yaml: ["), 0o644)

	targets := []string{targetBad}
	changed := []string{"some/changed/file.yaml"}

	result := filterTargets(targets, root, "kustomize", changed)

	// Fail-open: unparseable target should be included.
	if len(result) != 1 {
		t.Fatalf("expected 1 target (fail-open), got %d: %v", len(result), result)
	}
}

func TestOrphanTargets(t *testing.T) {
	t.Parallel()

	root1 := t.TempDir()
	root2 := t.TempDir()

	absRoot1 := filepath.Join(root1, "kustomize")
	absRoot2 := filepath.Join(root2, "kustomize")
	os.MkdirAll(filepath.Join(absRoot1, "a"), 0o755)
	os.MkdirAll(filepath.Join(absRoot1, "b"), 0o755)
	os.MkdirAll(filepath.Join(absRoot2, "a"), 0o755)
	// b only exists in root1

	mine := []string{
		filepath.Join(absRoot1, "a"),
		filepath.Join(absRoot1, "b"),
	}
	theirs := []string{
		filepath.Join(absRoot2, "a"),
	}

	orphans := orphanTargets(mine, absRoot1, theirs, absRoot2)
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d: %v", len(orphans), orphans)
	}
	if !strings.HasSuffix(orphans[0], "b") {
		t.Errorf("expected orphan to be 'b', got %s", orphans[0])
	}
}

func TestChangedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	r := newTestRepo(t)
	r.writeFile("file1.txt", "initial")
	r.commit("initial")
	r.branch("feature")
	r.writeFile("file2.txt", "new file")
	r.writeFile("file1.txt", "modified")
	r.commit("changes")
	r.checkout("main")
	cdRepo(t, r.dir)

	files, err := changedFiles("main", "feature")
	if err != nil {
		t.Fatalf("changedFiles: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 changed files, got %d: %v", len(files), files)
	}

	has := make(map[string]bool)
	for _, f := range files {
		has[f] = true
	}
	if !has["file1.txt"] {
		t.Error("expected file1.txt in changed files")
	}
	if !has["file2.txt"] {
		t.Error("expected file2.txt in changed files")
	}
}

func TestStripFileSourceKey(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"key=path/to/file", "path/to/file"},
		{"path/to/file", "path/to/file"},
		{"k=v=w", "v=w"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripFileSourceKey(c.in); got != c.want {
			t.Errorf("stripFileSourceKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"https://example.com/repo", true},
		{"http://example.com", true},
		{"ssh://git@github.com/repo", true},
		{"../relative/path", false},
		{"deployment.yaml", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isURL(c.in); got != c.want {
			t.Errorf("isURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
