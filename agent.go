package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type Session struct {
	Name       string     `json:"name"`
	Cwd        string     `json:"cwd"`
	Think      bool       `json:"think"`
	ReadOnly   bool       `json:"read_only"`
	LastPrompt int        `json:"last_prompt"` // tokens of the last request
	Msgs       []Message  `json:"messages"`
	mu         sync.Mutex // held for the whole run

	wmu      sync.Mutex // guards watchers and cancel
	watchers map[chan Event]struct{}
	cancel   context.CancelFunc
	running  bool   // a run holds mu
	step     int    // its step
	waiting  bool   // the run waits for the model (another session uses it)
	from     string // who started the run
}

func (s *Session) setFrom(f string) {
	s.wmu.Lock()
	s.from = f
	s.wmu.Unlock()
}

func (s *Session) fromNow() string {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.from
}

func (s *Session) setWaiting(w bool) {
	s.wmu.Lock()
	s.waiting = w
	s.wmu.Unlock()
}

// waitingNow reports if the run waits for the model.
func (s *Session) waitingNow() bool {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.waiting
}

// state reports if a run is going on and at which step.
func (s *Session) state() (bool, int) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.running, s.step
}

func (s *Session) setState(running bool, step int) {
	s.wmu.Lock()
	s.running, s.step = running, step
	if !running {
		s.waiting = false
	}
	s.wmu.Unlock()
}

func (s *Session) broadcast(e Event) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	for ch := range s.watchers {
		select {
		case ch <- e:
		default: // a watcher that does not read loses events; the run goes on
		}
	}
}

func (s *Session) setCancel(c context.CancelFunc) {
	s.wmu.Lock()
	s.cancel = c
	s.wmu.Unlock()
}

func (s *Session) cancelRun() bool {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.cancel == nil {
		return false
	}
	s.cancel()
	return true
}

type Stats struct {
	Model    string   `json:"model"`
	Steps    int      `json:"steps"`
	Prompt   int      `json:"prompt_tokens"`
	Cached   int      `json:"cached_tokens"`
	Output   int      `json:"output_tokens"`
	Thinking int      `json:"thinking_chars"`
	Secs     float64  `json:"secs"`
	Retries  int      `json:"retries"`
	Window   int      `json:"window"`
	ToolErrs int      `json:"tool_errors"`
	Limit    bool     `json:"step_limit,omitempty"` // the run stopped at max_steps
	Checks   []string `json:"checks,omitempty"`     // results of the "Done when" commands
}

type Event struct {
	T      string `json:"t"` // user, queued, dropped, thinking, text, tool, result, info, error, done, attached
	From   string `json:"from,omitempty"`
	Replay bool   `json:"replay,omitempty"`
	Text   string `json:"text,omitempty"`
	Before int    `json:"before,omitempty"` // compaction: tokens before and after
	After  int    `json:"after,omitempty"`
	Tool   string `json:"tool,omitempty"`
	OK     bool   `json:"ok,omitempty"`
	Stats  *Stats `json:"stats,omitempty"`
}

type Agent struct {
	LLM  *LLM
	Cfg  Config
	Gate chan struct{} // one model request at a time across sessions; nil means no gate
	Save func()        // called after each step, so the session file stays current
	Wait func(bool)    // called when a request starts and stops waiting at the gate
}

// chat sends one request through the gate. The model server answers one
// request at a time, so a run of another session waits here and says so once.
func (a *Agent) chat(ctx context.Context, r Request, onDelta func(Delta) bool, emit func(Event), told *bool) (Message, Usage, error) {
	if a.Gate != nil {
		select {
		case a.Gate <- struct{}{}:
		default:
			if !*told {
				emit(Event{T: "info", Text: "waiting for the model: a run of another session is using it"})
				*told = true
			}
			if a.Wait != nil {
				a.Wait(true)
			}
			select {
			case a.Gate <- struct{}{}:
				if a.Wait != nil {
					a.Wait(false)
				}
			case <-ctx.Done():
				if a.Wait != nil {
					a.Wait(false)
				}
				return Message{}, Usage{}, ctx.Err()
			}
		}
		defer func() { <-a.Gate }()
	}
	return a.LLM.Chat(ctx, r, onDelta)
}

const selfCheckAsk = `Before you report, read the task again, mainly its "Done when" part and every numbered or bulleted requirement. For each one, say done or not done. Fix every item that is not done. Then give the final report.`

// doneWhen returns the "Done when" part of a task, or "".
func doneWhen(prompt string) string {
	lines := strings.Split(prompt, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(strings.ToLower(strings.TrimLeft(l, " #*-")), "done when") {
			continue
		}
		part := []string{l}
		for _, m := range lines[i+1:] {
			if strings.TrimSpace(m) == "" || strings.HasPrefix(strings.TrimSpace(m), "#") {
				break
			}
			part = append(part, m)
		}
		return strings.Join(part, "\n")
	}
	return ""
}

var checkTools = []string{"./", "go ", "make", "npm ", "pnpm ", "yarn ", "mvn ", "gradle ", "cargo ", "pytest", "python", "uv ", "node ", "bun ", "deno ", "swift ", "dotnet ", "bash ", "sh ", "ctest", "cmake "}

// checkCommands finds the commands in backticks in the "Done when" part.
func checkCommands(prompt string) []string {
	part := doneWhen(prompt)
	var out []string
	for {
		a := strings.Index(part, "`")
		if a < 0 {
			break
		}
		b := strings.Index(part[a+1:], "`")
		if b < 0 {
			break
		}
		cmd := strings.TrimSpace(part[a+1 : a+1+b])
		part = part[a+b+2:]
		for _, p := range checkTools {
			if strings.HasPrefix(cmd, p) {
				out = append(out, cmd)
				break
			}
		}
	}
	return out
}

const systemPrompt = `You are a coding agent. You work in %s on %s. Your tools are read, write, edit and bash.

Work in small steps. Read the code before you change it. After each change, build or run the tests and read the result.
Use bash for ls, grep, find, git, builds and tests. Filter long output with grep, head or tail.
To check a library API, print its signatures (javap -cp JAR CLASS, go doc PKG.NAME, python -c "help(...)") instead of unpacking its sources.
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

// contextFiles returns ~/.qh/AGENTS.md, ~/.qh/notes/REPO.md (REPO is the
// name of the git repo that holds cwd), then AGENTS.md (or CLAUDE.md) from the
// top directory down to cwd.
func contextFiles(cwd string) []string {
	var out []string
	if d, err := qhDir(); err == nil {
		if p := filepath.Join(d, "AGENTS.md"); exists(p) {
			out = append(out, p)
		}
		for r := cwd; ; r = filepath.Dir(r) {
			if _, err := os.Stat(filepath.Join(r, ".git")); err == nil {
				if p := filepath.Join(d, "notes", filepath.Base(r)+".md"); exists(p) {
					out = append(out, p)
				}
				break
			}
			if r == filepath.Dir(r) {
				break
			}
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

func (a *Agent) Run(ctx context.Context, s *Session, prompt, from string, emit func(Event)) Stats {
	t0 := time.Now()
	st := Stats{Model: filepath.Base(a.LLM.Model), Window: a.Cfg.Context}
	if s.LastPrompt > a.compactAt() {
		if err := a.Compact(ctx, s, "", emit); err != nil {
			emit(Event{T: "error", Text: "compaction failed: " + err.Error()})
		}
	}
	tools := &Tools{Cwd: s.Cwd, ReadOnly: s.ReadOnly}
	schemas := toolSchemas(s.ReadOnly)
	system := Message{Role: "system", Content: buildSystem(s.Cwd, s.ReadOnly)}
	s.Msgs = append(s.Msgs, Message{Role: "user", Content: prompt, From: from})
	seen := map[string]int{}
	told := false
	ended := false // the model gave its final answer
	selfChecked := doneWhen(prompt) == ""
	checks := checkCommands(prompt)
	checked := s.ReadOnly
	changed := map[string]bool{} // files that write and edit changed in this run
	fixTurn := false             // the model got one turn to fix failed checks
	wantClean := noWarnings.MatchString(prompt)
	defer s.setState(false, 0)

	for st.Steps < a.Cfg.MaxSteps {
		st.Steps++
		s.setState(true, st.Steps)
		think := s.Think
		var msg Message
		var u Usage
		var err error
		for attempt := 0; ; attempt++ {
			var reasoning, text strings.Builder
			lastCheck := 0
			msg, u, err = a.chat(ctx, Request{
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
			}, emit, &told)
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
			if ctx.Err() != nil {
				emit(Event{T: "info", Text: "interrupted"})
			} else {
				emit(Event{T: "error", Text: err.Error()})
			}
			break
		}
		st.Prompt, st.Cached = u.PromptTokens, u.Details.CachedTokens
		st.Output += u.CompletionTokens
		s.LastPrompt = u.PromptTokens + u.CompletionTokens
		s.Msgs = append(s.Msgs, msg)
		if a.Save != nil {
			a.Save()
		}
		if len(msg.ToolCalls) == 0 {
			if !selfChecked {
				selfChecked = true
				emit(Event{T: "info", Text: "self-check: asking the model to compare its work with the task"})
				s.Msgs = append(s.Msgs, Message{Role: "user", Content: selfCheckAsk, From: "qh"})
				continue
			}
			if !checked {
				// The checks run after the report, and once more after a fix
				// turn, so the check lines describe the code as it ends.
				failed := a.finalChecks(ctx, tools, checks, changed, wantClean, &st, emit)
				if failed != "" && !fixTurn {
					fixTurn = true
					s.Msgs = append(s.Msgs, Message{Role: "user", From: "qh", Content: failed})
					continue
				}
				checked = true
			}
			ended = true
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
			if !ok {
				st.ToolErrs++
			} else if tc.Function.Name == "write" || tc.Function.Name == "edit" {
				var pa struct{ Path, File_path string }
				json.Unmarshal([]byte(tc.Function.Arguments), &pa)
				if p := cmp.Or(pa.Path, pa.File_path); p != "" {
					changed[tools.path(p)] = true
				}
			}
			summary := firstLine(out)
			if !ok {
				summary = failLine(out)
			}
			emit(Event{T: "result", Tool: tc.Function.Name, OK: ok, Text: summary})
			s.Msgs = append(s.Msgs, Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: out})
			if ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil {
			emit(Event{T: "info", Text: "interrupted"})
			break
		}
		if left := a.Cfg.MaxSteps - st.Steps; left == 5 {
			last := &s.Msgs[len(s.Msgs)-1]
			last.Content += "\n\n[qh: 5 steps left before the step limit. Finish the work, run the check, and write the final report.]"
		}
		if s.LastPrompt > a.compactAt() {
			if err := a.Compact(ctx, s, "the task that is in progress now", emit); err != nil {
				emit(Event{T: "error", Text: "compaction failed: " + err.Error()})
				break
			}
			s.Msgs = append(s.Msgs, Message{Role: "user", From: "compact",
				Content: "Continue the task from the summary. Check the files before you change them again."})
		}
	}
	if !ended && st.Steps >= a.Cfg.MaxSteps && ctx.Err() == nil {
		st.Limit = true
		emit(Event{T: "info", Text: fmt.Sprintf("stopped at the step limit (%d); asking for the report", a.Cfg.MaxSteps)})
		a.limitReport(ctx, s, system, schemas, emit, &told)
		if !checked {
			a.finalChecks(ctx, tools, checks, changed, wantClean, &st, emit)
		}
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

// finalChecks runs the "Done when" commands and looks for unused imports in
// the changed files. It returns a message for the model when something
// should be fixed, or "".
func (a *Agent) finalChecks(ctx context.Context, tools *Tools, cmds []string, changed map[string]bool, wantClean bool, st *Stats, emit func(Event)) string {
	failed := ""
	if len(cmds) > 0 {
		failed = a.runChecks(ctx, tools, cmds, wantClean, st, emit)
	}
	var notes []string
	files := make([]string, 0, len(changed))
	for f := range changed {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, imp := range unusedImports(f, string(b)) {
			rel, _ := filepath.Rel(tools.Cwd, f)
			notes = append(notes, fmt.Sprintf("%s: import %s is not used", rel, imp))
		}
	}
	if len(notes) == 0 {
		return failed
	}
	emit(Event{T: "info", Text: fmt.Sprintf("%d unused imports in the changed files", len(notes))})
	for _, n := range notes {
		st.Checks = append(st.Checks, "qh note: "+n)
	}
	msg := "qh found imports that the changed files do not use:\n" + strings.Join(notes, "\n") +
		"\n\nRemove them, build again, then give the final report."
	if failed != "" {
		return failed + "\n\n" + msg
	}
	return msg
}

// Kotlin operator and delegate names: an import of these is used without
// its name appearing in the code.
var kotlinOperators = map[string]bool{"getValue": true, "setValue": true, "provideDelegate": true, "invoke": true,
	"get": true, "set": true, "contains": true, "plus": true, "minus": true, "times": true, "div": true, "rem": true,
	"rangeTo": true, "rangeUntil": true, "compareTo": true, "iterator": true, "next": true, "hasNext": true,
	"unaryMinus": true, "unaryPlus": true, "not": true, "inc": true, "dec": true, "plusAssign": true, "minusAssign": true,
	"component1": true, "component2": true, "component3": true, "component4": true, "component5": true}

var importRe = regexp.MustCompile(`^\s*import\s+(static\s+)?([\w.` + "`" + `]+?)(\.\*)?(\s+as\s+(\w+))?\s*;?\s*$`)

// unusedImports lists the imports of a Kotlin or Java file whose simple name
// does not appear again in the file. Star imports are skipped.
func unusedImports(path, src string) []string {
	if !strings.HasSuffix(path, ".kt") && !strings.HasSuffix(path, ".kts") && !strings.HasSuffix(path, ".java") {
		return nil
	}
	lines := strings.Split(src, "\n")
	type imp struct{ full, name string }
	var imps []imp
	var body strings.Builder
	for _, l := range lines {
		if m := importRe.FindStringSubmatch(l); m != nil {
			if m[3] != "" {
				continue
			}
			name := m[5]
			if name == "" {
				name = m[2][strings.LastIndex(m[2], ".")+1:]
			}
			imps = append(imps, imp{m[2], strings.Trim(name, "`")})
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(l), "package ") {
			continue
		}
		body.WriteString(l + "\n")
	}
	text := body.String()
	var out []string
	for _, im := range imps {
		if kotlinOperators[im.name] {
			continue
		}
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(im.name) + `\b`).MatchString(text) {
			out = append(out, im.full)
		}
	}
	return out
}

// runChecks runs the "Done when" commands and keeps one line per command in
// st.Checks. When one fails, it returns a message for the model; else "".
func (a *Agent) runChecks(ctx context.Context, tools *Tools, cmds []string, wantClean bool, st *Stats, emit func(Event)) string {
	st.Checks = nil
	var fails, warns []string
	for _, c := range cmds {
		emit(Event{T: "tool", Tool: "check", Text: c})
		args, _ := json.Marshal(map[string]any{"command": c, "timeout": 900})
		out, ok := tools.Run(ctx, "bash", string(args))
		summary := firstLine(out)
		if !ok {
			summary = failLine(out)
		}
		emit(Event{T: "result", Tool: "check", OK: ok, Text: summary})
		if ok {
			w := warningLines(out)
			if wantClean && len(w) > 0 {
				st.Checks = append(st.Checks, fmt.Sprintf("qh check passed with %d warnings: %s", len(w), c))
				warns = append(warns, w...)
			} else {
				st.Checks = append(st.Checks, "qh check passed: "+c)
			}
			continue
		}
		st.Checks = append(st.Checks, "qh check FAILED: "+c)
		tail := out
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		fails = append(fails, "`"+c+"`:\n"+tail)
	}
	if len(fails) == 0 && len(warns) == 0 {
		return ""
	}
	if len(fails) == 0 {
		if len(warns) > 30 {
			warns = warns[:30]
		}
		return "qh ran the commands from the \"Done when\" part after your report. They passed, but the task asks for clean code and the compiler printed these warnings:\n\n" +
			strings.Join(warns, "\n") + "\n\nFix them, run the command again, then give the final report."
	}
	return "qh ran the commands from the \"Done when\" part after your report, and these failed:\n\n" +
		strings.Join(fails, "\n\n") + "\n\nFix the cause, run the command again, then give the final report."
}

// noWarnings matches a task that asks for code without warnings.
var noWarnings = regexp.MustCompile(`(?i)unused|no warnings|without warnings|warning-free|zero warnings`)

var warnLine = regexp.MustCompile(`(?i)^w: |\bwarning:|\[warning\]|is never used|unused import|declared and not used|imported and not used`)

// warningLines returns the compiler warnings in out (Kotlin "w: ", javac,
// gcc and clang "warning:", Maven "[WARNING]", Go "declared and not used").
func warningLines(out string) []string {
	var w []string
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if warnLine.MatchString(l) && !seen[l] && !strings.Contains(strings.ToLower(l), "deprecat") {
			seen[l] = true
			w = append(w, oneLine(l, 300))
		}
	}
	return w
}

var errWords = regexp.MustCompile(`(?i)\berror\b|error:|\bfailed\b|failure|exception|panic|cannot|not found|denied|^e: `)

// failLine picks the line that says what went wrong in the output of a
// failed call: the first line that looks like an error, else the last line
// with text. Build tools print banners first, so the first line rarely helps.
func failLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	exit := ""
	if n := len(lines); n > 1 && strings.HasPrefix(lines[n-1], "[exit ") {
		exit, lines = " "+lines[n-1], lines[:n-1]
	}
	for _, l := range lines {
		if errWords.MatchString(l) {
			return oneLine(l, 140) + exit
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return oneLine(lines[i], 140) + exit
		}
	}
	return strings.TrimSpace(exit)
}

const limitAsk = `You reached the step limit, and the tools are off now. Write the final report: what is done, what is not done, the files you changed, and the last test result you saw.`

// limitReport asks for a report without tools after the step limit, so the
// run still ends with one.
func (a *Agent) limitReport(ctx context.Context, s *Session, system Message, schemas []map[string]any, emit func(Event), told *bool) {
	s.Msgs = append(s.Msgs, Message{Role: "user", From: "qh", Content: limitAsk})
	msg, _, err := a.chat(ctx, Request{Messages: append([]Message{system}, s.Msgs...), Tools: schemas, Think: false, MaxTok: a.Cfg.MaxTokens},
		func(d Delta) bool {
			if d.Text != "" {
				emit(Event{T: "text", Text: d.Text})
			}
			return true
		}, emit, told)
	if err != nil {
		emit(Event{T: "error", Text: err.Error()})
		return
	}
	msg.ToolCalls = nil // calls without results would break the next request
	if strings.TrimSpace(msg.Content) == "" {
		msg.Content = "(no report: the run stopped at the step limit)"
	}
	s.Msgs = append(s.Msgs, msg)
}
