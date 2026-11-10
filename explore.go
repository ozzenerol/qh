package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The explore system prompt has no context files and no time-varying text,
// so the server caches its prefix across explore calls of a folder.
const exploreSystem = `You answer one question about the code in %s. Your tools are read, grep, glob and ls. You cannot change files.

Search with grep and glob, then read only the parts that matter.
Answer in at most 40 lines: the facts, each with its file:line. Quote short code where it helps.
Say what you could not find. Do not suggest changes unless the question asks for them.`

const exploreLine = "\nFor a question that needs more than 3 searches (where is X configured, what calls Y), use explore: it searches in a separate context and returns only the answer. For a file you know, just read it."

const exploreMaxLines = 60

func (a *Agent) exploreOn() bool { return a.Cfg.Explore == nil || *a.Cfg.Explore }

// schemas is the tool list of a run: the normal tools plus explore when it is on.
func (a *Agent) schemas(readOnly bool) []map[string]any {
	s := toolSchemas(readOnly)
	if !readOnly && a.exploreOn() {
		s = append(s, map[string]any{"type": "function", "function": map[string]any{
			"name":        "explore",
			"description": "Ask a read-only helper a question about the code. It searches in its own context and returns only the answer with file:line references.",
			"parameters": map[string]any{"type": "object", "required": []string{"question"}, "properties": map[string]any{
				"question": map[string]any{"type": "string", "description": "one concrete question"}}},
		}})
	}
	if !readOnly && a.Cfg.batch() {
		s = batchSchemas(s)
	}
	return s
}

// systemExtra is the text that the run adds to the session's system prompt.
func (a *Agent) systemExtra(readOnly bool) string {
	x := ""
	if !readOnly && a.exploreOn() {
		x += exploreLine
	}
	if !readOnly && a.Cfg.batch() {
		x += batchLine
	}
	if !readOnly {
		x += reportLine(a.Cfg.reportLines())
	}
	return x
}

func exploreSchemas() []map[string]any {
	s := toolSchemas(true)
	for _, t := range toolSchemas(false) {
		if t["function"].(map[string]any)["name"] == "glob" {
			s = append(s, t)
		}
	}
	return s
}

// explore runs a read-only sub-run in a fresh context and returns its answer.
// Its requests go through the same gate as the run. Its tool calls are shown
// as sub events; its steps and tokens go to the Explore fields of st.
func (a *Agent) explore(ctx context.Context, cwd, rawArgs string, st *Stats, emit func(Event), told *bool) (string, bool) {
	var args struct{ Question, Query, Prompt string }
	json.Unmarshal([]byte(rawArgs), &args)
	q := strings.TrimSpace(args.Question + args.Query + args.Prompt)
	if q == "" {
		return `error: explore needs the "question" argument`, false
	}
	limit := a.Cfg.ExploreSteps
	if limit <= 0 {
		limit = 15
	}
	st.Explores++
	tools := &Tools{Cwd: cwd, ReadOnly: true, GrepCtx: a.Cfg.grepCtx()} // its own Seen: what the helper read, the run has not seen
	tools.Confine, tools.ReadRoots = a.Cfg.confine()
	schemas := exploreSchemas()
	msgs := []Message{{Role: "system", Content: fmt.Sprintf(exploreSystem, cwd)}, {Role: "user", Content: q}}
	answer := ""
	for step := 1; ; step++ {
		last := step > limit
		if last {
			msgs = append(msgs, Message{Role: "user", Content: "Stop searching. Give the answer now from what you found, as text."})
		}
		think := !last
		var msg Message
		var u Usage
		var err error
		for attempt := 0; ; attempt++ {
			var reasoning strings.Builder
			msg, u, err = a.chat(ctx, Request{Messages: msgs, Tools: schemas, Think: think, MaxTok: a.Cfg.MaxTokens}, func(d Delta) bool {
				reasoning.WriteString(d.Reasoning)
				return reasoning.Len() < a.Cfg.ThinkBudget && !looping(reasoning.String())
			}, emit, told)
			if errors.Is(err, errStopped) && attempt == 0 {
				think = false
				continue
			}
			break
		}
		if errors.Is(err, errStopped) {
			err = nil
		}
		if err != nil {
			return "error: explore stopped: " + err.Error(), false
		}
		st.ExploreSteps++
		st.ExploreOut += u.CompletionTokens
		st.ExploreFresh += u.PromptTokens - u.Details.CachedTokens
		if len(msg.ToolCalls) == 0 || last {
			answer = strings.TrimSpace(msg.Content)
			break
		}
		msgs = append(msgs, msg)
		for _, tc := range msg.ToolCalls {
			emit(Event{T: "tool", Sub: true, Tool: tc.Function.Name, Text: argSummary(tc.Function.Name, tc.Function.Arguments)})
			out, ok := tools.Run(ctx, tc.Function.Name, tc.Function.Arguments)
			sum := firstLine(out)
			if !ok {
				sum = failLine(out)
			}
			emit(Event{T: "result", Sub: true, Tool: tc.Function.Name, OK: ok, Text: sum})
			msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: out})
			if ctx.Err() != nil {
				return "error: explore was interrupted", false
			}
		}
	}
	if answer == "" {
		return "error: the explore helper gave no answer; search yourself", false
	}
	if l := strings.Split(answer, "\n"); len(l) > exploreMaxLines {
		answer = strings.Join(l[:exploreMaxLines], "\n") + fmt.Sprintf("\n[qh: answer cut at %d lines]", exploreMaxLines)
	}
	return answer, true
}

const exploreNudgeText = "\n\n[qh: for a search like this, explore keeps the results out of your context: explore {question: ...}]"

var bashSearch = regexp.MustCompile(`^\s*(cd\s+\S+\s*&&\s*)?(grep|rg|find|ag)\b`)

// exploreNudge counts search calls in a row. After 5 with no edit between
// them, once the context passes 30k tokens, it suggests explore, once a run.
type exploreNudge struct {
	run  int
	done bool
}

func (n *exploreNudge) after(name, rawArgs string, ctxTokens int) string {
	search := name == "grep" || name == "glob" || name == "read" || name == "ls"
	if name == "bash" {
		var a struct{ Command string }
		json.Unmarshal([]byte(rawArgs), &a)
		search = bashSearch.MatchString(a.Command)
	}
	if !search {
		n.run = 0
		return ""
	}
	n.run++
	if n.done || n.run < 5 || ctxTokens <= 30000 {
		return ""
	}
	n.done = true
	return exploreNudgeText
}
