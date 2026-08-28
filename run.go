package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Run starts h as a local subprocess in j.Workspace and streams parsed events
// to emit.
func Run(ctx context.Context, h Harness, j Job, emit func(Event)) error {
	if h == nil {
		return fmt.Errorf("harness: backend is required")
	}
	if j.Workspace == "" {
		return fmt.Errorf("harness: workspace is required")
	}
	if err := WriteSystemPrompt(h, j); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, h.Binary(), h.Args(j)...)
	cmd.Dir = j.Workspace
	cmd.Env = mergeEnv(os.Environ(), expandEnv(h.Env(j.BaseURL)))

	if _, err := StreamCmd(cmd, h, emit); err != nil {
		var accountErr *AccountError
		if errors.As(err, &accountErr) {
			return err
		}
		return fmt.Errorf("harness: %s: %w", h.Binary(), err)
	}
	return nil
}

// StreamCmd starts cmd, streams its combined output through h.ParseStream to
// emit, and returns after the process exits and parsing completes. It sets
// cmd.SysProcAttr and cmd.Cancel so context cancellation SIGTERMs the process
// group instead of orphaning children. On non-zero exit it classifies stderr
// and any parsed KindError event via h.AccountErrorText and returns an
// *AccountError on a match; otherwise it returns the raw exec error unwrapped
// so the caller can add its own context. stderr is returned so a caller can
// include a runtime failure message in that error.
func StreamCmd(cmd *exec.Cmd, h Harness, emit func(Event)) (stderr string, err error) {
	if emit == nil {
		emit = func(Event) {}
	}
	prepareProcessGroup(cmd)
	cmd.Cancel = func() error {
		return terminateProcessGroup(cmd.Process)
	}

	reader, writer := io.Pipe()
	var stderrBuf strings.Builder
	cmd.Stdout = writer
	cmd.Stderr = io.MultiWriter(writer, &stderrBuf)

	parseDone := make(chan struct{})
	// JSON-output backends may report provider failures on stdout rather than
	// stderr, so retain parsed error events for account classification.
	var parsedErrors []string
	go func() {
		h.ParseStream(reader, func(event Event) {
			if event.Kind == KindError && event.Text != "" {
				parsedErrors = append(parsedErrors, event.Text)
			}
			emit(event)
		})
		close(parseDone)
	}()

	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		<-parseDone
		return "", err
	}
	runErr := cmd.Wait()
	_ = writer.Close()
	<-parseDone
	stderr = stderrBuf.String()
	if runErr == nil {
		return stderr, nil
	}
	detail := h.AccountErrorText(stderr)
	for _, text := range parsedErrors {
		detail = PreferAccountErrorText(detail, h.AccountErrorText(text))
	}
	if detail != "" {
		return stderr, &AccountError{Detail: detail}
	}
	return stderr, runErr
}

func expandEnv(entries []string) []string {
	expanded := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.ContainsRune(entry, '=') {
			expanded = append(expanded, entry)
			continue
		}
		expanded = append(expanded, entry+"="+os.Getenv(entry))
	}
	return expanded
}

func mergeEnv(base, overrides []string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	add := func(entry string) {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			key = entry
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = entry
	}
	for _, entry := range base {
		add(entry)
	}
	for _, entry := range overrides {
		add(entry)
	}
	env := make([]string, 0, len(order))
	for _, key := range order {
		env = append(env, values[key])
	}
	return env
}
