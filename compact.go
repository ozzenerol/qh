package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const summaryHead = "[Summary of the conversation so far. The older messages were replaced by it.]\n\n"

const compactAsk = `Stop the task for a moment. Write a summary of this whole conversation for yourself, so you can continue the work from the summary alone. The older messages will be deleted.

Write these sections:
1. Goal: what the user wants, in their words where it matters.
2. Done: the steps that are finished, with the files you changed and why.
3. Facts: exact file paths, function names, commands, error messages and test results that you still need.
4. Decisions: what was decided and what the user rejected.
5. Now: the state at this moment and the next steps.

Do not call tools. Write only the summary, 800 words or fewer.`

// compactAt is the prompt size that starts an automatic compaction.
func (a *Agent) compactAt() int {
	if a.Cfg.CompactAt > 0 {
		return a.Cfg.CompactAt
	}
	if a.Cfg.Context <= 0 {
		return int(^uint(0) >> 1) // no window known: never compact by itself
	}
	return a.Cfg.Context * 8 / 10
}

// Compact replaces the messages of s with a summary that the model writes.
// The request is the same prompt as a normal step plus one question, so the
// server answers it from its prompt cache.
func (a *Agent) Compact(ctx context.Context, s *Session, focus string, emit func(Event)) error {
	if len(s.Msgs) < 2 {
		return errors.New("there is nothing to compact yet")
	}
	before := s.LastPrompt
	if before == 0 {
		before = estimateTokens(s.Msgs)
	}
	emit(Event{T: "compacting", Before: before})
	ask := compactAsk
	if strings.TrimSpace(focus) != "" {
		ask += "\n\nGive most room to this: " + strings.TrimSpace(focus)
	}
	msgs := append([]Message{{Role: "system", Content: s.systemPrompt(a.systemExtra(s.ReadOnly))}}, s.Msgs...)
	msgs = append(msgs, Message{Role: "user", Content: ask})
	// The tools stay in the request: they are part of the cached prefix.
	var summary string
	told := false
	for try := 0; try < 2 && summary == ""; try++ {
		msg, _, err := a.chat(ctx, Request{Messages: msgs, Tools: a.schemas(s.ReadOnly), Think: false, MaxTok: 6000},
			func(Delta) bool { return true }, emit, &told)
		if err != nil {
			return err
		}
		summary = strings.TrimSpace(msg.Content)
		if len(msg.ToolCalls) > 0 {
			// It called a tool anyway: answer the calls and ask once more.
			summary = ""
			msgs = append(msgs, msg)
			for _, tc := range msg.ToolCalls {
				msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name,
					Content: "error: tools are off while you write the summary. Write the summary now, as text."})
			}
		}
	}
	if summary == "" {
		return errors.New("the model wrote an empty summary; the conversation is unchanged")
	}
	archive(s)
	s.Msgs = []Message{
		{Role: "user", Content: summaryHead + summary, From: "compact"},
		{Role: "assistant", Content: "I have the summary and will continue from it.", From: "compact"},
	}
	s.LastPrompt = 0
	s.System = "" // the cache is gone, so the next request may pick up new AGENTS.md and notes
	after := estimateTokens(append([]Message{{Content: s.systemPrompt(a.systemExtra(s.ReadOnly))}}, s.Msgs...))
	emit(Event{T: "compacted", Text: summary, Before: before, After: after})
	return nil
}

// estimateTokens guesses the size of the messages (about 3.5 characters a token).
func estimateTokens(ms []Message) int {
	n := 0
	for _, m := range ms {
		n += len(m.Content) + len(m.ReasoningContent)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments) + 20
		}
	}
	return n * 2 / 7
}

// archive keeps the old messages in ~/.qh/sessions/archive before they go.
func archive(s *Session) {
	d, err := qhDir()
	if err != nil || s.Name == "" {
		return
	}
	dir := filepath.Join(d, "sessions", "archive")
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	b, _ := json.Marshal(s)
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%s.json", s.Name, time.Now().Format("20060102-150405"))), b, 0o600)
}
