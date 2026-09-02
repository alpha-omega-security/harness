package harness

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	guideDirMode  = 0o755
	guideFileMode = 0o644
	defaultSrcDir = "src"
)

// explicitSkillPrompt is the activation prompt for backends that discover a
// SKILL.md but do not have a native skill invocation. An explicit resume
// prompt is returned unchanged so callers can send a targeted repair nudge.
func explicitSkillPrompt(j Job, skillPath string) string {
	resume := j.ResumeSessionID != ""
	if resume && j.ResumePrompt != "" {
		return j.ResumePrompt
	}
	if !resume && j.Prompt != "" {
		return j.Prompt
	}
	if j.SkillName == "" {
		if resume {
			return "Continue from where you left off."
		}
		return ""
	}
	verb := "Follow"
	if resume {
		verb = "Continue following"
	}
	prompt := verb + " the instructions in " + skillPath + "/SKILL.md against the repository cloned at " + sourcePromptPath(j) + "."
	if j.OutputFile != "" {
		prompt += " Write your structured output to ./" + j.OutputFile + " as the skill specifies."
		prompt += schemaValidationHint(j)
	}
	return prompt
}

// buildSkillPrompt is Claude's activation prompt. SKILL.md holds the actual
// instructions, so the prompt only selects it and locates the repository.
func buildSkillPrompt(j Job) string {
	prompt := fmt.Sprintf("Use the %q skill on the repository cloned at %s.", j.SkillName, sourcePromptPath(j))
	if j.OutputFile != "" {
		prompt += fmt.Sprintf(" Write your structured output to ./%s as the skill specifies.", j.OutputFile)
		prompt += schemaValidationHint(j)
	}
	return prompt
}

// buildResumePrompt tells a resumed agent to continue and restates the output
// file, which is easy to lose among the earlier conversation turns.
func buildResumePrompt(j Job) string {
	if j.SkillName == "" {
		return "Continue from where you left off."
	}
	prompt := fmt.Sprintf(
		"Continue the %q skill on the repository at %s from where you left off.",
		j.SkillName,
		sourcePromptPath(j),
	)
	if j.OutputFile != "" {
		prompt += fmt.Sprintf(" Write your structured output to ./%s as the skill specifies.", j.OutputFile)
		prompt += schemaValidationHint(j)
	}
	return prompt
}

// sourcePromptPath keeps the historical ./src layout when SrcDir is empty.
// Callers whose workspace is already the repository root can set SrcDir to ".".
func sourcePromptPath(j Job) string {
	dir := strings.TrimSpace(j.SrcDir)
	if dir == "" {
		dir = defaultSrcDir
	}
	dir = filepath.ToSlash(filepath.Clean(dir))
	if dir == "." {
		return "the workspace root"
	}
	return "./" + strings.TrimPrefix(dir, "./")
}

// schemaValidationHint tells the agent to check its JSON output against the
// staged schema.json. A caller with its own validation endpoint or tooling
// sets Job.ValidationHint to a specific instruction; the generic default
// covers callers with no such endpoint. Non-JSON outputs get no hint.
func schemaValidationHint(j Job) string {
	if !strings.HasSuffix(j.OutputFile, ".json") {
		return ""
	}
	if j.ValidationHint != "" {
		return " " + j.ValidationHint
	}
	return fmt.Sprintf(" Validate ./%s against ./schema.json before finishing.", j.OutputFile)
}

// WriteSystemPrompt writes j.SystemPrompt to the guide file used by h. A
// backend that passes the system prompt in Args has no file to write.
func WriteSystemPrompt(h Harness, j Job) error {
	if strings.TrimSpace(j.SystemPrompt) == "" {
		return nil
	}
	if h == nil {
		return fmt.Errorf("harness: backend is required for a system prompt")
	}
	if h.SystemPromptViaArgs() {
		return nil
	}
	if j.Workspace == "" {
		return fmt.Errorf("harness: workspace is required for a system prompt")
	}
	guide := h.GuideFilename()
	if guide == "" || !filepath.IsLocal(guide) {
		return fmt.Errorf("harness: invalid guide filename %q", guide)
	}
	if err := os.MkdirAll(j.Workspace, guideDirMode); err != nil {
		return fmt.Errorf("harness: create workspace: %w", err)
	}
	root, err := os.OpenRoot(j.Workspace)
	if err != nil {
		return fmt.Errorf("harness: open workspace: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.Dir(guide), guideDirMode); err != nil {
		return fmt.Errorf("harness: create guide directory: %w", err)
	}
	content := j.SystemPrompt
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := writeGuideFile(root, guide, []byte(content)); err != nil {
		return fmt.Errorf("harness: write %s: %w", guide, err)
	}
	return nil
}

// writeGuideFile replaces guide through a fresh directory entry so an
// existing symlink, hard link, or special file is never opened for writing.
func writeGuideFile(root *os.Root, guide string, content []byte) error {
	mode := os.FileMode(guideFileMode)
	preserveMode := false
	if info, err := root.Lstat(guide); err == nil {
		if info.Mode().IsRegular() {
			mode = info.Mode().Perm()
			preserveMode = true
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	parent := filepath.Dir(guide)
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	tmp := filepath.Join(parent, ".harness-"+hex.EncodeToString(id[:])+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		_ = f.Close()
		if removeTemp {
			_ = root.Remove(tmp)
		}
	}()
	if _, err := f.Write(content); err != nil {
		return err
	}
	if preserveMode {
		if err := f.Chmod(mode); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmp, guide); err != nil {
		return err
	}
	removeTemp = false
	return nil
}
