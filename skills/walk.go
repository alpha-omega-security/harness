package skills

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

const maxWalkDepth = 6

// Walk visits SKILL.md files under root.
func Walk(root string, fn func(*Skill) error) error {
	if fn == nil {
		return fmt.Errorf("skills: walk callback is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	err = filepath.WalkDir(abs, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if walkDepth(abs, path) > maxWalkDepth {
				return fs.SkipDir
			}
			if shouldSkipDir(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != skillFilename {
			return nil
		}
		skill, err := Parse(path)
		if err != nil {
			return err
		}
		return fn(skill)
	})
	if err != nil {
		return fmt.Errorf("skills: walk %s: %w", abs, err)
	}
	return nil
}

func walkDepth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(filepath.Clean(rel), string(filepath.Separator)) + 1
}

func shouldSkipDir(name string) bool {
	switch name {
	case gitMetadataDir, "node_modules", ".venv", "__pycache__", "vendor":
		return true
	default:
		return false
	}
}
