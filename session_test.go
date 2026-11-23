package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// sessionRun records the jobs it receives. A job resuming "dead" fails with
// failure after emitting events, any other job succeeds as session "fresh".
func sessionRun(failure error, events ...Event) (func(context.Context, Job, func(Event)) error, *[]Job) {
	var jobs []Job
	return func(_ context.Context, j Job, emit func(Event)) error {
		jobs = append(jobs, j)
		if j.ResumeSessionID == "dead" {
			for _, event := range events {
				emit(event)
			}
			return failure
		}
		emit(Event{Kind: KindSession, SessionID: "fresh"})
		return nil
	}, &jobs
}

func TestRunSessionReportsSession(t *testing.T) {
	run, jobs := sessionRun(nil)
	var events []Event
	s, err := RunSession(t.Context(), run, Job{Prompt: "go"}, func(event Event) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if s != (Session{ID: "fresh"}) {
		t.Errorf("session = %+v", s)
	}
	if len(*jobs) != 1 || len(events) != 1 || events[0].SessionID != "fresh" {
		t.Errorf("jobs = %+v, events = %+v", *jobs, events)
	}
}

func TestRunSessionReportsMaxTurns(t *testing.T) {
	failure := errors.New("exit status 1")
	run, _ := sessionRun(failure, Event{Kind: KindSession, SessionID: "dead"}, Event{Kind: KindError, Text: maxTurnsText})
	s, err := RunSession(t.Context(), run, Job{ResumeSessionID: "dead"}, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("err = %v", err)
	}
	if s != (Session{ID: "dead", MaxTurns: true}) {
		t.Errorf("session = %+v", s)
	}
}

func TestRunSessionRestartsFreshResume(t *testing.T) {
	run, jobs := sessionRun(errors.New("exit status 1"), Event{Kind: KindError, Text: "No conversation found with session ID: dead"})
	var texts []string
	s, err := RunSession(t.Context(), run, Job{Prompt: "start over", ResumeSessionID: "dead", ResumePrompt: "fix it"}, func(event Event) {
		if event.Kind == KindText {
			texts = append(texts, event.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "fresh" {
		t.Errorf("session = %+v", s)
	}
	if len(*jobs) != 2 || (*jobs)[1].ResumeSessionID != "" || (*jobs)[1].Prompt != "start over" {
		t.Errorf("jobs = %+v", *jobs)
	}
	if !slices.ContainsFunc(texts, func(text string) bool { return strings.Contains(text, "restarting fresh") }) {
		t.Errorf("texts = %q", texts)
	}
}

func TestRunSessionKeepsFailedResume(t *testing.T) {
	failure := errors.New("exit status 1")
	missing := Event{Kind: KindText, Text: "No conversation found with session ID: dead"}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for name, tc := range map[string]struct {
		ctx    context.Context
		job    Job
		events []Event
	}{
		"resume prompt only": {t.Context(), Job{ResumeSessionID: "dead", ResumePrompt: "fix it"}, []Event{missing}},
		"startup failure":    {t.Context(), Job{Prompt: "go", ResumeSessionID: "dead"}, []Event{{Kind: KindText, Text: "connect ECONNREFUSED"}}},
		"cancelled":          {cancelled, Job{Prompt: "go", ResumeSessionID: "dead"}, []Event{missing}},
		"session announced":  {t.Context(), Job{Prompt: "go", ResumeSessionID: "dead"}, []Event{{Kind: KindSession, SessionID: "dead"}, missing}},
		"fresh run":          {t.Context(), Job{Prompt: "go"}, []Event{missing}},
	} {
		t.Run(name, func(t *testing.T) {
			run, jobs := sessionRun(failure, tc.events...)
			if tc.job.ResumeSessionID == "" {
				run = func(_ context.Context, j Job, emit func(Event)) error {
					*jobs = append(*jobs, j)
					emit(missing)
					return failure
				}
			}
			_, err := RunSession(tc.ctx, run, tc.job, nil)
			if !errors.Is(err, failure) {
				t.Errorf("err = %v, want %v", err, failure)
			}
			if len(*jobs) != 1 {
				t.Errorf("jobs = %+v, want one run", *jobs)
			}
		})
	}
}

func TestRunSessionStopsAntigravityFreshResume(t *testing.T) {
	for name, tc := range map[string]struct {
		job  Job
		runs int
	}{
		"restarts with the prompt": {Job{Prompt: "start over", ResumeSessionID: "dead", ResumePrompt: "fix it"}, 2},
		"resume prompt only":       {Job{ResumeSessionID: "dead", ResumePrompt: "fix it"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			var jobs []Job
			stopped := false
			run := func(ctx context.Context, j Job, emit func(Event)) error {
				jobs = append(jobs, j)
				if j.ResumeSessionID == "" {
					emit(Event{Kind: KindSession, SessionID: "fresh"})
					return nil
				}
				emit(Event{Kind: KindText, Text: `warning: conversation "dead" not found`})
				stopped = ctx.Err() != nil
				emit(Event{Kind: KindSession, SessionID: "throwaway"})
				return ctx.Err()
			}
			var sessions []string
			s, err := RunSession(t.Context(), run, tc.job, func(e Event) {
				if e.Kind == KindSession {
					sessions = append(sessions, e.SessionID)
				}
			})
			if !stopped {
				t.Error("the run that lost its conversation was not cancelled")
			}
			if slices.Contains(sessions, "throwaway") {
				t.Errorf("emit forwarded agy's throwaway session: %v", sessions)
			}
			if len(jobs) != tc.runs {
				t.Fatalf("jobs = %+v, want %d runs", jobs, tc.runs)
			}
			if tc.runs == 2 && (err != nil || s.ID != "fresh" || jobs[1].ResumeSessionID != "" || jobs[1].Prompt != "start over") {
				t.Errorf("session = %+v, err = %v, jobs = %+v", s, err, jobs)
			}
			if tc.runs == 1 && (err == nil || !strings.Contains(err.Error(), "conversation dead not found") || s.ID != "") {
				t.Errorf("session = %+v, err = %v", s, err)
			}
		})
	}
}

func TestRunSessionStopsCapturedAntigravityFreshResume(t *testing.T) {
	data, err := os.ReadFile("testdata/antigravity-1.5.15-lost-resume.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var runs int
	run := func(ctx context.Context, j Job, emit func(Event)) error {
		runs++
		if j.ResumeSessionID == "" {
			emit(Event{Kind: KindSession, SessionID: "fresh"})
			return nil
		}
		// A cancelled agy is killed, so nothing it would print afterwards arrives.
		AntigravityHarness{}.ParseStream(strings.NewReader(string(data)), func(e Event) {
			if ctx.Err() == nil {
				emit(e)
			}
		})
		return ctx.Err()
	}
	var kinds []string
	s, err := RunSession(t.Context(), run, Job{Prompt: "start over", ResumeSessionID: "0b8c1d2e-0000-4000-8000-000000000000", ResumePrompt: "Fix it."}, func(e Event) {
		kinds = append(kinds, e.Kind)
	})
	if err != nil || s.ID != "fresh" || runs != 2 {
		t.Errorf("session = %+v, err = %v, runs = %d", s, err, runs)
	}
	if slices.Contains(kinds, KindTool) || slices.Contains(kinds, KindUsage) {
		t.Errorf("the lost resume kept running: %v", kinds)
	}
}

// sessionRunHarness parses the helper's output as claude does, so stderr
// reaches RunSession the way Run delivers it.
type sessionRunHarness struct{ runTestHarness }

func (sessionRunHarness) Args(j Job) []string {
	return []string{"-test.run=TestRunSessionHelperProcess", "--", j.ResumeSessionID}
}

func (sessionRunHarness) ParseStream(r io.Reader, emit func(Event)) {
	ClaudeHarness{}.ParseStream(r, emit)
}

func (sessionRunHarness) Env(string) []string { return []string{"HARNESS_SESSION_HELPER=1"} }

func TestRunSessionThroughRun(t *testing.T) {
	for name, tc := range map[string]struct {
		resume  string
		session string
		runs    int
	}{
		"missing conversation": {"missing", "fresh", 2},
		"startup failure":      {"flaky", "", 1},
	} {
		t.Run(name, func(t *testing.T) {
			runs := 0
			run := func(ctx context.Context, j Job, emit func(Event)) error {
				runs++
				return Run(ctx, sessionRunHarness{}, j, emit)
			}
			s, err := RunSession(t.Context(), run, Job{Workspace: t.TempDir(), Prompt: "go", ResumeSessionID: tc.resume}, nil)
			if (err == nil) != (tc.session != "") || s.ID != tc.session || runs != tc.runs {
				t.Errorf("session = %+v, err = %v, runs = %d", s, err, runs)
			}
		})
	}
}

func TestRunSessionHelperProcess(t *testing.T) {
	if os.Getenv("HARNESS_SESSION_HELPER") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "missing":
		_, _ = fmt.Fprintln(os.Stderr, "No conversation found with session ID: missing")
		os.Exit(1)
	case "flaky":
		_, _ = fmt.Fprintln(os.Stderr, "connect ECONNREFUSED")
		os.Exit(1)
	}
	_, _ = fmt.Println(`{"type":"system","subtype":"init","session_id":"fresh"}`)
	os.Exit(0)
}
