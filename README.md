# kustdiff

kustdiff compares the rendered output of [Kustomize](https://kustomize.io/) targets between two git refs. It checks out both refs into temporary worktrees, discovers and builds all kustomization targets, and produces a unified diff of the rendered YAML.

This is useful for reviewing the actual effect of changes in pull requests or across branches — seeing what will change in the final manifests rather than just the source files.

## Features

- Discovers kustomization targets automatically by walking a configurable root directory
- Recognizes all standard kustomization filenames: `kustomization.yaml`, `kustomization.yml`, `Kustomization`
- Supports Helm chart inflation for targets that use `helmCharts`
- **Git diff narrowing** — uses `git diff` to identify changed files, parses kustomization dependency graphs, and builds only affected targets (skipping the rest entirely)
- **Content-addressed caching** — hashes target inputs via git tree objects and caches rendered output between runs
- **Bounded concurrency** — configurable worker limit to prevent resource exhaustion
- All optimizations are fail-open: if dependency parsing or caching fails for a target, it falls back to a full build

## Requirements

- Go 1.22+
- Git
- Helm (only if your kustomizations use `helmCharts`)

## Installation

```sh
go install kustdiff@latest
```

Or build from source:

```sh
git clone <repo-url>
cd kustdiff
go build -o kustdiff .
```

## Usage

```sh
kustdiff -base main -head feature-branch
```

The diff is written to stdout in unified diff format. If there are no differences, it prints "No differences found."

### Examples

Compare a feature branch against main:

```sh
kustdiff -base main -head feature-branch
```

Compare two specific commits:

```sh
kustdiff -base abc1234 -head def5678
```

Use a different root directory and search deeper:

```sh
kustdiff -base main -head HEAD -root deploy/overlays -depth 4
```

Force a full build of all targets (skip narrowing):

```sh
kustdiff -base main -head HEAD -full-build
```

Run with a single worker and no cache:

```sh
kustdiff -base main -head HEAD -workers 1 -no-cache
```

Enable debug logging to see what's happening:

```sh
kustdiff -base main -head HEAD -debug
```

## Options

| Flag | Environment Variable | Default | Description |
|------|---------------------|---------|-------------|
| `-base` | `INPUT_BASE_REF` | *(required)* | Base git ref to compare from (branch, tag, or SHA) |
| `-head` | `INPUT_HEAD_REF` | *(required)* | Head git ref to compare to (branch, tag, or SHA) |
| `-root` | `INPUT_ROOT_DIR` | `kustomize` | Root directory containing kustomization targets |
| `-depth` | `INPUT_MAX_DEPTH` | `2` | Max directory depth to search for kustomization files |
| `-workers` | `INPUT_WORKERS` | Number of CPUs | Max concurrent kustomize builds |
| `-full-build` | `INPUT_FULL_BUILD` | `false` | Skip dependency-based narrowing and build all targets |
| `-no-cache` | `INPUT_NO_CACHE` | `false` | Disable the build cache |
| `-cache-dir` | `INPUT_CACHE_DIR` | `~/.cache/kustdiff` | Directory for the build cache |
| `-debug` | `DEBUG=true` | `false` | Print debug information to stderr |

Environment variables are checked first, then overridden by flags if provided.

## How It Works

### Target Discovery

kustdiff walks the root directory (default `kustomize/`) up to the configured depth, looking for directories that contain a `kustomization.yaml`, `kustomization.yml`, or `Kustomization` file. Each such directory is a build target.

### Dependency-Based Narrowing

By default, kustdiff runs `git diff --name-only` between the two refs to determine which files changed. It then parses each target's kustomization file to build a dependency graph — following `resources`, `components`, `patches`, `configMapGenerator` file sources, `secretGenerator` file sources, `helmCharts`, `replacements`, and other references recursively.

A target is only built if at least one of its dependencies overlaps with the changed file set. Targets that exist in only one ref (added or removed) are always built.

If dependency parsing fails for any target, that target is included in the build (fail-open). Use `--full-build` to disable narrowing entirely.

### Caching

Rendered output is cached based on a content-addressed key derived from git tree and blob hashes of all dependency paths. If the inputs to a target haven't changed between runs, the cached output is reused without invoking kustomize.

The cache is stored at `~/.cache/kustdiff/v1/` (or `$XDG_CACHE_HOME/kustdiff/v1/`) using a two-character fan-out directory structure. Use `--no-cache` to disable caching, or `--cache-dir` to change the location.

Cache failures are silently ignored — a failed read triggers a fresh build, and a failed write is skipped.

### Build

Each target is built using the kustomize library directly (no subprocess). If a target's kustomization file references `helmCharts`, Helm chart inflation is enabled automatically. Builds run concurrently up to the configured worker limit.

## Running Tests

```sh
go test ./... -count=1
```

Tests include unit tests for dependency parsing, caching, and target filtering, as well as end-to-end tests that create temporary git repositories and run the full pipeline.
