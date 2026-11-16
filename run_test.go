package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type runTestHarness struct{}

func (runTestHarness) Binary() string { return os.Args[0] }

func (runTestHarness) Args(j Job) []string {
	return []string{"-test.run=TestRunHelperProcess", "--", j.Prompt}
}

func (runTestHarness) Prompt(j Job) string { return j.Prompt }

func (runTestHarness) ParseStream(r io.Reader, emit func(Event)) {
	scanJSONL(r, emit, func(raw []byte, emit func(Event)) {
		var event Event
		if json.Unmarshal(raw, &event) == nil {
			emit(event)
		}
	})
}

func (runTestHarness) SkillDir(workspace, name string) string {
	return filepath.Join(workspace, "skills", name)
}

func (runTestHarness) GuideFilename() string     { return "AGENTS.md" }
func (runTestHarness) SystemPromptViaArgs() bool { return false }
func (runTestHarness) EgressHosts() []string     { return nil }
func (runTestHarness) StateEnv(string) []string  { return nil }
func (runTestHarness) DefaultModels() []ModelDefault {
	return nil
}

func (runTestHarness) Env(string) []string {
	return []string{"HARNESS_RUN_HELPER=1", "HARNESS_RUN_VALUE=override"}
}

func (runTestHarness) AccountErrorText(s string) string {
	return claudeAccountErrorText(s)
}

func TestRun(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HARNESS_RUN_VALUE", "old")
	var events []Event
	err := Run(t.Context(), runTestHarness{}, Job{
		Workspace:    workspace,
		Prompt:       "ok",
		SystemPrompt: "Follow the local rules.",
	}, func(event Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != KindText || events[0].Text != "override" {
		t.Fatalf("events = %+v", events)
	}
	guide, err := os.ReadFile(filepath.Join(workspace, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(guide) != "Follow the local rules.\n" {
		t.Errorf("guide = %q", guide)
	}
}

func TestRunAccountError(t *testing.T) {
	for _, prompt := range []string{"account", "account-json"} {
		err := Run(t.Context(), runTestHarness{}, Job{
			Workspace: t.TempDir(),
			Prompt:    prompt,
		}, nil)
		var accountErr *AccountError
		if err == nil || !strings.Contains(err.Error(), "quota exceeded") {
			t.Fatalf("Run(%q) error = %v", prompt, err)
		}
		if !asAccountError(err, &accountErr) {
			t.Fatalf("Run(%q) returned %T, want *AccountError", prompt, err)
		}
	}
}

func TestStreamCmdKeepsBoundedDiagnostics(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestRunHelperProcess", "--", "large-stderr")
	cmd.Env = append(os.Environ(), "HARNESS_RUN_HELPER=1")
	stderr, err := StreamCmd(cmd, runTestHarness{}, nil)
	var account *AccountError
	if len(stderr) != maxStderrBytes || !strings.HasSuffix(stderr, "quota exceeded\n") || !asAccountError(err, &account) {
		t.Fatalf("stderr bytes = %d, error = %v", len(stderr), err)
	}
}

// TestStreamCmdClassifiesStderrBeyondTail pins that account phrases written
// before the retained stderr tail still classify the failure.
func TestStreamCmdClassifiesStderrBeyondTail(t *testing.T) {
	for _, tc := range []struct {
		prompt string
		detail string
	}{
		{"early-quota", "quota exceeded"},
		{"revoked-then-rate-limit", "access has been revoked"},
		{"large-noise", ""},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestRunHelperProcess", "--", tc.prompt)
			cmd.Env = append(os.Environ(), "HARNESS_RUN_HELPER=1")
			_, err := StreamCmd(cmd, runTestHarness{}, nil)
			var account *AccountError
			if tc.detail == "" {
				if err == nil || asAccountError(err, &account) {
					t.Fatalf("error = %v, want a plain process error", err)
				}
				return
			}
			if !asAccountError(err, &account) || account.Detail != tc.detail {
				t.Fatalf("error = %v, want account error %q", err, tc.detail)
			}
		})
	}
}

func asAccountError(err error, target **AccountError) bool {
	account, ok := err.(*AccountError)
	if ok {
		*target = account
	}
	return ok
}

func TestRunHelperProcess(t *testing.T) {
	if os.Getenv("HARNESS_RUN_HELPER") != "1" {
		return
	}
	prompt := os.Args[len(os.Args)-1]
	if prompt == "large-stderr" {
		_, _ = io.WriteString(os.Stderr, strings.Repeat("x", maxStderrBytes*2)+"\nquota exceeded\n")
		os.Exit(2)
	}
	noise := strings.Repeat("x", maxStderrBytes*2) + "\n"
	switch prompt {
	case "early-quota":
		_, _ = io.WriteString(os.Stderr, "quota exceeded\n"+noise)
		os.Exit(2)
	case "revoked-then-rate-limit":
		_, _ = io.WriteString(os.Stderr, "access has been revoked\n"+noise+"rate limit\n")
		os.Exit(2)
	case "large-noise":
		_, _ = io.WriteString(os.Stderr, noise)
		os.Exit(2)
	}
	if prompt == "account" {
		_, _ = fmt.Fprintln(os.Stderr, "quota exceeded")
		os.Exit(2)
	}
	if prompt == "account-json" {
		encoded, _ := json.Marshal(Event{Kind: KindError, Text: "quota exceeded"})
		_, _ = fmt.Println(string(encoded))
		os.Exit(2)
	}
	event := Event{Kind: KindText, Text: os.Getenv("HARNESS_RUN_VALUE")}
	encoded, _ := json.Marshal(event)
	_, _ = fmt.Println(string(encoded))
	os.Exit(0)
}

func TestMergeEnv(t *testing.T) {
	t.Parallel()
	got := mergeEnv([]string{"A=1", "B=2"}, []string{"A=3", "C=4"})
	want := []string{"A=3", "B=2", "C=4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("mergeEnv() = %v, want %v", got, want)
	}
}
