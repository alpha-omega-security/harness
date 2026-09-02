package skills

import (
	"path"
	"slices"
	"strings"
)

const (
	gitMetadataDir    = ".git"
	gitMetadataPrefix = gitMetadataDir + "/"
)

// DirAllExcluded reports whether an ignore pattern excludes every file below
// rel. A pattern ending in /** is the only form that can blanket a directory.
func DirAllExcluded(rel string, _ []string, ignore []string) bool {
	if rel == gitMetadataDir || strings.HasPrefix(rel, gitMetadataPrefix) {
		return false
	}
	return dirBlanketed(rel, ignore)
}

func dirBlanketed(rel string, patterns []string) bool {
	for _, pattern := range patterns {
		prefix, ok := strings.CutSuffix(pattern, "/**")
		if ok && Match(prefix, rel) {
			return true
		}
	}
	return false
}

// PathIncluded applies an optional include list and an ignore list to rel.
// Paths are slash-separated and relative to the caller's root. The .git
// directory is always retained for git-aware skills.
func PathIncluded(rel string, paths, ignore []string) bool {
	if rel == gitMetadataDir || strings.HasPrefix(rel, gitMetadataPrefix) {
		return true
	}
	if len(paths) > 0 && !matchAny(paths, rel) {
		return false
	}
	return !matchAny(ignore, rel)
}

func matchAny(patterns []string, name string) bool {
	return slices.ContainsFunc(patterns, func(pattern string) bool {
		return Match(pattern, name)
	})
}

// ValidateGlob returns path.ErrBadPattern for a malformed segment.
func ValidateGlob(pattern string) error {
	for segment := range strings.SplitSeq(pattern, "/") {
		if _, err := path.Match(segment, "x"); err != nil {
			return err
		}
	}
	return nil
}

// Match reports whether name matches a shell glob with doublestar support.
func Match(pattern, name string) bool {
	if pattern == "" || name == "" {
		return pattern == name
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			if len(rest) == 0 {
				return true
			}
			for index := 0; index <= len(name); index++ {
				if matchSegments(rest, name[index:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, _ := path.Match(pattern[0], name[0])
		if !ok {
			return false
		}
		pattern = pattern[1:]
		name = name[1:]
	}
	return len(name) == 0
}

// SplitPatterns parses the newline-separated storage form into a clean slice,
// trimming whitespace and dropping empty lines.
func SplitPatterns(value string) []string {
	if value == "" {
		return nil
	}
	var patterns []string
	for line := range strings.SplitSeq(value, "\n") {
		if pattern := strings.TrimSpace(line); pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

// JoinPatterns serialises patterns into the newline-separated storage form.
func JoinPatterns(patterns []string) string {
	return strings.Join(patterns, "\n")
}
