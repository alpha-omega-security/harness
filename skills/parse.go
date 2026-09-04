// Package skills parses, filters, and stages agent skill directories. The core
// SKILL.md format is described at [SpecURL]. [Skill.SchemaJSON] exposes a
// conventional sibling file permitted by the specification's open directory
// layout.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SpecURL is the Agent Skills format specification.
const SpecURL = "https://agentskills.io/specification"

const (
	skillFilename  = "SKILL.md"
	schemaFilename = "schema.json"
	maxNameLen     = 64
	maxDescLen     = 1024
	maxCompatLen   = 500
)

var (
	frontmatterPattern = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n?(.*)\z`)
	namePattern        = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// Skill is one parsed markdown instruction file and its source directory.
type Skill struct {
	Name          string
	Description   string
	License       string
	Compatibility string
	AllowedTools  string
	Metadata      map[string]any
	Body          string
	SourcePath    string
	// SchemaJSON is the unmodified content of a sibling schema.json. The file
	// is a common extension rather than a named field in the Agent Skills
	// specification.
	SchemaJSON string
	// SourceHash is the SHA-256 of the instruction file followed by its
	// sibling schema.json, when present. Including the schema makes a
	// schema-only edit visible to callers that use this value for change
	// detection.
	SourceHash string
	Warnings   []string
}

type frontmatter struct {
	Name          string         `yaml:"name,omitempty"`
	Description   string         `yaml:"description,omitempty"`
	License       string         `yaml:"license,omitempty"`
	Compatibility string         `yaml:"compatibility,omitempty"`
	AllowedTools  string         `yaml:"allowed-tools,omitempty"`
	Metadata      map[string]any `yaml:"metadata,omitempty"`
}

// Parse reads a markdown instruction file. YAML frontmatter is optional.
func Parse(path string) (*Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("skills: read %s: %w", path, err)
	}
	abs, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("skills: resolve %s: %w", path, err)
	}
	schema, err := os.ReadFile(filepath.Join(abs, schemaFilename))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("skills: read %s: %w", schemaFilename, err)
	}
	fm, body, hasFrontmatter := splitFrontmatter(raw)
	parsed := frontmatter{Metadata: make(map[string]any)}
	if hasFrontmatter {
		if err := yaml.Unmarshal(fm, &parsed); err != nil {
			return nil, fmt.Errorf("skills: yaml %s: %w", path, err)
		}
		if parsed.Metadata == nil {
			parsed.Metadata = make(map[string]any)
		}
	}
	skill := &Skill{
		Name:          strings.TrimSpace(parsed.Name),
		Description:   strings.TrimSpace(parsed.Description),
		License:       strings.TrimSpace(parsed.License),
		Compatibility: strings.TrimSpace(parsed.Compatibility),
		AllowedTools:  strings.TrimSpace(parsed.AllowedTools),
		Metadata:      parsed.Metadata,
		Body:          strings.TrimSpace(body),
		SourcePath:    abs,
		SchemaJSON:    string(schema),
		SourceHash:    hash(raw, schema),
	}
	if !hasFrontmatter {
		skill.Body = strings.TrimSpace(string(raw))
	}
	skill.validate(path, hasFrontmatter)
	return skill, nil
}

func splitFrontmatter(raw []byte) ([]byte, string, bool) {
	match := frontmatterPattern.FindSubmatch(raw)
	if match == nil {
		return nil, string(raw), false
	}
	return match[1], string(match[2]), true
}

func hash(parts ...[]byte) string {
	hasher := sha256.New()
	for _, part := range parts {
		_, _ = hasher.Write(part)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func (s *Skill) validate(path string, hadFrontmatter bool) {
	if s.Name == "" {
		s.Name = inferredName(path)
		if hadFrontmatter {
			s.Warnings = append(s.Warnings, "name missing, using path name")
		}
	}
	if hadFrontmatter && s.Description == "" {
		s.Warnings = append(s.Warnings, "description is missing")
	}
	if len(s.Name) > maxNameLen {
		s.Warnings = append(s.Warnings, fmt.Sprintf("name %q exceeds %d characters", s.Name, maxNameLen))
	}
	if !namePattern.MatchString(s.Name) {
		s.Warnings = append(s.Warnings,
			fmt.Sprintf("name %q is not spec-conformant (lowercase, digits, hyphens only)", s.Name))
	}
	if len(s.Description) > maxDescLen {
		s.Warnings = append(s.Warnings, fmt.Sprintf("description exceeds %d characters", maxDescLen))
	}
	if len(s.Compatibility) > maxCompatLen {
		s.Warnings = append(s.Warnings, fmt.Sprintf("compatibility exceeds %d characters", maxCompatLen))
	}
}

func inferredName(path string) string {
	base := filepath.Base(path)
	if strings.EqualFold(base, skillFilename) {
		return filepath.Base(filepath.Dir(path))
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// ValidateNamespace rejects metadata keys in prefix that are not listed in
// allowed. Keys outside the namespace are ignored.
func ValidateNamespace(meta map[string]any, prefix string, allowed map[string]bool) error {
	for key := range meta {
		if strings.HasPrefix(key, prefix) && !allowed[key] {
			return fmt.Errorf("skills: unknown metadata key %q", key)
		}
	}
	return nil
}

// Render returns the SKILL.md bytes for skill with one trailing newline.
func Render(skill *Skill) ([]byte, error) {
	if skill == nil {
		return nil, fmt.Errorf("skills: skill is required")
	}
	if skill.Description == "" && skill.License == "" && skill.Compatibility == "" &&
		skill.AllowedTools == "" && len(skill.Metadata) == 0 {
		return withTrailingNewline(skill.Body), nil
	}
	fm, err := yaml.Marshal(frontmatter{
		Name:          skill.Name,
		Description:   skill.Description,
		License:       skill.License,
		Compatibility: skill.Compatibility,
		AllowedTools:  skill.AllowedTools,
		Metadata:      skill.Metadata,
	})
	if err != nil {
		return nil, fmt.Errorf("skills: marshal frontmatter: %w", err)
	}
	var output strings.Builder
	output.WriteString("---\n")
	output.Write(fm)
	output.WriteString("---\n\n")
	output.WriteString(skill.Body)
	return withTrailingNewline(output.String()), nil
}

func withTrailingNewline(s string) []byte {
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s)
}
