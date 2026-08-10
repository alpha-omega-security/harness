package harness

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// directiveDirs are directory basename patterns that agent CLIs load as
// configuration, hooks, or skills. The patterns are lowercase because
// StripDirectives matches them case-insensitively.
var directiveDirs = []string{
	// Harness backends.
	".claude",
	".opencode",
	"skills",

	// Other agent CLIs.
	".anthropic",
	".cursor",
	".windsurf",
	".continue",
	".cline",
	".roo",
	".goose",
	".aider",
	".aider.*",
	".gemini",
	".codex",
	".copilot",
	".devin",
}

// directiveFiles are file basename patterns that agent CLIs load as project
// instructions. See directiveDirs.
var directiveFiles = []string{
	// Harness backends.
	"claude.md",
	"claude.*.md",
	"agents.md",
	"copilot-instructions.md",

	// Other agent CLIs.
	"agent.md",
	"gemini.md",
	"codex.md",
	".cursorrules",
	".cursorignore",
	".windsurfrules",
	".clinerules",
	".roorules",
	".rooignore",
	".aider.conf.yml",
	".aider.conf.yaml",
	".aiderrules",
	"*.instructions.md",
	"*.prompt.md",
	".rules",
	"llms.txt",
	"llms-full.txt",
}

// DirectivePaths returns directory and file basename patterns that agent CLIs
// automatically load as project instructions or configuration. Matching is
// case-insensitive and follows path.Match semantics. The returned slices are
// copies and may be modified by the caller.
func DirectivePaths() (dirs, files []string) {
	return append([]string(nil), directiveDirs...), append([]string(nil), directiveFiles...)
}

// StripDirectives removes files and directories below root whose basenames
// match DirectivePaths. A removed directory counts as one item regardless of
// its contents. The .git subtree is skipped. A missing root is a no-op.
func StripDirectives(root string) (int, error) {
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	removed := 0
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}

		base := strings.ToLower(entry.Name())
		if entry.IsDir() && base == ".git" {
			return filepath.SkipDir
		}
		directoryLike := entry.IsDir() || entry.Type()&fs.ModeSymlink != 0
		if directoryLike && matchesDirective(directiveDirs, base) {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			removed++
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if matchesDirective(directiveFiles, base) {
			if err := os.Remove(p); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}

func matchesDirective(patterns []string, base string) bool {
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, base); matched {
			return true
		}
	}
	return false
}
