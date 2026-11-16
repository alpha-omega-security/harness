package modelproxy

import (
	"fmt"
	"maps"
	"slices"
)

const (
	maxShapeDepth = 32
	shapeOmitted  = "..."
)

// Refused requests are hostile too: bound the traversal and the allocation.
func shape(v any) string {
	w := shapeWriter{data: make([]byte, 0, maxLoggedShape)}
	w.value(v, 0)
	if w.truncated {
		w.data = append(w.data, shapeOmitted...)
	}
	return string(w.data)
}

type shapeWriter struct {
	data      []byte
	truncated bool
}

func (w *shapeWriter) text(s string) {
	if w.truncated {
		return
	}
	left := maxLoggedShape - len(shapeOmitted) - len(w.data)
	if len(s) > left {
		s, w.truncated = s[:left], true
	}
	w.data = append(w.data, s...)
}

func (w *shapeWriter) value(v any, depth int) {
	if w.truncated {
		return
	}
	if depth >= maxShapeDepth {
		w.truncated = true
		return
	}
	switch v := v.(type) {
	case map[string]any:
		w.object(v, depth)
	case []any:
		w.array(v, depth)
	default:
		w.text(fmt.Sprintf("%T", v))
	}
}

func (w *shapeWriter) object(v map[string]any, depth int) {
	w.text("{")
	for i, k := range slices.Sorted(maps.Keys(v)) {
		if w.truncated {
			break
		}
		if i > 0 {
			w.text(" ")
		}
		w.text(k)
		if s, ok := v[k].(string); ok && (k == "type" || k == "role" || k == "execution") {
			w.text("=")
			w.text(s)
			continue
		}
		w.text(":")
		w.value(v[k], depth+1)
	}
	w.text("}")
}

func (w *shapeWriter) array(v []any, depth int) {
	w.text("[")
	seen := map[string]bool{}
	for _, item := range v {
		start := len(w.data)
		if len(seen) > 0 {
			w.text(" ")
		}
		itemStart := len(w.data)
		w.value(item, depth+1)
		if w.truncated {
			break
		}
		s := string(w.data[itemStart:])
		if seen[s] {
			w.data = w.data[:start]
			continue
		}
		seen[s] = true
	}
	w.text("]")
}
