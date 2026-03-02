package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kustomize/api/types"
)

// loadKustomization reads and parses a kustomization file, calling
// FixKustomization to merge deprecated fields.
func loadKustomization(path string) (*types.Kustomization, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var k types.Kustomization
	if err := k.Unmarshal(data); err != nil {
		return nil, err
	}
	k.FixKustomization()
	return &k, nil
}

// isURL returns true if s looks like a URL (contains "://").
func isURL(s string) bool {
	return strings.Contains(s, "://")
}

// isKustomizationDir returns true if dir contains a kustomization file.
func isKustomizationDir(dir string) bool {
	return findKustomizationFile(dir) != ""
}

// stripFileSourceKey strips the optional "key=" prefix from a file source
// entry (e.g. "mykey=path/to/file" → "path/to/file").
func stripFileSourceKey(s string) string {
	if i := strings.Index(s, "="); i >= 0 {
		return s[i+1:]
	}
	return s
}

// collectDeps recursively collects all file and directory paths that a
// kustomize target depends on. The visited map provides cycle detection.
func collectDeps(targetDir string, visited map[string]bool) ([]string, error) {
	abs, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, err
	}
	if visited[abs] {
		return nil, nil
	}
	visited[abs] = true

	kf := findKustomizationFile(abs)
	if kf == "" {
		return nil, nil
	}

	k, err := loadKustomization(kf)
	if err != nil {
		return nil, err
	}

	var deps []string
	// Always include the kustomization file itself.
	deps = append(deps, kf)

	// addFile adds a resolved file path to deps.
	addFile := func(base, rel string) {
		if rel == "" || isURL(rel) {
			return
		}
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		deps = append(deps, p)
	}

	// addDirOrFile resolves a resource reference — if it's a directory
	// with a kustomization file, recurse into it; otherwise treat as file.
	addDirOrFile := func(base, ref string) error {
		if ref == "" || isURL(ref) {
			return nil
		}
		p := ref
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		if isKustomizationDir(p) {
			sub, err := collectDeps(p, visited)
			if err != nil {
				return err
			}
			deps = append(deps, sub...)
		} else {
			deps = append(deps, p)
		}
		return nil
	}

	// Resources (includes merged Bases after FixKustomization).
	for _, r := range k.Resources {
		if err := addDirOrFile(abs, r); err != nil {
			return nil, err
		}
	}

	// Components — always directories.
	for _, c := range k.Components {
		if isURL(c) {
			continue
		}
		p := c
		if !filepath.IsAbs(p) {
			p = filepath.Join(abs, p)
		}
		sub, err := collectDeps(p, visited)
		if err != nil {
			return nil, err
		}
		deps = append(deps, sub...)
	}

	// Patches.
	for _, p := range k.Patches {
		addFile(abs, p.Path)
	}

	// PatchesStrategicMerge (deprecated but still used).
	for _, p := range k.PatchesStrategicMerge {
		s := string(p)
		path := filepath.Join(abs, s)
		if _, err := os.Stat(path); err == nil {
			deps = append(deps, path)
		}
		// Otherwise it's inline YAML — skip.
	}

	// PatchesJson6902 (deprecated but still used).
	for _, p := range k.PatchesJson6902 {
		addFile(abs, p.Path)
	}

	// ConfigMapGenerator.
	for _, g := range k.ConfigMapGenerator {
		for _, f := range g.FileSources {
			addFile(abs, stripFileSourceKey(f))
		}
		for _, e := range g.EnvSources {
			addFile(abs, e)
		}
	}

	// SecretGenerator.
	for _, g := range k.SecretGenerator {
		for _, f := range g.FileSources {
			addFile(abs, stripFileSourceKey(f))
		}
		for _, e := range g.EnvSources {
			addFile(abs, e)
		}
	}

	// HelmCharts.
	chartHome := "charts"
	if k.HelmGlobals != nil && k.HelmGlobals.ChartHome != "" {
		chartHome = k.HelmGlobals.ChartHome
	}
	for _, h := range k.HelmCharts {
		// Add the chart directory.
		chartDir := filepath.Join(abs, chartHome, h.Name)
		deps = append(deps, chartDir)
		if h.ValuesFile != "" {
			addFile(abs, h.ValuesFile)
		}
		for _, vf := range h.AdditionalValuesFiles {
			addFile(abs, vf)
		}
	}

	// Replacements.
	for _, r := range k.Replacements {
		addFile(abs, r.Path)
	}

	// Simple file-path lists.
	for _, f := range k.Crds {
		addFile(abs, f)
	}
	for _, f := range k.Configurations {
		addFile(abs, f)
	}
	for _, f := range k.Generators {
		addFile(abs, f)
	}
	for _, f := range k.Transformers {
		addFile(abs, f)
	}
	for _, f := range k.Validators {
		addFile(abs, f)
	}

	return deps, nil
}

// changedFiles returns repo-relative paths of files changed between two refs.
// Uses three-dot diff (merge-base semantics) and falls back to two-dot.
func changedFiles(baseRef, headRef string) ([]string, error) {
	// Try three-dot first (merge-base semantics, matching PR diff behavior).
	out, err := exec.Command("git", "diff", "--name-only", baseRef+"..."+headRef).Output()
	if err != nil {
		// Fall back to two-dot.
		out, err = exec.Command("git", "diff", "--name-only", baseRef+".."+headRef).Output()
		if err != nil {
			return nil, err
		}
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	return strings.Split(s, "\n"), nil
}

// filterTargets returns the subset of targets whose dependencies overlap with
// the changed file set. Targets whose dependencies cannot be parsed are
// included (fail-open).
func filterTargets(targets []string, worktreeRoot, rootDir string, changed []string) []string {
	if len(changed) == 0 {
		return nil
	}

	changedSet := make(map[string]bool, len(changed))
	for _, f := range changed {
		changedSet[f] = true
	}

	// Also add all parent directories of changed files, so that a change to
	// "charts/foo/templates/deploy.yaml" is detected when a dep is "charts/foo".
	for _, f := range changed {
		for d := filepath.Dir(f); d != "." && d != "/"; d = filepath.Dir(d) {
			changedSet[d] = true
		}
	}

	repoRoot, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		// Can't determine repo root — fail-open, return all targets.
		return targets
	}
	repoRootStr := strings.TrimSpace(string(repoRoot))

	var result []string
	for _, target := range targets {
		deps, err := collectDeps(target, make(map[string]bool))
		if err != nil {
			// Fail-open: include target if we can't parse deps.
			result = append(result, target)
			continue
		}

		affected := false
		for _, dep := range deps {
			// Convert absolute dep path to repo-relative.
			// The dep is in the worktree, so strip worktreeRoot to get worktree-relative,
			// then that's equivalent to repo-relative since the worktree is a checkout.
			rel, err := filepath.Rel(worktreeRoot, dep)
			if err != nil {
				affected = true
				break
			}
			if changedSet[rel] {
				affected = true
				break
			}
		}
		if affected {
			result = append(result, target)
		}
	}
	_ = repoRootStr // suppress unused warning; kept for potential future use
	return result
}

// orphanTargets returns targets that exist in "mine" but not in "theirs"
// (i.e., added or removed targets that must always be built).
func orphanTargets(mine []string, myRoot string, theirs []string, theirRoot string) []string {
	// Build a set of repo-relative target paths from "theirs".
	theirSet := make(map[string]bool, len(theirs))
	for _, t := range theirs {
		rel, err := filepath.Rel(theirRoot, t)
		if err != nil {
			continue
		}
		theirSet[rel] = true
	}

	var orphans []string
	for _, t := range mine {
		rel, err := filepath.Rel(myRoot, t)
		if err != nil {
			orphans = append(orphans, t)
			continue
		}
		if !theirSet[rel] {
			orphans = append(orphans, t)
		}
	}
	return orphans
}
