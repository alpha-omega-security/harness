package harness

import (
	"context"
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
	if emit == nil {
		emit = func(Event) {}
	}
	if err := WriteSystemPrompt(h, j); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, h.Binary(), h.Args(j)...)
	cmd.Dir = j.Workspace
	cmd.Env = mergeEnv(os.Environ(), expandEnv(h.Env(j.BaseURL)))
	prepareProcessGroup(cmd)
	cmd.Cancel = func() error {
		return terminateProcessGroup(cmd.Process)
	}

	reader, writer := io.Pipe()
	var stderr strings.Builder
	cmd.Stdout = writer
	cmd.Stderr = io.MultiWriter(writer, &stderr)

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
		return fmt.Errorf("harness: start %s: %w", h.Binary(), err)
	}
	runErr := cmd.Wait()
	_ = writer.Close()
	<-parseDone
	if runErr == nil {
		return nil
	}
	detail := h.AccountErrorText(stderr.String())
	for _, text := range parsedErrors {
		detail = PreferAccountErrorText(detail, h.AccountErrorText(text))
	}
	if detail != "" {
		return &AccountError{Detail: detail}
	}
	return fmt.Errorf("harness: %s: %w", h.Binary(), runErr)
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
