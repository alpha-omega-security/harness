package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	// DefaultRepairMaxTurns caps a repair round: rewriting one file needs a
	// few turns, not a new analysis.
	DefaultRepairMaxTurns = 4
	// DefaultOutputMaxBytes caps the OutputFile read by RunRepaired.
	DefaultOutputMaxBytes = 1 << 20

	maxSchemaErrors       = 8
	maxRepairPromptOutput = 4000
)

// Repair is how RunRepaired validates Job.OutputFile.
type Repair struct {
	// Schema is the JSON Schema the output must satisfy. Nil skips schema
	// validation.
	Schema []byte
	// Validate checks what a JSON Schema cannot express. It runs after the
	// schema and may be nil.
	Validate func([]byte) error
	// MaxTurns caps the repair round, DefaultRepairMaxTurns when zero. Like
	// Job.MaxTurns, Codex and OpenCode ignore it.
	MaxTurns int
	// MaxBytes caps the output read, DefaultOutputMaxBytes when zero.
	MaxBytes int64
}

// ValidationError reports an OutputFile that failed validation. Output is
// what the agent wrote, nil when it wrote nothing readable.
type ValidationError struct {
	OutputFile string
	Output     []byte
	Detail     string
}

func (e *ValidationError) Error() string {
	return e.OutputFile + " failed validation: " + e.Detail
}

// RunRepaired runs j through RunSession, then reads and validates
// j.OutputFile in j.Workspace. When the output is invalid and the backend
// announced a session, it resumes that session once with the validation
// errors, capped at r.MaxTurns. It returns the valid output, or a
// *ValidationError holding the original output when the repair did not
// produce a valid one. When a run fails, the first or the repair, it returns
// that error with the output only if the output is valid, and a failed first
// run gets no repair. A file left at j.OutputFile before the run reads as the
// run's output, so the caller starts from a workspace without one.
func RunRepaired(ctx context.Context, run func(context.Context, Job, func(Event)) error, j Job, r Repair, emit func(Event)) ([]byte, Session, error) {
	if j.OutputFile == "" {
		return nil, Session{}, errors.New("harness: RunRepaired needs an OutputFile")
	}
	schema, err := compileSchema(r.Schema)
	if err != nil {
		return nil, Session{}, err
	}
	s, err := RunSession(ctx, run, j, emit)
	output, invalid := r.check(j, schema)
	if err != nil {
		if invalid != nil {
			return nil, s, err
		}
		return output, s, err
	}
	if invalid == nil {
		return output, s, nil
	}
	if s.ID == "" || ctx.Err() != nil {
		return nil, s, invalid
	}

	repair := j
	// An empty Prompt keeps RunSession from restarting a lost conversation
	// fresh: a capped repair round cannot redo the analysis.
	repair.Prompt = ""
	repair.ResumeSessionID = s.ID
	repair.ResumePrompt = repairPrompt(invalid)
	repair.MaxTurns = r.MaxTurns
	if repair.MaxTurns <= 0 {
		repair.MaxTurns = DefaultRepairMaxTurns
	}
	repaired, err := RunSession(ctx, run, repair, emit)
	if repaired.ID != "" {
		s = repaired
	}
	fixed, stillInvalid := r.check(j, schema)
	if stillInvalid == nil {
		return fixed, s, err
	}
	if err != nil {
		return nil, s, errors.Join(invalid, err)
	}
	invalid.Detail += "\nafter repair: " + stillInvalid.Detail
	return nil, s, invalid
}

// check reads and validates the output, returning a *ValidationError when it
// is missing, too large or invalid.
func (r Repair) check(j Job, schema *jsonschema.Schema) ([]byte, *ValidationError) {
	output, detail := r.read(j)
	if detail == "" {
		detail = r.validate(schema, output)
	}
	if detail == "" {
		return output, nil
	}
	return nil, &ValidationError{OutputFile: j.OutputFile, Output: output, Detail: detail}
}

// read opens the output through a root at the workspace and accepts only a
// regular file, so a link the agent planted does not make the host read
// another file.
func (r Repair) read(j Job) ([]byte, string) {
	limit := r.MaxBytes
	if limit <= 0 {
		limit = DefaultOutputMaxBytes
	}
	root, err := os.OpenRoot(j.Workspace)
	if err != nil {
		return nil, "the workspace cannot be opened"
	}
	defer func() { _ = root.Close() }()
	entry, err := root.Lstat(j.OutputFile)
	if err != nil {
		return nil, "the file was not written"
	}
	if !entry.Mode().IsRegular() {
		return nil, "the file is not a regular file"
	}
	f, err := root.Open(j.OutputFile)
	if err != nil {
		return nil, "the file cannot be opened"
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !os.SameFile(entry, info) {
		return nil, "the file changed while it was read"
	}
	output, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, "the file cannot be read"
	}
	if int64(len(output)) > limit {
		return nil, fmt.Sprintf("the file is larger than %d bytes", limit)
	}
	if strings.TrimSpace(string(output)) == "" {
		return nil, "the file is empty"
	}
	return output, ""
}

func (r Repair) validate(schema *jsonschema.Schema, output []byte) string {
	if schema != nil {
		if detail := validateSchema(schema, output); detail != "" {
			return detail
		}
	}
	if r.Validate != nil {
		if err := r.Validate(output); err != nil {
			if detail := err.Error(); detail != "" {
				return detail
			}
			return "the output was rejected"
		}
	}
	return ""
}

func compileSchema(schema []byte) (*jsonschema.Schema, error) {
	if schema == nil {
		return nil, nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("harness: schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", doc); err != nil {
		return nil, fmt.Errorf("harness: schema: %w", err)
	}
	compiled, err := c.Compile("schema.json")
	if err != nil {
		return nil, fmt.Errorf("harness: schema: %w", err)
	}
	return compiled, nil
}

// validateSchema returns one "/pointer: message" line per failure, at most
// maxSchemaErrors, or "" when output satisfies schema.
func validateSchema(schema *jsonschema.Schema, output []byte) string {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(output))
	if err != nil {
		return "the file is not valid JSON: " + err.Error()
	}
	err = schema.Validate(inst)
	if err == nil {
		return ""
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return err.Error()
	}
	var lines []string
	failures := verr.BasicOutput().Errors
	for _, failure := range failures {
		if failure.Error == nil {
			continue
		}
		if len(lines) == maxSchemaErrors {
			lines = append(lines, fmt.Sprintf("... (%d more)", len(failures)-maxSchemaErrors))
			break
		}
		location := failure.InstanceLocation
		if location == "" {
			location = "/"
		}
		lines = append(lines, location+": "+failure.Error.String())
	}
	if len(lines) == 0 {
		return verr.Error()
	}
	return strings.Join(lines, "\n")
}

func repairPrompt(invalid *ValidationError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your ./%s failed validation:\n\n%s\n\nRewrite only ./%s so that it passes. Keep the facts from your previous work, do not restart the analysis, and write nothing else.", invalid.OutputFile, invalid.Detail, invalid.OutputFile)
	if output := strings.TrimSpace(string(invalid.Output)); output != "" {
		if len(output) > maxRepairPromptOutput {
			output = strings.ToValidUTF8(output[:maxRepairPromptOutput], "") + "\n... truncated ..."
		}
		fmt.Fprintf(&b, "\n\nThe invalid ./%s:\n%s", invalid.OutputFile, output)
	}
	return b.String()
}
