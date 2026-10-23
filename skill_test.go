package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/git-pkgs/skills"
)

func writeSkillFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStageSkillFromFS(t *testing.T) {
	fsys := fstest.MapFS{
		"generate/SKILL.md":    {Data: []byte("---\nname: generate\ndescription: Generate tests\nmodel: opus\n---\nWrite tests.\n")},
		"generate/schema.json": {Data: []byte(`{"type":"object"}`)},
	}
	skill, err := skills.ParseFS(fsys, "generate")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	dir, err := StageSkill(ClaudeHarness{}, Job{Workspace: workspace, OutputFile: "report.json"}, fsys, skill)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(workspace, ".claude", "skills", "generate") {
		t.Errorf("dir = %q", dir)
	}
	content := readFile(t, filepath.Join(dir, "SKILL.md"))
	if !strings.Contains(content, "Write tests.") || strings.Contains(content, "model") {
		t.Errorf("SKILL.md = %q", content)
	}
	if got := readFile(t, filepath.Join(dir, "schema.json")); got != `{"type":"object"}` {
		t.Errorf("staged schema = %q", got)
	}
	if got := readFile(t, filepath.Join(workspace, "schema.json")); got != `{"type":"object"}` {
		t.Errorf("workspace schema = %q", got)
	}
}

func TestStageSkillFromDiskMirrorsScripts(t *testing.T) {
	source := t.TempDir()
	writeSkillFile(t, filepath.Join(source, "audit", "SKILL.md"), "---\nname: audit\ndescription: Audit\n---\nRun bash scripts/check.sh\n", 0o644)
	writeSkillFile(t, filepath.Join(source, "audit", "scripts", "check.sh"), "#!/bin/sh\n", 0o755)
	writeSkillFile(t, filepath.Join(source, "audit", "scripts", "lib", "util.sh"), "util\n", 0o644)
	skill, err := skills.Parse(filepath.Join(source, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	writeSkillFile(t, filepath.Join(workspace, "scripts", "stale.sh"), "old\n", 0o644)

	dir, err := StageSkill(CodexHarness{}, Job{Workspace: workspace, SkillName: "audit"}, nil, skill)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(workspace, "skills", "audit") {
		t.Errorf("dir = %q", dir)
	}
	for _, p := range []string{
		filepath.Join(dir, "scripts", "check.sh"),
		filepath.Join(workspace, "scripts", "check.sh"),
		filepath.Join(workspace, "scripts", "lib", "util.sh"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	info, err := os.Stat(filepath.Join(workspace, "scripts", "check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Errorf("mirrored script mode = %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(workspace, "scripts", "stale.sh")); !os.IsNotExist(err) {
		t.Errorf("stale script survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "schema.json")); !os.IsNotExist(err) {
		t.Errorf("schema.json written without a schema: %v", err)
	}
}

func TestStageSkillInMemory(t *testing.T) {
	skill := &skills.Skill{Name: "triage", Description: "Triage", Body: "Sort the findings.", SchemaJSON: `{"type":"array"}`}
	workspace := t.TempDir()
	dir, err := StageSkill(CopilotHarness{}, Job{Workspace: workspace}, nil, skill)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "SKILL.md")), "Sort the findings.") {
		t.Error("SKILL.md missing body")
	}
	if got := readFile(t, filepath.Join(workspace, "schema.json")); got != `{"type":"array"}` {
		t.Errorf("workspace schema = %q", got)
	}
}

func TestStageSkillRejectsUnsafeInput(t *testing.T) {
	skill := &skills.Skill{Name: "audit", Description: "Audit"}
	workspace := t.TempDir()
	for _, job := range []Job{
		{Workspace: workspace, SkillName: "../outside"},
		{Workspace: workspace, SkillName: "a/b"},
		{Workspace: workspace, OutputFile: "../report.json"},
		{},
	} {
		if _, err := StageSkill(ClaudeHarness{}, job, nil, skill); err == nil {
			t.Errorf("StageSkill accepted %+v", job)
		}
	}
	if _, err := StageSkill(nil, Job{Workspace: workspace}, nil, skill); err == nil {
		t.Error("StageSkill accepted a nil harness")
	}
	if _, err := StageSkill(ClaudeHarness{}, Job{Workspace: workspace}, nil, nil); err == nil {
		t.Error("StageSkill accepted a nil skill")
	}
}

func TestStageSkillRefusesSymlinkedSchema(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.json")
	writeSkillFile(t, outside, "keep", 0o644)
	if err := os.Symlink(outside, filepath.Join(workspace, "schema.json")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	skill := &skills.Skill{Name: "audit", Description: "Audit", SchemaJSON: "{}"}
	if _, err := StageSkill(ClaudeHarness{}, Job{Workspace: workspace}, nil, skill); err == nil {
		t.Fatal("StageSkill followed a schema.json symlink out of the workspace")
	}
	if got := readFile(t, outside); got != "keep" {
		t.Errorf("outside file = %q", got)
	}
}
