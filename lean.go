package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Output tokens are almost all of a run's time (about 60 tok/s on the Mac,
// prefill and tools are small). outSplit says where they go, and the three
// options below cut the biggest parts.

// outSplit adds the characters of one answer to m by kind: think, text,
// and the arguments of each tool call by tool (edit counts old and new
// apart, since a long old is a cost of its own).
func outSplit(m map[string]int, msg Message, think int) {
	m["think"] += think
	m["text"] += len(msg.Content)
	for _, tc := range msg.ToolCalls {
		name := tc.Function.Name
		var args map[string]any
		if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil || name != "edit" {
			m[name] += len(tc.Function.Arguments)
			continue
		}
		rest := len(tc.Function.Arguments)
		for _, k := range []string{"old", "new"} {
			if s, ok := args[k].(string); ok {
				m["edit."+k] += len(s)
				rest -= len(s)
			}
		}
		m["edit"] += max(0, rest)
	}
}

// splitLine is "think 41%, write 25%, text 9%, ...": every kind over 2% of
// the characters, biggest first.
func splitLine(m map[string]int) string {
	all := 0
	for _, v := range m {
		all += v
	}
	if all == 0 {
		return ""
	}
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		if v*50 >= all {
			l = append(l, kv{k, v})
		}
	}
	sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v || l[i].v == l[j].v && l[i].k < l[j].k })
	var out []string
	for _, x := range l {
		out = append(out, fmt.Sprintf("%s %d%%", x.k, x.v*100/all))
	}
	return strings.Join(out, ", ")
}

// Config options; the defaults cut output, and these values turn each off:
//
//	"write_note": false     no note when write rewrites a big file to change a few lines
//	"report_lines": 0       no line limit for the final report in the system prompt
//	"edit_think": true      think on the step after a clean edit/write/todo step
func (c Config) writeNote() bool { return c.WriteNote == nil || *c.WriteNote }

func (c Config) reportLines() int {
	if c.ReportLines == nil {
		return 8
	}
	return *c.ReportLines
}

func (c Config) editThink() bool { return c.EditThink != nil && *c.EditThink }

const (
	rewriteMin   = 60 // lines in the old file before a rewrite gets the note
	rewriteShare = 3  // and the changed lines are under 1/rewriteShare of it
)

// rewriteNote is the note for a write that replaced an existing file of
// old lines and changed add+del of them, or "" when write was the right tool.
func rewriteNote(old, add, del int) string {
	if old < rewriteMin || (add+del)*rewriteShare >= old {
		return ""
	}
	return fmt.Sprintf("\n[qh: you wrote all %d lines to change %d. Use edit for a change like this; write sends the whole file again, and that is the slowest part of a step.]", old, add+del)
}

// reportLine is the system prompt line that caps the final report.
func reportLine(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("\nKeep the final report under %d lines. Do not repeat the code or the test output in it.", n)
}

// quietStep reports if every call of a step was a write, edit or todo that
// succeeded with nothing for the model to look at (no qh note, no compiler
// errors). The next step is then usually a build or the next edit, and
// thinking there adds little.
func quietStep(calls []ToolCall, ok []bool, out []string) bool {
	if len(calls) == 0 {
		return false
	}
	for i, tc := range calls {
		switch tc.Function.Name {
		case "write", "edit", "todo":
		default:
			return false
		}
		if !ok[i] || strings.Contains(out[i], "[qh") {
			return false
		}
	}
	return true
}

// applySet merges "key=value,key=value" into c as JSON fields, so qh eval
// can turn options on and off: -set write_note=false,report_lines=0.
func applySet(c *Config, set string) error {
	if strings.TrimSpace(set) == "" {
		return nil
	}
	m := map[string]json.RawMessage{}
	for _, kv := range strings.Split(set, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || k == "" {
			return fmt.Errorf("-set: want key=value, got %q", kv)
		}
		if !json.Valid([]byte(v)) {
			b, _ := json.Marshal(v)
			v = string(b)
		}
		m[k] = json.RawMessage(v)
	}
	b, _ := json.Marshal(m)
	return json.Unmarshal(b, c)
}
