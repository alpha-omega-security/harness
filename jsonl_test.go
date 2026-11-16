package harness

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestScanJSONLRecoversAfterOversizedLines(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		failures    int
	}{
		{"first\n" + strings.Repeat("x", maxJSONLLine+1) + "\nlast", "first\nlast", 1},
		{"first\n" + strings.Repeat("x", maxJSONLLine+1), "first\n", 1},
		{"first\nlast", "first\nlast", 0},
	} {
		var lines []string
		var failures int
		scanJSONL(strings.NewReader(tc.input), func(e Event) {
			if e.Kind != KindError {
				t.Errorf("unexpected event: %+v", e)
			}
			failures++
		}, func(raw []byte, _ func(Event)) { lines = append(lines, string(raw)) })
		if got := strings.Join(lines, ""); got != tc.want {
			t.Errorf("parsed %q, want %q", got, tc.want)
		}
		if failures != tc.failures {
			t.Errorf("oversized failures = %d", failures)
		}
	}
}

func TestScanJSONLAcceptsTheLimitAndReportsReadErrors(t *testing.T) {
	var sizes []int
	var failures []Event
	scanJSONL(io.MultiReader(strings.NewReader(strings.Repeat("x", maxJSONLLine-1)+"\n"), failingStream{}), func(e Event) {
		failures = append(failures, e)
	}, func(raw []byte, _ func(Event)) { sizes = append(sizes, len(raw)) })
	if len(sizes) != 1 || sizes[0] != maxJSONLLine || len(failures) != 1 || !strings.Contains(failures[0].Text, "broken stream") {
		t.Fatalf("sizes = %v, failures = %+v", sizes, failures)
	}
}

type failingStream struct{}

func (failingStream) Read([]byte) (int, error) { return 0, errors.New("broken stream") }
