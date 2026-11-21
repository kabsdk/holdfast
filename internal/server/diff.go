package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Change is one difference between two revisions, located by a path such as
// hosts.web01.example.com.jobs.system.paths[1].
type Change struct {
	Path string `json:"path"`
	From any    `json:"from,omitempty"`
	To   any    `json:"to,omitempty"`
}

// Diff compares any two revisions (spec §3.2).
func (s *Server) Diff(ctx context.Context, a, b int64) ([]Change, error) {
	q := s.st.Q()
	ra, err := q.GetRevision(ctx, a)
	if err != nil {
		return nil, err
	}
	rb, err := q.GetRevision(ctx, b)
	if err != nil {
		return nil, err
	}
	fa, fb := map[string]any{}, map[string]any{}
	flatten("", toAny(ra.Config), fa)
	flatten("", toAny(rb.Config), fb)
	keys := map[string]bool{}
	for k := range fa {
		keys[k] = true
	}
	for k := range fb {
		keys[k] = true
	}
	var out []Change
	for k := range keys {
		va, inA := fa[k]
		vb, inB := fb[k]
		if inA && inB && fmt.Sprint(va) == fmt.Sprint(vb) {
			continue
		}
		c := Change{Path: k}
		if inA {
			c.From = va
		}
		if inB {
			c.To = vb
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if out == nil {
		out = []Change{}
	}
	return out, nil
}

func toAny(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// flatten turns a JSON value into path → leaf value. Empty objects and arrays are leaves, so adding
// an empty host still shows up.
func flatten(prefix string, v any, out map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 && prefix != "" {
			out[prefix] = "{}"
		}
		for k, x := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, x, out)
		}
	case []any:
		if len(t) == 0 {
			out[prefix] = "[]"
		}
		for i, x := range t {
			flatten(fmt.Sprintf("%s[%d]", prefix, i), x, out)
		}
	default:
		out[prefix] = t
	}
}
