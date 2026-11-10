package main

import (
	"fmt"
	"maps"
	"strings"
)

// Settings that cut model turns. Each one is on unless the config sets it to
// false, so qh eval can measure it on and off.
type StepsCfg struct {
	BatchHint   *bool `json:"batch_hint"`   // prompt and tool words: send independent calls in one answer
	EditSnippet *bool `json:"edit_snippet"` // edit returns the changed lines, numbered, so no read is needed to see them
	GrepContext *bool `json:"grep_context"` // grep with few matches returns lines around each one
}

func onUnlessFalse(b *bool) bool { return b == nil || *b }

func (c StepsCfg) batch() bool   { return onUnlessFalse(c.BatchHint) }
func (c StepsCfg) snippet() bool { return onUnlessFalse(c.EditSnippet) }
func (c StepsCfg) grepCtx() bool { return onUnlessFalse(c.GrepContext) }

// setAll turns every setting on or off.
func (c *StepsCfg) setAll(on bool) {
	c.BatchHint, c.EditSnippet, c.GrepContext = &on, &on, &on
}

const batchLine = "\nSend independent tool calls together in one answer: read all the files you need at once, and make all the edits you already know in one answer. qh runs them in order. One call per answer only when a call needs the result of the one before."

// batchSchemas adds a few words about several calls in one answer to the
// descriptions of read and edit.
func batchSchemas(s []map[string]any) []map[string]any {
	add := map[string]string{
		"read": " To read several files, call read for each in one answer.",
		"edit": " Several edits, also to one file, can go in one answer; each sees the file as the one before left it.",
	}
	out := make([]map[string]any, len(s))
	for i, t := range s {
		f := t["function"].(map[string]any)
		extra, ok := add[f["name"].(string)]
		if !ok {
			out[i] = t
			continue
		}
		nf := maps.Clone(f)
		nf["description"] = f["description"].(string) + extra
		out[i] = map[string]any{"type": t["type"], "function": nf}
	}
	return out
}

const (
	snippetAround = 2  // lines shown before and after the new text
	snippetMax    = 24 // lines of new text shown at most
)

// editSnippet returns the lines of src around the new text that starts at
// line (1-based) and has nl lines, numbered as read numbers them.
func editSnippet(src string, line, nl int) string {
	lines := strings.Split(strings.TrimSuffix(src, "\n"), "\n")
	from := max(1, line-snippetAround)
	to := min(len(lines), line+max(nl, 1)-1+snippetAround)
	cut := 0
	if to-from+1 > snippetMax+2*snippetAround {
		cut = to - (from + snippetMax + 2*snippetAround - 1)
		to = from + snippetMax + 2*snippetAround - 1
	}
	var b strings.Builder
	b.WriteString("\nthe file now:\n")
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i, lines[i-1])
	}
	if cut > 0 {
		fmt.Fprintf(&b, "[%d more lines]\n", cut)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

const (
	grepCtxMax   = 5 // grep shows context only up to this many matches
	grepCtxLines = 3 // lines before and after each match
)

type grepHit struct {
	rel   string
	lines [][]byte
	at    int // 0-based
}

// grepWithContext prints the hits like grep -n -C: matches as file:N:text,
// the lines around them as file-N-text, a "--" between separate runs.
func grepWithContext(hits []grepHit) string {
	var b strings.Builder
	for i := 0; i < len(hits); {
		h := hits[i]
		from, to := max(0, h.at-grepCtxLines), min(len(h.lines)-1, h.at+grepCtxLines)
		match := map[int]bool{h.at: true}
		j := i + 1
		for ; j < len(hits) && hits[j].rel == h.rel && hits[j].at-grepCtxLines <= to+1; j++ {
			match[hits[j].at] = true
			to = min(len(h.lines)-1, hits[j].at+grepCtxLines)
		}
		if i > 0 {
			b.WriteString("--\n")
		}
		for k := from; k <= to; k++ {
			sep := "-"
			if match[k] {
				sep = ":"
			}
			txt := strings.TrimSuffix(string(h.lines[k]), "\r")
			if len(txt) > 300 {
				txt = txt[:300] + "..."
			}
			fmt.Fprintf(&b, "%s%s%d%s%s\n", h.rel, sep, k+1, sep, txt)
		}
		i = j
	}
	return b.String()
}
