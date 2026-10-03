package fakegh

import (
	"encoding/json"
	"fmt"
	"strings"
)

// evalJQ evaluates the small jq subset saddle passes to gh: paths (.a.b),
// iteration (.[]), pipes, array construction ([...]) and object shorthand
// ({a, b}). It returns one value per output.
func evalJQ(expr string, v any) ([]any, error) {
	// Round-trip so v holds only JSON types.
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var in any
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	return jq(strings.TrimSpace(expr), in)
}

func jq(expr string, in any) ([]any, error) {
	if parts := splitTop(expr, '|'); len(parts) > 1 {
		vals := []any{in}
		for _, p := range parts {
			var next []any
			for _, v := range vals {
				out, err := jq(strings.TrimSpace(p), v)
				if err != nil {
					return nil, err
				}
				next = append(next, out...)
			}
			vals = next
		}
		return vals, nil
	}
	switch {
	case strings.HasPrefix(expr, "[") && strings.HasSuffix(expr, "]") && !strings.HasPrefix(expr, "[]"):
		out, err := jq(strings.TrimSpace(expr[1:len(expr)-1]), in)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = []any{}
		}
		return []any{out}, nil
	case strings.HasPrefix(expr, "{") && strings.HasSuffix(expr, "}"):
		m, ok := in.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("jq: cannot index %T", in)
		}
		obj := map[string]any{}
		for _, k := range strings.Split(expr[1:len(expr)-1], ",") {
			k = strings.TrimSpace(k)
			obj[k] = m[k]
		}
		return []any{obj}, nil
	case strings.HasPrefix(expr, "."):
		return path(expr[1:], in)
	}
	return nil, fmt.Errorf("fakegh: unsupported jq expression %q", expr)
}

// path walks ".a.b[]"-style segments (without the leading dot).
func path(p string, in any) ([]any, error) {
	if p == "" {
		return []any{in}, nil
	}
	if strings.HasPrefix(p, "[]") {
		rest := strings.TrimPrefix(strings.TrimPrefix(p, "[]"), ".")
		var out []any
		switch x := in.(type) {
		case []any:
			for _, e := range x {
				r, err := path(rest, e)
				if err != nil {
					return nil, err
				}
				out = append(out, r...)
			}
		case map[string]any:
			for _, e := range x {
				r, err := path(rest, e)
				if err != nil {
					return nil, err
				}
				out = append(out, r...)
			}
		default:
			return nil, fmt.Errorf("jq: cannot iterate over %T", in)
		}
		return out, nil
	}
	end := strings.IndexAny(p, ".[")
	key, rest := p, ""
	if end >= 0 {
		key, rest = p[:end], strings.TrimPrefix(p[end:], ".")
	}
	if in == nil {
		return path(rest, nil)
	}
	m, ok := in.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("jq: cannot index %T with %q", in, key)
	}
	return path(rest, m[key])
}

// splitTop splits s on sep outside brackets and braces.
func splitTop(s string, sep byte) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[', '{', '(':
			depth++
		case ']', '}', ')':
			depth--
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}
