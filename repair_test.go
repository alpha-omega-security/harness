package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repairSchema = `{"type":"object","required":["verdict"],"properties":{"verdict":{"enum":["valid","invalid"]}},"additionalProperties":false}`

// repairRun writes outputs[i] to report.json on the i-th call, or nothing for
// an empty string, announces session "s1" and fails with failures[i] when set.
func repairRun(workspace string, outputs []string, failures ...error) (func(context.Context, Job, func(Event)) error, *[]Job) {
	var jobs []Job
	return func(_ context.Context, j Job, emit func(Event)) error {
		i := len(jobs)
		jobs = append(jobs, j)
		emit(Event{Kind: KindSession, SessionID: "s1"})
		if i < len(outputs) && outputs[i] != "" {
			if err := os.WriteFile(filepath.Join(workspace, "report.json"), []byte(outputs[i]), 0o600); err != nil {
				return err
			}
		}
		if i < len(failures) {
			return failures[i]
		}
		return nil
	}, &jobs
}

func repairJob(workspace string) Job {
	return Job{Workspace: workspace, Prompt: "Analyse it.", OutputFile: "report.json", MaxTurns: 80}
}

func TestRunRepairedKeepsAValidOutput(t *testing.T) {
	workspace := t.TempDir()
	run, jobs := repairRun(workspace, []string{`{"verdict":"valid"}`})
	output, s, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{Schema: []byte(repairSchema)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != `{"verdict":"valid"}` || s.ID != "s1" || len(*jobs) != 1 {
		t.Errorf("output = %s, session = %+v, jobs = %d", output, s, len(*jobs))
	}
}

func TestRunRepairedResumesWithTheValidationErrors(t *testing.T) {
	workspace := t.TempDir()
	run, jobs := repairRun(workspace, []string{`{"verdict":"maybe"}`, `{"verdict":"invalid"}`})
	output, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{Schema: []byte(repairSchema)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != `{"verdict":"invalid"}` || len(*jobs) != 2 {
		t.Fatalf("output = %s, jobs = %+v", output, *jobs)
	}
	repair := (*jobs)[1]
	if repair.ResumeSessionID != "s1" || repair.Prompt != "" || repair.MaxTurns != DefaultRepairMaxTurns {
		t.Errorf("repair job = %+v", repair)
	}
	for _, want := range []string{"/verdict:", `{"verdict":"maybe"}`, "./report.json"} {
		if !strings.Contains(repair.ResumePrompt, want) {
			t.Errorf("repair prompt misses %q: %s", want, repair.ResumePrompt)
		}
	}
}

func TestRunRepairedAsksForAMissingOutput(t *testing.T) {
	workspace := t.TempDir()
	run, jobs := repairRun(workspace, []string{"", `{"verdict":"valid"}`})
	output, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{MaxTurns: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != `{"verdict":"valid"}` || (*jobs)[1].MaxTurns != 2 || !strings.Contains((*jobs)[1].ResumePrompt, "was not written") {
		t.Errorf("output = %s, jobs = %+v", output, *jobs)
	}
}

func TestRunRepairedRunsTheCallerValidation(t *testing.T) {
	workspace := t.TempDir()
	run, jobs := repairRun(workspace, []string{`{"verdict":"invalid"}`, `{"verdict":"invalid"}`})
	validate := func([]byte) error { return errors.New("the verdict contradicts the evidence") }
	_, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{Schema: []byte(repairSchema), Validate: validate}, nil)
	var invalid *ValidationError
	if !errors.As(err, &invalid) || !strings.Contains(invalid.Detail, "contradicts") || len(*jobs) != 2 {
		t.Fatalf("err = %v, jobs = %d", err, len(*jobs))
	}
}

func TestRunRepairedRejectsOnAnEmptyCallerError(t *testing.T) {
	workspace := t.TempDir()
	run, jobs := repairRun(workspace, []string{`{"verdict":"invalid"}`, `{"verdict":"invalid"}`})
	validate := func([]byte) error { return errors.New("") }
	output, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{Validate: validate}, nil)
	var invalid *ValidationError
	if !errors.As(err, &invalid) || invalid.Detail == "" || output != nil || len(*jobs) != 2 {
		t.Fatalf("output = %s, err = %v, jobs = %d", output, err, len(*jobs))
	}
}

func TestRunRepairedKeepsTheOriginalWhenTheRepairFails(t *testing.T) {
	workspace := t.TempDir()
	run, _ := repairRun(workspace, []string{`{"verdict":"maybe"}`, `{"verdict":"still"}`})
	output, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{Schema: []byte(repairSchema)}, nil)
	var invalid *ValidationError
	if output != nil || !errors.As(err, &invalid) {
		t.Fatalf("output = %s, err = %v", output, err)
	}
	if string(invalid.Output) != `{"verdict":"maybe"}` || !strings.Contains(invalid.Detail, "after repair") {
		t.Errorf("validation error = %+v", invalid)
	}
}

func TestRunRepairedJoinsAFailedRepairRun(t *testing.T) {
	workspace := t.TempDir()
	failure := errors.New("exit status 1")
	run, _ := repairRun(workspace, []string{`{"verdict":"maybe"}`}, nil, failure)
	_, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{Schema: []byte(repairSchema)}, nil)
	var invalid *ValidationError
	if !errors.Is(err, failure) || !errors.As(err, &invalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRepairedKeepsAValidOutputFromAFailedRepairRun(t *testing.T) {
	workspace := t.TempDir()
	failure := errors.New("exit status 1")
	run, jobs := repairRun(workspace, []string{`{"verdict":"maybe"}`, `{"verdict":"valid"}`}, nil, failure)
	output, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{Schema: []byte(repairSchema)}, nil)
	var invalid *ValidationError
	if string(output) != `{"verdict":"valid"}` || !errors.Is(err, failure) || errors.As(err, &invalid) || len(*jobs) != 2 {
		t.Fatalf("output = %s, err = %v, jobs = %d", output, err, len(*jobs))
	}
}

func TestRunRepairedDoesNotRepairWithoutASession(t *testing.T) {
	workspace := t.TempDir()
	var calls int
	run := func(context.Context, Job, func(Event)) error {
		calls++
		return os.WriteFile(filepath.Join(workspace, "report.json"), []byte(`{}`), 0o600)
	}
	_, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{Schema: []byte(repairSchema)}, nil)
	var invalid *ValidationError
	if !errors.As(err, &invalid) || calls != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls)
	}
}

func TestRunRepairedOnAFailedRun(t *testing.T) {
	failure := errors.New("exit status 1")
	for _, tt := range []struct {
		name, output string
		want         bool
	}{
		{"keeps a valid output", `{"verdict":"valid"}`, true},
		{"drops an invalid output", `{"verdict":"maybe"}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			run, jobs := repairRun(workspace, []string{tt.output}, failure)
			output, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{Schema: []byte(repairSchema)}, nil)
			if !errors.Is(err, failure) || (output != nil) != tt.want || len(*jobs) != 1 {
				t.Errorf("output = %s, err = %v, jobs = %d", output, err, len(*jobs))
			}
		})
	}
}

func TestRunRepairedRefusesAnOversizedOutput(t *testing.T) {
	workspace := t.TempDir()
	run, _ := repairRun(workspace, []string{`{"verdict":"valid"}`, `{"verdict":"valid"}`})
	_, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{MaxBytes: 8}, nil)
	var invalid *ValidationError
	if !errors.As(err, &invalid) || !strings.Contains(invalid.Detail, "larger than 8 bytes") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRepairedDoesNotFollowALink(t *testing.T) {
	workspace := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(secret, []byte(`{"verdict":"valid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(workspace, "report.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	run, _ := repairRun(workspace, nil)
	output, _, err := RunRepaired(t.Context(), run, repairJob(workspace), Repair{}, nil)
	var invalid *ValidationError
	if output != nil || !errors.As(err, &invalid) || invalid.Output != nil {
		t.Fatalf("output = %s, err = %v", output, err)
	}
}

func TestRunRepairedRejectsItsArguments(t *testing.T) {
	run, jobs := repairRun(t.TempDir(), nil)
	if _, _, err := RunRepaired(t.Context(), run, Job{}, Repair{}, nil); err == nil {
		t.Error("a job without OutputFile was accepted")
	}
	if _, _, err := RunRepaired(t.Context(), run, repairJob(t.TempDir()), Repair{Schema: []byte(`{"type":`)}, nil); err == nil {
		t.Error("a broken schema was accepted")
	}
	if len(*jobs) != 0 {
		t.Errorf("ran %d jobs", len(*jobs))
	}
}
