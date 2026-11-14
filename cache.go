package main

import (
	"strconv"
	"strings"
)

// systemPrompt returns the system prompt of the session. mlx_lm.server reuses
// its cache only for a request that starts with the same tokens, so the
// prompt is built once and kept for the whole session, even when AGENTS.md,
// the notes or the source folders change during it. It is built again for an
// empty session, after compaction, and when the folder or read-only mode
// changes, since the cache is lost then anyway.
// extra goes at the end of the base prompt, before the context files.
func (s *Session) systemPrompt(extra string) string {
	if s.Profile != "" {
		if t, err := loadProfile(s.Profile); err == nil {
			extra += "\n\n" + t
		}
	}
	key := s.Cwd + "\x00" + strconv.FormatBool(s.ReadOnly) + "\x00" + extra + "\x00" + promptVersion(s.ReadOnly)
	if s.System == "" || s.SysKey != key || len(s.Msgs) == 0 {
		p := buildSystem(s.Cwd, s.ReadOnly)
		if i := strings.Index(p, "\n\n# "); i >= 0 {
			p = p[:i] + extra + p[i:]
		} else {
			p += extra
		}
		s.System, s.SysKey = p, key
	}
	return s.System
}

// promptVersion changes when the prompt templates or the tool list do, so a
// session saved by another qh build gets the prompt that names its tools.
func promptVersion(readOnly bool) string {
	var b strings.Builder
	b.WriteString(systemPrompt + systemPromptRO)
	for _, t := range toolSchemas(readOnly) {
		b.WriteString("\x00" + t["function"].(map[string]any)["name"].(string))
	}
	return strconv.FormatUint(contentHash([]byte(b.String())), 36)
}

// answerOpen gives every tool call of the last assistant message that has no
// result an "interrupted" one. A run stopped in the middle of its calls
// leaves them open, and the next request needs a result for each call. The
// results are appended, so the request still extends the cached one.
func (s *Session) answerOpen() {
	for i := len(s.Msgs) - 1; i >= 0; i-- {
		m := s.Msgs[i]
		if m.Role == "tool" {
			continue
		}
		if m.Role != "assistant" {
			return
		}
		done := map[string]bool{}
		for _, r := range s.Msgs[i+1:] {
			done[r.ToolCallID] = true
		}
		for _, tc := range m.ToolCalls {
			if !done[tc.ID] {
				s.Msgs = append(s.Msgs, Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name,
					Content: "error: the run was interrupted before this call ran"})
			}
		}
		return
	}
}
