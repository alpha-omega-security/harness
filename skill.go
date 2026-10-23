package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/git-pkgs/skills"
)

const (
	skillSchemaFile  = "schema.json"
	skillScriptsDir  = "scripts"
	stagedDirPerm    = 0o755
	stagedFilePerm   = 0o644
	stagedExecPerm   = 0o755
	executableBits   = 0o111
	workspaceRootDir = "."
)

// StageSkill writes skill where h discovers it for job and returns that
// directory. Sibling files are read from fsys under skill.Dir, or from
// skill.SourcePath when fsys is nil, so skills embedded in a binary, on disk,
// or built in memory all stage the same way. Frontmatter keys outside the
// Agent Skills spec are left out.
//
// schema.json and scripts/ are also copied to the workspace root, because
// skills refer to ./schema.json and scripts/... relative to the working
// directory. An existing scripts/ there is replaced.
func StageSkill(h Harness, job Job, fsys fs.FS, skill *skills.Skill) (string, error) {
	if h == nil {
		return "", errors.New("harness: stage skill: harness is required")
	}
	if skill == nil {
		return "", errors.New("harness: stage skill: skill is required")
	}
	if job.Workspace == "" {
		return "", errors.New("harness: stage skill: workspace is required")
	}
	name := job.SkillName
	if name == "" {
		name = skill.Name
	}
	if err := skills.ValidateName(name); err != nil {
		return "", fmt.Errorf("harness: stage skill: %w", err)
	}
	if job.OutputFile != "" && !filepath.IsLocal(job.OutputFile) {
		return "", fmt.Errorf("harness: stage skill: output file %q is outside the workspace", job.OutputFile)
	}
	destination := h.SkillDir(job.Workspace, name)
	if destination == "" {
		return "", fmt.Errorf("harness: stage skill: %s does not support staged skills", Name(h))
	}
	if err := skills.Stage(fsys, skill, job.Workspace, destination, skills.WithoutExtra()); err != nil {
		return "", fmt.Errorf("harness: stage skill: %w", err)
	}
	if err := mirrorSkillFiles(job.Workspace, destination); err != nil {
		return "", fmt.Errorf("harness: stage skill: %w", err)
	}
	return destination, nil
}

// mirrorSkillFiles copies the staged schema.json and scripts/ into the
// workspace root through an os.Root, so symlinks in the workspace cannot
// redirect the writes outside it.
func mirrorSkillFiles(workspace, staged string) error {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	schema, err := os.ReadFile(filepath.Join(staged, skillSchemaFile))
	switch {
	case err == nil:
		if err := root.WriteFile(skillSchemaFile, schema, stagedFilePerm); err != nil {
			return fmt.Errorf("mirror %s: %w", skillSchemaFile, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	scripts := os.DirFS(filepath.Join(staged, skillScriptsDir))
	if _, err := fs.Stat(scripts, workspaceRootDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := root.RemoveAll(skillScriptsDir); err != nil {
		return fmt.Errorf("mirror %s: %w", skillScriptsDir, err)
	}
	err = fs.WalkDir(scripts, workspaceRootDir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := path.Join(skillScriptsDir, p)
		if entry.IsDir() {
			return root.MkdirAll(target, stagedDirPerm)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := fs.ReadFile(scripts, p)
		if err != nil {
			return err
		}
		perm := os.FileMode(stagedFilePerm)
		if info.Mode().Perm()&executableBits != 0 {
			perm = stagedExecPerm
		}
		return root.WriteFile(target, data, perm)
	})
	if err != nil {
		return fmt.Errorf("mirror %s: %w", skillScriptsDir, err)
	}
	return nil
}
