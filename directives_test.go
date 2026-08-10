package harness

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDirectivePaths(t *testing.T) {
	t.Parallel()

	dirs, files := DirectivePaths()
	for _, want := range []string{".claude", ".opencode", "skills", ".cursor", ".aider.*"} {
		if !slices.Contains(dirs, want) {
			t.Errorf("DirectivePaths dirs missing %q", want)
		}
	}
	for _, want := range []string{"claude.md", "agents.md", "copilot-instructions.md", "gemini.md", "*.instructions.md"} {
		if !slices.Contains(files, want) {
			t.Errorf("DirectivePaths files missing %q", want)
		}
	}

	for _, patterns := range [][]string{dirs, files} {
		for _, pattern := range patterns {
			if pattern != strings.ToLower(pattern) {
				t.Errorf("pattern %q is not lowercase", pattern)
			}
			if strings.ContainsAny(pattern, `/\\`) {
				t.Errorf("pattern %q is not a basename", pattern)
			}
			if _, err := path.Match(pattern, "x"); err != nil {
				t.Errorf("pattern %q: %v", pattern, err)
			}
		}
	}

	dirs[0] = "changed"
	files[0] = "changed"
	freshDirs, freshFiles := DirectivePaths()
	if slices.Contains(freshDirs, "changed") || slices.Contains(freshFiles, "changed") {
		t.Fatal("DirectivePaths returned mutable package state")
	}
}

func TestStripDirectives(t *testing.T) {
	root := t.TempDir()
	writeDirectiveTestFiles(t, root, map[string]string{
		"README.md":                                  "kept",
		"src/main.go":                                "package main",
		"CLAUDE.md":                                  "remove",
		"docs/claude.local.md":                       "remove",
		"nested/AGENTS.md":                           "remove",
		"pkg/Agent.md":                               "remove",
		"deploy.prompt.md":                           "remove",
		".claude/settings.json":                      "remove",
		".opencode/skill/evil/SKILL.md":              "remove",
		"skills/evil/SKILL.md":                       "remove",
		".github/skills/evil/SKILL.md":               "remove",
		".github/copilot-instructions.md":            "remove",
		".github/instructions/build.instructions.md": "remove",
		"vendor/.cursor/rules/evil.mdc":              "remove",
		"cache/.Aider.tags/index":                    "remove",
	})

	removed, err := StripDirectives(root)
	if err != nil {
		t.Fatalf("StripDirectives: %v", err)
	}
	if removed != 13 {
		t.Errorf("StripDirectives removed %d items, want 13", removed)
	}

	assertDirectiveTestExists(t, root, "README.md", "src/main.go", ".github")
	assertDirectiveTestGone(t, root,
		"CLAUDE.md",
		"docs/claude.local.md",
		"nested/AGENTS.md",
		"pkg/Agent.md",
		"deploy.prompt.md",
		".claude",
		".opencode",
		"skills",
		".github/skills",
		".github/copilot-instructions.md",
		".github/instructions/build.instructions.md",
		"vendor/.cursor",
		"cache/.Aider.tags",
	)

	removed, err = StripDirectives(root)
	if err != nil {
		t.Fatalf("second StripDirectives: %v", err)
	}
	if removed != 0 {
		t.Errorf("second StripDirectives removed %d items, want 0", removed)
	}
}

func TestStripDirectivesPreservesGitAndBenignPaths(t *testing.T) {
	root := t.TempDir()
	writeDirectiveTestFiles(t, root, map[string]string{
		".git/HEAD":                "ref: refs/heads/main",
		".git/refs/heads/claude":   "abc",
		".github/workflows/ci.yml": "name: ci",
		".cursor":                  "regular file",
		"docs/AGENTS_GUIDE.md":     "kept",
		"claude.go":                "package claude",
		"skills":                   "regular file",
		"src/ai/model.go":          "kept",
		".ai/config.yml":           "kept",
		".llm/prompts/x.txt":       "kept",
		"rules.txt":                "kept",
	})

	removed, err := StripDirectives(root)
	if err != nil {
		t.Fatalf("StripDirectives: %v", err)
	}
	if removed != 0 {
		t.Errorf("StripDirectives removed %d items, want 0", removed)
	}
	assertDirectiveTestExists(t, root,
		".git/HEAD",
		".git/refs/heads/claude",
		".github/workflows/ci.yml",
		".cursor",
		"docs/AGENTS_GUIDE.md",
		"claude.go",
		"skills",
		"src/ai/model.go",
		".ai/config.yml",
		".llm/prompts/x.txt",
		"rules.txt",
	)
}

func TestStripDirectivesRemovesSymlinksWithoutFollowingThem(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target", filepath.Join(root, "nested", ".cursor")); err != nil {
		t.Fatal(err)
	}

	removed, err := StripDirectives(root)
	if err != nil {
		t.Fatalf("StripDirectives: %v", err)
	}
	if removed != 2 {
		t.Errorf("StripDirectives removed %d items, want 2", removed)
	}
	assertDirectiveTestGone(t, root, ".claude", "nested/.cursor")
	assertDirectiveTestExists(t, root, "target")
}

func TestStripDirectivesMissingRootIsNoop(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	removed, err := StripDirectives(root)
	if err != nil {
		t.Fatalf("StripDirectives: %v", err)
	}
	if removed != 0 {
		t.Errorf("StripDirectives removed %d items, want 0", removed)
	}
}

func writeDirectiveTestFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertDirectiveTestExists(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
}

func assertDirectiveTestGone(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, got %v", name, err)
		}
	}
}
