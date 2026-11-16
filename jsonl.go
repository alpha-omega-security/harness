package harness

import (
	"bufio"
	"io"
)

const maxJSONLLine = 16 << 20

// Oversized lines are drained so a hostile tool result cannot exhaust the host
// or hide the session, usage and error events that follow it.
func scanJSONL(r io.Reader, emit func(Event), parse func([]byte, func(Event))) {
	br := bufio.NewReader(r)
	var raw []byte
	var oversized bool
	for {
		part, err := br.ReadSlice('\n')
		if !oversized {
			if len(raw)+len(part) > maxJSONLLine {
				raw, oversized = nil, true
				emit(Event{Kind: KindError, Text: "stream line exceeds 16 MiB"})
			} else {
				raw = append(raw, part...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if !oversized && len(raw) > 0 {
			parse(raw, emit)
		}
		raw, oversized = raw[:0], false
		if err == io.EOF {
			return
		}
		if err != nil {
			emit(Event{Kind: KindError, Text: "stream read: " + err.Error()})
			return
		}
	}
}
