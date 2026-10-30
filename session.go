package harness

import (
	"context"
	"slices"
	"strings"
)

// maxTurnsText is the error event text a backend emits when it stops on
// Job.MaxTurns.
const maxTurnsText = "hit max turns"

// missingSessionTexts are what claude and codex print when the conversation
// to resume is gone.
var missingSessionTexts = []string{"No conversation found with session ID", "no rollout found for thread id"}

// Session is what one invocation reported about its conversation.
type Session struct {
	// ID is the session the backend announced, empty when it announced none.
	ID string
	// MaxTurns reports that the backend stopped on Job.MaxTurns.
	MaxTurns bool

	missing bool
}

// RunSession calls run with j, which harness.Run and container.Scope.Run
// both fit, and returns the session the backend announced. A resume that
// fails because the backend no longer has the conversation is run once more
// without ResumeSessionID, unless j has a ResumePrompt and no Prompt: a caller
// that wants that fallback sets Prompt.
func RunSession(ctx context.Context, run func(context.Context, Job, func(Event)) error, j Job, emit func(Event)) (Session, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	s, err := runSession(ctx, run, j, emit)
	if err == nil || j.ResumeSessionID == "" || s.ID != "" || !s.missing || ctx.Err() != nil {
		return s, err
	}
	if j.Prompt == "" && j.ResumePrompt != "" {
		// A bare resume prompt is a corrective nudge ("rewrite the invalid
		// report.json") that means nothing to a fresh agent, and there is
		// no fresh framing to fall back on.
		emit(Event{Kind: KindText, Text: "resume of session " + j.ResumeSessionID + " failed; not restarting a resume prompt fresh"})
		return s, err
	}
	// The backend could not load the saved conversation (expired or
	// pruned). Restart fresh in the same workspace so the retry lineage
	// isn't permanently wedged on a dead session id.
	emit(Event{Kind: KindText, Text: "resume of session " + j.ResumeSessionID + " failed; restarting fresh"})
	j.ResumeSessionID = ""

	return runSession(ctx, run, j, emit)
}

func runSession(ctx context.Context, run func(context.Context, Job, func(Event)) error, j Job, emit func(Event)) (Session, error) {
	var s Session
	err := run(ctx, j, func(event Event) {
		switch {
		case event.Kind == KindSession && event.SessionID != "":
			s.ID = event.SessionID
		case event.Kind == KindError && event.Text == maxTurnsText:
			s.MaxTurns = true
		case event.Kind == KindText || event.Kind == KindError:
			s.missing = s.missing || slices.ContainsFunc(missingSessionTexts, func(text string) bool { return strings.Contains(event.Text, text) })
		}
		emit(event)
	})
	return s, err
}
