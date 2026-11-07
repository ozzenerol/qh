package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Session struct {
	Name     string    `json:"name"`
	Cwd      string    `json:"cwd"`
	Think    bool      `json:"think"`
	ReadOnly bool      `json:"read_only"`
	Msgs     []Message `json:"messages"`
	mu       sync.Mutex
}

type Stats struct {
	Model    string  `json:"model"`
	Steps    int     `json:"steps"`
	Prompt   int     `json:"prompt_tokens"`
	Cached   int     `json:"cached_tokens"`
	Output   int     `json:"output_tokens"`
	Thinking int     `json:"thinking_chars"`
	Secs     float64 `json:"secs"`
	Retries  int     `json:"retries"`
}

type Event struct {
	T     string `json:"t"` // thinking, text, tool, result, info, error, done, list
	Text  string `json:"text,omitempty"`
	Tool  string `json:"tool,omitempty"`
	OK    bool   `json:"ok,omitempty"`
	Stats *Stats `json:"stats,omitempty"`
}

type Agent struct {
	LLM *LLM
	Cfg Config
}

const systemPrompt = `You are a coding agent. You work in %s on %s. Your tools are read, write, edit and bash.

Work in small steps. Read the code before you change it. After each change, build or run the tests and read the result.
Use bash for ls, grep, find, git, builds and tests. Filter long output with grep, head or tail.
Use edit for small changes and write for new files. In edit, copy old exactly from the file and leave out the line numbers.
Do not ask for permission. Do the task.
When you finish, reply with a short report: the files you changed, and the result of the last build or test. If you could not finish, say what is left.`

const systemPromptRO = `You are a coding agent in a read-only run. You work in %s on %s. Your tools are read, grep and ls. You cannot change files or run commands.

Find the facts with grep and ls, then read the parts that matter. Quote file paths and line numbers for each claim.
Do not ask for permission. Do the task.
When you finish, reply with the answer, then list the files you read.`

func buildSystem(cwd string, readOnly bool) string {
	tmpl := systemPrompt
	if readOnly {
		tmpl = systemPromptRO
	}
	s := fmt.Sprintf(tmpl, cwd, runtime.GOOS)
	for _, f := range contextFiles(cwd) {
		b, err := os.ReadFile(f)
		if err != nil || len(strings.TrimSpace(string(b))) == 0 {
			continue
		}
		s += "\n\n# " + f + "\n\n" + strings.TrimSpace(string(b))
	}
	return s
}

// contextFiles returns ~/.qh/AGENTS.md, then AGENTS.md (or CLAUDE.md) from
// the top directory down to cwd.
func contextFiles(cwd string) []string {
	var out []string
	if d, err := qhDir(); err == nil {
		if p := filepath.Join(d, "AGENTS.md"); exists(p) {
			out = append(out, p)
		}
	}
	var dirs []string
	for d := cwd; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == filepath.Dir(d) {
			break
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		for _, n := range []string{"AGENTS.md", "CLAUDE.md"} {
			if p := filepath.Join(dirs[i], n); exists(p) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// looping reports a thinking or text loop: the last 160 characters already
// appear at least twice in the recent text.
func looping(s string) bool {
	const tail, window = 160, 8000
	if len(s) < tail*4 {
		return false
	}
	end := s[len(s)-tail:]
	if strings.TrimSpace(end) == "" {
		return false
	}
	start := max(0, len(s)-window)
	return strings.Count(s[start:len(s)-tail], end) >= 2
}

func (a *Agent) Run(ctx context.Context, s *Session, prompt string, emit func(Event)) Stats {
	t0 := time.Now()
	st := Stats{Model: filepath.Base(a.LLM.Model)}
	tools := &Tools{Cwd: s.Cwd, ReadOnly: s.ReadOnly}
	schemas := toolSchemas(s.ReadOnly)
	system := Message{Role: "system", Content: buildSystem(s.Cwd, s.ReadOnly)}
	s.Msgs = append(s.Msgs, Message{Role: "user", Content: prompt})
	seen := map[string]int{}

	for st.Steps < a.Cfg.MaxSteps {
		st.Steps++
		think := s.Think
		var msg Message
		var u Usage
		var err error
		for attempt := 0; ; attempt++ {
			var reasoning, text strings.Builder
			lastCheck := 0
			msg, u, err = a.LLM.Chat(ctx, Request{
				Messages: append([]Message{system}, s.Msgs...),
				Tools:    schemas, Think: think, MaxTok: a.Cfg.MaxTokens,
			}, func(d Delta) bool {
				if d.Reasoning != "" {
					reasoning.WriteString(d.Reasoning)
					emit(Event{T: "thinking", Text: d.Reasoning})
				}
				if d.Text != "" {
					text.WriteString(d.Text)
					emit(Event{T: "text", Text: d.Text})
				}
				n := reasoning.Len() + text.Len()
				if n-lastCheck < 400 {
					return true
				}
				lastCheck = n
				return reasoning.Len() < a.Cfg.ThinkBudget && !looping(reasoning.String()) && !looping(text.String())
			})
			st.Thinking += reasoning.Len()
			if errors.Is(err, errStopped) && attempt == 0 {
				// Ask again with thinking off. The prompt is the same, so the
				// server answers it from its cache.
				st.Retries++
				think = false
				emit(Event{T: "info", Text: fmt.Sprintf("thinking stopped after %d chars (loop or budget); asking again without thinking", reasoning.Len())})
				continue
			}
			break
		}
		if errors.Is(err, errStopped) {
			emit(Event{T: "info", Text: "the answer repeats itself; keeping what came so far"})
			err = nil
		}
		if err != nil {
			emit(Event{T: "error", Text: err.Error()})
			break
		}
		st.Prompt, st.Cached = u.PromptTokens, u.Details.CachedTokens
		st.Output += u.CompletionTokens
		s.Msgs = append(s.Msgs, msg)
		if len(msg.ToolCalls) == 0 {
			break
		}
		for _, tc := range msg.ToolCalls {
			key := tc.Function.Name + "\x00" + normArgs(tc.Function.Arguments)
			seen[key]++
			emit(Event{T: "tool", Tool: tc.Function.Name, Text: argSummary(tc.Function.Name, tc.Function.Arguments)})
			out, ok := tools.Run(ctx, tc.Function.Name, tc.Function.Arguments)
			if n := seen[key]; n >= 3 {
				out = fmt.Sprintf("[you made this exact call %d times; the result will not change. Try a different step.]\n", n) + out
			}
			emit(Event{T: "result", Tool: tc.Function.Name, OK: ok, Text: firstLine(out)})
			s.Msgs = append(s.Msgs, Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: out})
			if ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil {
			emit(Event{T: "error", Text: "cancelled"})
			break
		}
	}
	if st.Steps >= a.Cfg.MaxSteps {
		emit(Event{T: "info", Text: fmt.Sprintf("stopped at the step limit (%d)", a.Cfg.MaxSteps)})
	}
	st.Secs = time.Since(t0).Seconds()
	return st
}

func normArgs(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return raw
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func argSummary(name, raw string) string {
	args := map[string]any{}
	json.Unmarshal([]byte(raw), &args)
	str := func(k string) string { v, _ := args[k].(string); return v }
	switch name {
	case "bash":
		return oneLine(str("command"), 160)
	case "read":
		s := str("path")
		if o, ok := args["offset"]; ok {
			s += fmt.Sprintf(" @%v", o)
		}
		return s
	case "edit", "write", "ls":
		return str("path")
	case "grep":
		s := str("pattern")
		if p := str("path"); p != "" {
			s += " in " + p
		}
		if g := str("glob"); g != "" {
			s += " (" + g + ")"
		}
		return s
	}
	return oneLine(raw, 160)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

func firstLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	last := lines[len(lines)-1]
	if strings.HasPrefix(last, "[exit ") {
		if len(lines) > 1 {
			return oneLine(lines[0], 100) + " " + last
		}
		return last
	}
	return oneLine(lines[0], 120)
}
