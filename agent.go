package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Session struct {
	Name       string     `json:"name"`
	Cwd        string     `json:"cwd"`
	Think      bool       `json:"think"`
	ReadOnly   bool       `json:"read_only"`
	Todo       []TodoItem `json:"todo,omitempty"`
	LastPrompt int        `json:"last_prompt"` // tokens of the last request
	Msgs       []Message  `json:"messages"`
	System     string     `json:"system,omitempty"`  // the system prompt, kept for the session (see systemPrompt)
	SysKey     string     `json:"sys_key,omitempty"` // the cwd and mode it was built for
	mu         sync.Mutex // held for the whole run

	wmu      sync.Mutex // guards watchers and cancel
	watchers map[chan Event]struct{}
	cancel   context.CancelFunc
	running  bool   // a run holds mu
	step     int    // its step
	waiting  bool   // the run waits for the model (another session uses it)
	from     string // who started the run

	seen  map[string]*fileSeen // files qh showed the model, for the stale-file guard
	reads map[string]*readMark // reads whose result the model got, for the re-read note
	runs  int                  // runs so far in this daemon
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
	Model        string         `json:"model"`
	Steps        int            `json:"steps"`
	Prompt       int            `json:"prompt_tokens"`
	Cached       int            `json:"cached_tokens"`
	Fresh        int            `json:"fresh_tokens"`
	Explores     int            `json:"explore_runs,omitempty"` // explore sub-runs; their steps and tokens are not in the fields above
	ExploreSteps int            `json:"explore_steps,omitempty"`
	ExploreOut   int            `json:"explore_output_tokens,omitempty"`
	ExploreFresh int            `json:"explore_fresh_tokens,omitempty"` // prompt tokens of all requests that were not in the server's cache
	Output       int            `json:"output_tokens"`
	Thinking     int            `json:"thinking_chars"`
	Secs         float64        `json:"secs"`
	Retries      int            `json:"retries"`
	Window       int            `json:"window"`
	ToolErrs     int            `json:"tool_errors"`
	Removes      int            `json:"removes,omitempty"`       // rm -r targets in bash commands
	CmdFails     int            `json:"failed_commands"`         // commands that ran and exited non-zero (failing builds and tests); not in ToolErrs
	Limit        bool           `json:"step_limit,omitempty"`    // the run stopped at max_steps
	Checks       []string       `json:"checks,omitempty"`        // results of the "Done when" commands
	Todo         []string       `json:"todo_open,omitempty"`     // todo items not done at the end
	Facts        []string       `json:"facts,omitempty"`         // what qh saw the run do, and where the report differs
	ThinkDropped int            `json:"think_dropped,omitempty"` // tokens of earlier turns' thinking removed at the start
	Ctx          map[string]int `json:"ctx_by_tool,omitempty"`   // tokens (chars/4) of tool results in the context at the end, per tool
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
	Sub    bool   `json:"sub,omitempty"` // a tool call of an explore sub-run
	Stats  *Stats `json:"stats,omitempty"`
}

type Agent struct {
	LLM  *LLM
	Cfg  Config
	Gate chan struct{} // one model request at a time across sessions; nil means no gate
	Save func()        // called after each step, so the session file stays current
	Wait func(bool)    // called when a request starts and stops waiting at the gate

	UndoDir string // where the run keeps the files it changes, for qh undo; "" keeps nothing
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

const selfCheckAsk = `Before you report, read the task again, mainly its "Done when" part and every numbered or bulleted requirement. For each one, say done or not done. For each numeric rule in the task (a range, a minimum or maximum, a limit, a default), name the file and line that enforces it, and check what happens at and past the edge. For each assertion in the tests you wrote, describe the smallest wrong implementation (for example: the expiry is reset on every call, the limit is off by one, the field is never set) and say whether the assertion fails on it. An assertion that still passes on that wrong code, like "ttl <= 3600" when the bug would also give 3600, is not done: make it exact. Watch for state that another thread sets later: an assertion made before it is set passes whatever the code does. For every try/catch you wrote, list the calls inside the try that can throw the caught type, and say for each one whether the task wants it handled that way; keep the try around only those calls. Fix every item that is not done. Then give the final report.`

var negRe = regexp.MustCompile(`(?i)\b(do not|don't|dont|never|must not|mustn't|should not|shouldn't|avoid|not\s+\S+|instead of|no more than|without|or blank|or empty|exactly|at least|at most|only if|unless)\b`)

// negativeRules quotes the lines of the task that say what not to do. The
// model slips on these more than on positive steps, so the self-check names
// them one by one.
func negativeRules(prompt string) string {
	var rules []string
	seen := map[string]bool{}
	for _, l := range strings.Split(prompt, "\n") {
		t := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-*0123456789.) "))
		if len(t) < 8 || !negRe.MatchString(t) || seen[t] {
			continue
		}
		seen[t] = true
		rules = append(rules, "- "+oneLine(t, 300))
		if len(rules) == 20 {
			break
		}
	}
	if len(rules) == 0 {
		return ""
	}
	return "\n\nThese lines of the task say what not to do or give an exact rule (\"or blank\" is not \"or empty\"). For each one, say whether your code keeps it word for word, and quote the code if you are not sure:\n" + strings.Join(rules, "\n")
}

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

var checkTools = []string{"grep ", "! grep ", "rg ", "! rg ", "test ", "[ ", "diff ", "cmp ", "git diff", "git status", "wc ", "jq ", "curl ", "./", "go ", "make", "npm ", "pnpm ", "yarn ", "mvn ", "gradle ", "cargo ", "pytest", "python", "uv ", "node ", "bun ", "deno ", "swift ", "dotnet ", "bash ", "sh ", "ctest", "cmake "}

// checkCommands finds the commands in backticks in the "Done when" part.
func checkCommands(prompt string) []string {
	cmds, _ := checkSpans(prompt)
	return cmds
}

// checkSpans splits the backticked spans of the "Done when" part into the
// commands qh runs and the ones that look like commands (their first word is
// a program on PATH) but are not on the list, so the report can say so.
func checkSpans(prompt string) (cmds, skipped []string) {
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
		ok := false
		for _, p := range checkTools {
			if strings.HasPrefix(cmd, p) {
				out = append(out, cmd)
				ok = true
				break
			}
		}
		if f := strings.Fields(cmd); !ok && len(f) > 1 {
			if _, err := exec.LookPath(f[0]); err == nil {
				skipped = append(skipped, cmd)
			}
		}
	}
	return out, skipped
}

const systemPrompt = `You are a coding agent. You work in %s on %s. Your tools are read, write, edit, bash, bg, grep, glob, todo and lookup.

For a task with 3 or more steps, write a todo list first and keep it current.
Work in small steps. Read the code before you change it. After each change, build or run the tests and read the result.
Search code with grep and find files with glob. Use bash for git, builds and tests; filter long output with head or tail.
For a server or watcher, use bash with background: true and read its output with bg.
To check that a library has a class or top-level function and how to import it, use lookup.
To check a library API, print its signatures (javap -cp JAR CLASS, go doc PKG.NAME, python -c "help(...)") instead of unpacking its sources.
Use edit for small changes and write for new files. In edit, copy old exactly from the file and leave out the line numbers.
Do not ask for permission. Do the task.
If the user asks a general question or wants example code, answer it from what you know; search the files only when the question is about the code in this folder.
When you finish a task, reply with a short report: the files you changed, and the result of the last build or test. If you could not finish, say what is left.`

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
	if roots := sourceRoots(cwd); len(roots) > 0 {
		s += "\n\n# Where the code lives (paths from the repo root)\n\n" + strings.Join(roots, "\n")
	}
	return s
}

var rootRe = regexp.MustCompile(`^(.*?/)?src/(main|test|testFixtures|integrationTest)/(kotlin|java|scala|resources|groovy)/`)

var moduleFiles = map[string]bool{"go.mod": true, "package.json": true, "Cargo.toml": true, "pyproject.toml": true,
	"build.gradle.kts": true, "build.gradle": true, "pom.xml": true}

// sourceRoots lists the source folders and module roots of the git repo
// that holds cwd, so the model does not guess where a class lives. The list
// only changes when the layout does, so the prompt cache keeps working.
func sourceRoots(cwd string) []string {
	cmd := exec.Command("git", "ls-files", "--full-name", ":/")
	cmd.Dir = cwd
	b, err := cmd.Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var roots []string
	add := func(r string) {
		if !seen[r] && len(roots) < 40 {
			seen[r] = true
			roots = append(roots, r)
		}
	}
	for _, f := range strings.Split(string(b), "\n") {
		if m := rootRe.FindString(f); m != "" {
			add(m)
		} else if moduleFiles[filepath.Base(f)] && strings.Contains(f, "/") {
			add(filepath.Dir(f) + "/ (module)")
		}
	}
	if len(roots) < 2 {
		return nil
	}
	sort.Strings(roots)
	return roots
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
		if r := repoName(cwd); r != "" {
			if p := filepath.Join(d, "notes", r+".md"); exists(p) {
				out = append(out, p)
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

// repoName is the folder name of the git repo that holds dir, or "".
func repoName(dir string) string {
	for r := dir; ; r = filepath.Dir(r) {
		if _, err := os.Stat(filepath.Join(r, ".git")); err == nil {
			return filepath.Base(r)
		}
		if r == filepath.Dir(r) {
			return ""
		}
	}
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
	if n := dropOldThinking(s.Msgs); n > 0 {
		st.ThinkDropped = n
		s.LastPrompt = max(0, s.LastPrompt-n)
		emit(Event{T: "info", Text: fmt.Sprintf("dropped %d tokens of thinking from earlier turns", n)})
	}
	if s.LastPrompt > a.compactAt() {
		if err := a.Compact(ctx, s, "", emit); err != nil {
			emit(Event{T: "error", Text: "compaction failed: " + err.Error()})
		}
	}
	if s.seen == nil {
		s.seen = map[string]*fileSeen{}
	}
	s.runs++
	changed := map[string]bool{} // files that write and edit changed in this run
	tools := &Tools{Cwd: s.Cwd, ReadOnly: s.ReadOnly, Seen: s.seen, Changed: changed}
	tools.Diag, tools.DiagTimeout = a.diagCfg(s.Cwd)
	if !s.ReadOnly {
		tools.BG = newBG()
		defer tools.BG.Close()
		tools.Undo = newCheckpoint(a.UndoDir, s.Cwd, prompt)
		defer tools.Undo.finish()
	}
	schemas := a.schemas(s.ReadOnly)
	s.answerOpen()
	system := Message{Role: "system", Content: s.systemPrompt(a.systemExtra(s.ReadOnly))}
	s.Msgs = append(s.Msgs, Message{Role: "user", Content: prompt, From: from})
	if a.Save != nil {
		a.Save()
	}
	seen := map[string]int{}
	told := false
	ended := false // the model gave its final answer
	selfChecked := doneWhen(prompt) == ""
	checks, skippedChecks := checkSpans(prompt)
	checked := s.ReadOnly
	strays0 := map[string]bool{}
	if !s.ReadOnly {
		for _, f := range strayFiles(s.Cwd) {
			strays0[f] = true
		}
	}
	digs := 0                  // library-digging commands since the last passing build
	nudges := 0                // times the model was told to stop digging
	editFile, editRun := "", 0 // edits in a row on one file
	lastErrs := ""             // the compiler errors of the last failed build
	var nudge exploreNudge     // suggests explore after many searches in a big context
	blocked := 0               // library commands blocked after the second nudge
	reactiveTold := false
	removed := map[string]int{} // rm -r targets of bash commands in this run
	testEdits := 0              // writes and edits to test files since the last main-code change
	fixTurn := false            // the model got one turn to fix failed checks
	wantClean := noWarnings.MatchString(prompt)
	todoDue := false // remind the model of its open todo items after the next tool call
	defer s.setState(false, 0)

	// limit grows by passGrace for each pass that qh asks for after the
	// model's answer (self-check, fixes after the checks), so a run that
	// finished its task does not run out of steps in the clean-up.
	const passGrace = 15
	limit := a.Cfg.MaxSteps
	for st.Steps < limit {
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
		st.Fresh += u.PromptTokens - u.Details.CachedTokens
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
				s.Msgs = append(s.Msgs, Message{Role: "user", Content: selfCheckAsk + negativeRules(prompt) + negativeAsserts(changed) + todoSelfCheck(s.Todo), From: "qh"})
				limit = max(limit, st.Steps+passGrace)
				continue
			}
			if !checked {
				// The checks run after the report, and once more after a fix
				// turn, so the check lines describe the code as it ends.
				// After the fix turn the model gets no more turns, so qh
				// removes the unused imports itself.
				failed := a.finalChecks(ctx, tools, prompt, checks, changed, wantClean, fixTurn, &st, emit)
				if failed != "" && !fixTurn {
					fixTurn = true
					s.Msgs = append(s.Msgs, Message{Role: "user", From: "qh", Content: failed})
					limit = max(limit, st.Steps+passGrace)
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
			var out string
			var ok bool
			if nudges >= 2 && tc.Function.Name == "bash" && libDig.MatchString(tc.Function.Arguments) {
				out = "error: qh blocks digging in library jars and caches for the rest of this run. Use lookup NAME to check a class or function name and its import, then write the code and compile it."
				if blocked++; blocked >= 2 && lastErrs != "" {
					out += "\n\n[qh: the last build failed with:\n" + lastErrs + "\nFix this line in your own file. Do not look for the answer in the library.]"
				}
			} else if tc.Function.Name == "todo" && !s.ReadOnly {
				out, ok = runTodo(s, tc.Function.Arguments, emit)
			} else if tc.Function.Name == "explore" && !s.ReadOnly && a.exploreOn() {
				out, ok = a.explore(ctx, s.Cwd, tc.Function.Arguments, &st, emit, &told)
			} else if note := s.reread(tools, tc.Function.Name, tc.Function.Arguments, st.Steps); note != "" {
				out, ok = note, true
			} else {
				out, ok = tools.Run(ctx, tc.Function.Name, tc.Function.Arguments)
				if tc.Function.Name == "read" && ok {
					s.sawRead(tools, tc.Function.Arguments, tc.ID, out, st.Steps)
				}
			}
			if i := strings.Index(out, "[qh: compiler errors]\n"); i >= 0 {
				e := out[i+len("[qh: compiler errors]\n"):]
				if j := strings.Index(e, "\n["); j >= 0 {
					e = e[:j]
				}
				lastErrs = e
			}
			if tc.Function.Name == "bash" {
				var ba struct{ Command string }
				json.Unmarshal([]byte(tc.Function.Arguments), &ba)
				for _, t := range rmTargets(ba.Command) {
					st.Removes++
					if removed[t]++; removed[t] == 3 {
						out += "\n\n[qh: that is the third time you deleted " + t + ". Deleting caches or build output rarely fixes a build; read the error and fix the code or the config it names.]"
						emit(Event{T: "info", Text: "repeated rm -r of " + t})
					}
				}
			}
			if !reactiveTold && strings.Contains(out, "[qh: failing tests from the JUnit XML reports]") && usesReactor(changed) {
				reactiveTold = true
				out += "\n\n" + reactiveHint
				emit(Event{T: "info", Text: "tests fail on Reactor code: adding the empty-Mono hint"})
			}
			wrotePath := ""
			if (tc.Function.Name == "edit" || tc.Function.Name == "write") && ok {
				wrotePath, _ = filepath.Rel(tools.Cwd, tools.LastPath)
			}
			if wrotePath != "" {
				if isTestPath(wrotePath) {
					if testEdits++; testEdits == 8 || testEdits == 16 {
						out += fmt.Sprintf("\n\n[qh: that is %d changes to test files and none to the main code since then. The tests may be right: read the failure again, mainly the actual values and the response, and check the main code against it.]", testEdits)
						emit(Event{T: "info", Text: "many test edits: pointing the model at the main code"})
					}
				} else {
					testEdits = 0
				}
			}
			if tc.Function.Name == "edit" && ok {
				if wrotePath == editFile {
					editRun++
				} else {
					editFile, editRun = wrotePath, 1
				}
				if editRun == 5 {
					out += "\n\n[qh: that is 5 edits in a row on this file. For many small changes in one file, write the whole file once, or make them with one sed or perl command.]"
				}
			} else if tc.Function.Name != "read" {
				editFile, editRun = "", 0
			}
			switch {
			case tc.Function.Name == "bash" && ok && testCmd.MatchString(tc.Function.Arguments) && !libDig.MatchString(tc.Function.Arguments):
				digs = 0
			case tc.Function.Name == "bash" && libDig.MatchString(tc.Function.Arguments):
				if digs++; digs >= 3 {
					digs = 0
					nudges++
					if lastErrs != "" {
						out += "\n\n[qh: stop exploring the library. The build failed with these errors:\n" + lastErrs +
							"\nRead the line each error points to and fix it there. For a name the compiler cannot find, call lookup with it.]"
						emit(Event{T: "info", Text: "library digging: pointing the model back at the compiler errors"})
					} else {
						out += "\n\n[qh: stop exploring the library. Use lookup to check a class or function name and its import, then write the code and compile it.]"
						emit(Event{T: "info", Text: "library digging: telling the model to compile instead"})
					}
					if nudges == 2 {
						out += "\n[qh: from now on, commands that read library jars or caches are blocked in this run.]"
					}
				}
			}
			if n := seen[key]; n >= 3 {
				out = fmt.Sprintf("[you made this exact call %d times; the result will not change. Try a different step.]\n", n) + out
			}
			if !ok && failKind(tc.Function.Name, out) == "failed" {
				st.CmdFails++
			} else if !ok {
				st.ToolErrs++
			}
			summary := firstLine(out)
			if !ok {
				summary = failLine(out)
			}
			emit(Event{T: "result", Tool: tc.Function.Name, OK: ok, Text: summary})
			if !s.ReadOnly && a.exploreOn() {
				out += nudge.after(tc.Function.Name, tc.Function.Arguments, s.LastPrompt)
			}
			s.Msgs = append(s.Msgs, Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: out})
			if ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil {
			emit(Event{T: "info", Text: "interrupted"})
			break
		}
		last := &s.Msgs[len(s.Msgs)-1]
		if left := limit - st.Steps; left == 5 {
			last.Content += "\n\n[qh: 5 steps left before the step limit. Finish the work, run the check, and write the final report.]"
		} else if st.Steps%10 == 0 {
			last.Content += fmt.Sprintf("\n\n[qh: step %d of %d]", st.Steps, limit)
		}
		if tools.BG != nil {
			last.Content += tools.BG.Exits()
		}
		if st.Steps%todoEvery == 0 || todoDue {
			last.Content += todoReminder(s.Todo)
			todoDue = false
		}
		if s.LastPrompt > a.compactAt() {
			if err := a.Compact(ctx, s, "the task that is in progress now", emit); err != nil {
				emit(Event{T: "error", Text: "compaction failed: " + err.Error()})
				break
			}
			s.Msgs = append(s.Msgs, Message{Role: "user", From: "compact",
				Content: "Continue the task from the summary. Check the files before you change them again."})
			todoDue = true
			system.Content = s.systemPrompt(a.systemExtra(s.ReadOnly))
		}
	}
	if !ended && st.Steps >= limit && ctx.Err() == nil {
		st.Limit = true
		emit(Event{T: "info", Text: fmt.Sprintf("stopped at the step limit (%d); asking for the report", limit)})
		a.limitReport(ctx, s, system, schemas, emit, &told)
		if !checked {
			a.finalChecks(ctx, tools, prompt, checks, changed, wantClean, true, &st, emit)
		}
	}
	if !s.ReadOnly {
		for _, f := range strayFiles(s.Cwd) {
			if !strays0[f] {
				st.Checks = append(st.Checks, "qh note: the run left a backup file: "+f)
			}
		}
		for _, c := range skippedChecks {
			st.Checks = append(st.Checks, "qh check skipped (not a command qh runs): "+c)
		}
		stopped := tools.BG.Close()
		if len(stopped) > 0 {
			emit(Event{T: "info", Text: "stopped background commands: " + strings.Join(stopped, ", ")})
		}
		tools.Undo.finish()
		report := ""
		if n := len(s.Msgs); n > 0 && s.Msgs[n-1].Role == "assistant" {
			report = s.Msgs[n-1].Content
		}
		st.Facts = facts(report, tools.Undo.changes(), tools.Tests, stopped, tools.Undo != nil)
	}
	st.Todo = todoOpen(s.Todo)
	st.Ctx = ctxByTool(s.Msgs)
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
	case "glob":
		return str("pattern")
	case "todo":
		if it, err := parseTodo(raw); err == nil {
			d := 0
			for _, x := range it {
				if x.Status == "done" {
					d++
				}
			}
			return fmt.Sprintf("%d items, %d done", len(it), d)
		}
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
func (a *Agent) finalChecks(ctx context.Context, tools *Tools, prompt string, cmds []string, changed map[string]bool, wantClean, fixImports bool, st *Stats, emit func(Event)) string {
	a.format(ctx, tools, changed, st, emit)
	if fixImports {
		// Before the checks, so a wrong removal shows as a failed build.
		removeUnusedImports(tools.Cwd, changed, st, emit)
	}
	tools.Refresh(changed)
	failed := ""
	if len(cmds) > 0 {
		failed = a.runChecks(ctx, tools, cmds, checkExpects(prompt), wantClean, changedTests(changed), st, emit)
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
	if smells := reflectionInTests(files); len(smells) > 0 {
		emit(Event{T: "info", Text: fmt.Sprintf("%d tests use reflection on private fields", len(smells))})
		for _, m := range smells {
			st.Checks = append(st.Checks, "qh note: "+m)
		}
		msg := "These tests reach private fields by reflection:\n" + strings.Join(smells, "\n") +
			"\n\nThat usually hides a design or compile problem. Set the value through a constructor, a setter or test config instead; if you cannot, say why in the final report."
		if failed != "" {
			failed += "\n\n" + msg
		} else {
			failed = msg
		}
	}
	if preds := gatewayPredicates(files); len(preds) > 0 {
		emit(Event{T: "info", Text: fmt.Sprintf("%d gateway routes with predicates that replace each other", len(preds))})
		for _, m := range preds {
			st.Checks = append(st.Checks, "qh note: "+m)
		}
		msg := "In the Spring Cloud Gateway Kotlin DSL, each predicate call on its own line replaces the one before it, so the route matches on the last one only:\n" + strings.Join(preds, "\n") +
			"\n\nJoin them with and (path(\"/x\") and method(HttpMethod.GET)), build again, then give the final report."
		if failed != "" {
			failed += "\n\n" + msg
		} else {
			failed = msg
		}
	}
	if miss := missingAnnotations(prompt, files); len(miss) > 0 {
		emit(Event{T: "info", Text: fmt.Sprintf("%d annotations from the task are missing", len(miss))})
		for _, m := range miss {
			st.Checks = append(st.Checks, "qh note: "+m)
		}
		msg := "The task's code has annotations that the written files do not:\n" + strings.Join(miss, "\n") +
			"\n\nAdd them (tests that build the class by hand do not catch this), build again, then give the final report."
		if failed != "" {
			failed += "\n\n" + msg
		} else {
			failed = msg
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

// formatCfg merges the global format settings with the ones of the repo.
func (a *Agent) formatCfg(cwd string) FormatCfg {
	f := FormatCfg{Format: map[string]string{}, SortImports: a.Cfg.SortImports}
	for k, v := range a.Cfg.Format {
		f.Format[k] = v
	}
	if r, ok := a.Cfg.Repos[repoName(cwd)]; ok {
		for k, v := range r.Format {
			f.Format[k] = v
		}
		if r.SortImports != nil {
			f.SortImports = r.SortImports
		}
	}
	return f
}

// format sorts imports and runs the configured formatters on the changed
// files. Style rules then never cost the model a turn.
func (a *Agent) format(ctx context.Context, tools *Tools, changed map[string]bool, st *Stats, emit func(Event)) {
	if len(changed) == 0 {
		return
	}
	cfg := a.formatCfg(tools.Cwd)
	files := make([]string, 0, len(changed))
	for f := range changed {
		files = append(files, f)
	}
	sort.Strings(files)
	var fixed []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if out, n := fixColumnZero(f, string(b)); n > 0 && os.WriteFile(f, []byte(out), 0o644) == nil {
			rel, _ := filepath.Rel(tools.Cwd, f)
			fixed = append(fixed, fmt.Sprintf("%s (%d lines)", rel, n))
		}
	}
	if len(fixed) > 0 {
		emit(Event{T: "info", Text: "indented members written at column 0 in " + strings.Join(fixed, ", ")})
		st.Checks = append(st.Checks, "qh indented members that were at column 0 inside a class: "+strings.Join(fixed, ", "))
	}
	if cfg.SortImports != nil && *cfg.SortImports {
		var sorted []string
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if out := sortImports(f, string(b)); out != string(b) {
				if os.WriteFile(f, []byte(out), 0o644) == nil {
					rel, _ := filepath.Rel(tools.Cwd, f)
					sorted = append(sorted, rel)
				}
			}
		}
		if len(sorted) > 0 {
			emit(Event{T: "info", Text: "sorted the imports of " + strings.Join(sorted, ", ")})
			st.Checks = append(st.Checks, "qh sorted the imports of: "+strings.Join(sorted, ", "))
		}
	}
	byExt := map[string][]string{}
	for _, f := range files {
		if _, ok := cfg.Format[filepath.Ext(f)]; ok {
			if _, err := os.Stat(f); err == nil {
				byExt[filepath.Ext(f)] = append(byExt[filepath.Ext(f)], f)
			}
		}
	}
	exts := make([]string, 0, len(byExt))
	for e := range byExt {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	for _, e := range exts {
		cmd := cfg.Format[e]
		for _, f := range byExt[e] {
			cmd += " '" + strings.ReplaceAll(f, "'", `'\''`) + "'"
		}
		emit(Event{T: "tool", Tool: "format", Text: fmt.Sprintf("%s (%d files)", cfg.Format[e], len(byExt[e]))})
		args, _ := json.Marshal(map[string]any{"command": cmd, "timeout": 300})
		out, ok := tools.Run(ctx, "bash", string(args))
		emit(Event{T: "result", Tool: "format", OK: ok, Text: firstLine(out)})
		state := "passed"
		if !ok {
			state = "FAILED"
		}
		st.Checks = append(st.Checks, fmt.Sprintf("qh format %s: %s on %d files", state, cfg.Format[e], len(byExt[e])))
	}
}

var memberStart = regexp.MustCompile(`^(@\w|fun |val |var |private |internal |protected |public |override |open |abstract |suspend |inline |const |lateinit |companion |data |enum |sealed |inner |init\b|class |object |interface |static |final |void |[A-Z]\w*(<[^>]*>)?\s+\w+\s*\()`)

// fixColumnZero indents declarations that sit at column 0 inside a class
// body of a Kotlin or Java file, with the block that follows them. Code at
// column 0 there is always a slip, and it compiles.
func fixColumnZero(path, src string) (string, int) {
	if !strings.HasSuffix(path, ".kt") && !strings.HasSuffix(path, ".java") {
		return src, 0
	}
	lines := strings.Split(src, "\n")
	unit := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "\t") {
			unit = "\t"
			break
		}
		if n := len(l) - len(strings.TrimLeft(l, " ")); n > 0 && n < len(l) {
			unit = strings.Repeat(" ", n)
			break
		}
	}
	if unit == "" {
		unit = "    "
	}
	depths := braceDepths(lines)
	if depths == nil {
		return src, 0
	}
	fixed := 0
	for i := 0; i < len(lines); i++ {
		d := depths[i]
		if d <= 0 || lines[i] == "" || lines[i][0] == ' ' || lines[i][0] == '\t' || !memberStart.MatchString(lines[i]) {
			continue
		}
		pad := strings.Repeat(unit, d)
		j := i
		for ; j < len(lines); j++ {
			if lines[j] != "" {
				lines[j] = pad + lines[j]
				fixed++
			}
			t := strings.TrimSpace(lines[j])
			cont := strings.HasPrefix(t, "@") || strings.HasSuffix(t, "=") || strings.HasSuffix(t, ",") || strings.HasSuffix(t, "(") || strings.HasSuffix(t, ".")
			if j+1 < len(lines) {
				n := strings.TrimSpace(lines[j+1])
				cont = cont || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "?.") || strings.HasPrefix(n, ":")
			}
			if j+1 >= len(lines) || (depths[j+1] <= d && !cont) {
				break
			}
		}
		i = j
	}
	return strings.Join(lines, "\n"), fixed
}

// braceDepths gives the brace depth at the start of each line, skipping
// strings, chars and comments. It returns nil when it cannot follow the file
// (a multi-line string, or braces that do not balance).
func braceDepths(lines []string) []int {
	out := make([]int, len(lines))
	d, inBlock := 0, false
	for i, l := range lines {
		out[i] = d
		for k := 0; k < len(l); k++ {
			c := l[k]
			if inBlock {
				if c == '*' && k+1 < len(l) && l[k+1] == '/' {
					inBlock = false
					k++
				}
				continue
			}
			switch {
			case c == '/' && k+1 < len(l) && l[k+1] == '/':
				k = len(l)
			case c == '/' && k+1 < len(l) && l[k+1] == '*':
				inBlock = true
				k++
			case c == '"' && strings.HasPrefix(l[k:], `"""`):
				e := strings.Index(l[k+3:], `"""`)
				if e < 0 {
					return nil
				}
				k += e + 5
			case c == '"' || c == '\'':
				for k++; k < len(l) && l[k] != c; k++ {
					if l[k] == '\\' {
						k++
					}
				}
			case c == '{':
				d++
			case c == '}':
				d--
			}
		}
	}
	if d != 0 || inBlock {
		return nil
	}
	return out
}

// sortImports sorts the import block of a Kotlin or Java file. Kotlin uses
// the IntelliJ layout (other packages, then java, javax and kotlin, then
// aliases), Java puts the static imports last. ASCII order inside each group;
// duplicates go. A block with comments in it stays as it is.
func sortImports(path, src string) string {
	kt := strings.HasSuffix(path, ".kt") || strings.HasSuffix(path, ".kts")
	if !kt && !strings.HasSuffix(path, ".java") {
		return src
	}
	lines := strings.Split(src, "\n")
	first, last := -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "import ") {
			if first < 0 {
				first = i
			}
			last = i
			continue
		}
		if first >= 0 && t != "" {
			break
		}
	}
	if first < 0 {
		return src
	}
	groups := make([][]string, 5)
	seen := map[string]bool{}
	for _, l := range lines[first : last+1] {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "import ") || strings.Contains(t, "//") || strings.Contains(t, "/*") {
			return src
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		name := importKey(t)
		g := 0
		switch {
		case kt && strings.Contains(name, " as "):
			g = 4
		case kt && strings.HasPrefix(name, "java."):
			g = 1
		case kt && strings.HasPrefix(name, "javax."):
			g = 2
		case kt && strings.HasPrefix(name, "kotlin."):
			g = 3
		case !kt && strings.HasPrefix(strings.TrimPrefix(t, "import "), "static "):
			g = 4
		}
		groups[g] = append(groups[g], t)
	}
	var block []string
	for g, ls := range groups {
		sort.Slice(ls, func(i, j int) bool { return importKey(ls[i]) < importKey(ls[j]) })
		if !kt && g == 4 && len(ls) > 0 && len(block) > 0 {
			block = append(block, "")
		}
		block = append(block, ls...)
	}
	out := append([]string{}, lines[:first]...)
	out = append(out, block...)
	out = append(out, lines[last+1:]...)
	return strings.Join(out, "\n")
}

func importKey(l string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(l, "import "), "static "), ";")
}

// removeUnusedImports deletes the import lines that unusedImports reports in
// the changed files and notes each file in the report.
func removeUnusedImports(cwd string, changed map[string]bool, st *Stats, emit func(Event)) {
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
		unused := unusedImports(f, string(b))
		if len(unused) == 0 {
			continue
		}
		drop := map[string]bool{}
		for _, u := range unused {
			drop[u] = true
		}
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			if m := importRe.FindStringSubmatch(l); m != nil && m[3] == "" && m[5] == "" && drop[m[2]] {
				continue
			}
			keep = append(keep, l)
		}
		if os.WriteFile(f, []byte(strings.Join(keep, "\n")), 0o644) == nil {
			rel, _ := filepath.Rel(cwd, f)
			emit(Event{T: "info", Text: fmt.Sprintf("removed %d unused imports from %s", len(unused), rel)})
			st.Checks = append(st.Checks, fmt.Sprintf("qh removed unused imports from %s: %s", rel, strings.Join(unused, ", ")))
		}
	}
}

var strayRe = regexp.MustCompile(`(\.bak|\.orig|\.rej|\.tmp|\.swp|~)$`)

// strayFiles lists the untracked backup and temp files in the git repo of dir.
func strayFiles(dir string) []string {
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var fs []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "?? ") && strayRe.MatchString(l) {
			fs = append(fs, strings.Trim(l[3:], `"`))
		}
	}
	return fs
}

var negAssert = regexp.MustCompile(`doesNotContain|doesNotHave|isNotEqualTo|isNotIn\b|assertNotEquals|assertFalse|isFalse\(\)|wasNot\s+Called|exactly\s*=\s*0|times\(0\)|never\(\)|shouldNot`)

// negativeAsserts quotes the assertions in changed test files that check
// for absence. They pass on empty, encoded or wrong values, so the
// self-check asks what the value holds when the code is right.
func negativeAsserts(changed map[string]bool) string {
	files := make([]string, 0, len(changed))
	for f := range changed {
		if isTestPath(f) {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, l := range strings.Split(string(b), "\n") {
			if negAssert.MatchString(l) && len(out) < 15 {
				out = append(out, fmt.Sprintf("%s:%d: %s", filepath.Base(f), i+1, strings.TrimSpace(l)))
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "\n\nThese assertions check that something is absent. Each passes when the value is empty, encoded (base64, escaped) or the wrong object. For each one, say what the checked value holds when the code is right, and make sure it would contain the thing if the code were wrong:\n" + strings.Join(out, "\n")
}

const reactiveHint = "[qh: this is Reactor code. A failure with no exception (wrong status, empty body, a handler never called) is often an empty Mono: a Mono that completes empty skips every later map and flatMap. map must not return null (use mapNotNull or flatMap with Mono.empty() on purpose), Mono.create { it.success(null) } is empty, and defaultIfEmpty or switchIfEmpty shows where it went empty. Check the main code's pipeline before you change the tests.]"

// usesReactor is true when a changed main-code file uses Mono or Flux.
func usesReactor(changed map[string]bool) bool {
	for f := range changed {
		if isTestPath(f) {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && (bytes.Contains(b, []byte("reactor.core.publisher")) || bytes.Contains(b, []byte("Mono<")) || bytes.Contains(b, []byte("Flux<"))) {
			return true
		}
	}
	return false
}

var gwPred = regexp.MustCompile(`^\s*(path|method|host|header|query|cookie|remoteAddr|weight|before|after|between|readBody)\s*\(`)

// gatewayPredicates finds route blocks in Kotlin gateway DSL files with two
// predicate calls as separate statements; the second silently replaces the
// first.
func gatewayPredicates(files []string) []string {
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".kt") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil || !bytes.Contains(b, []byte("route(")) || !bytes.Contains(b, []byte("uri(")) {
			continue
		}
		lines := strings.Split(string(b), "\n")
		prev := -1
		for i, l := range lines {
			t := strings.TrimSpace(l)
			switch {
			case gwPred.MatchString(l):
				joined := prev >= 0 && (strings.HasSuffix(strings.TrimSpace(lines[prev]), " and") || strings.HasSuffix(strings.TrimSpace(lines[prev]), " or"))
				if prev >= 0 && !joined && !strings.HasPrefix(t, "and ") && !strings.HasPrefix(t, "or ") {
					out = append(out, fmt.Sprintf("%s:%d: %s (after %s on line %d)", filepath.Base(f), i+1, t, strings.TrimSpace(lines[prev]), prev+1))
				}
				prev = i
			case strings.HasPrefix(t, "route(") || strings.HasPrefix(t, "}") || strings.Contains(t, "route {"):
				prev = -1
			}
		}
	}
	return out
}

// changedTests is true when the run wrote or edited a test file.
func changedTests(changed map[string]bool) bool {
	for f := range changed {
		if isTestPath(f) {
			return true
		}
	}
	return false
}

// rerunCmd makes Gradle run the tests again instead of calling them up to date.
func rerunCmd(c string) string {
	if strings.Contains(c, "gradle") && !strings.Contains(c, "--rerun") && !strings.ContainsAny(c, "|;&") {
		return c + " --rerun-tasks"
	}
	return c
}

var rmRe = regexp.MustCompile(`(?:^|[;&|]\s*|\s)rm\s+((?:-[a-zA-Z]+\s+)+)([^;&|]+)`)

// rmTargets lists what a command deletes with rm -r.
func rmTargets(cmd string) []string {
	var out []string
	for _, m := range rmRe.FindAllStringSubmatch(cmd, -1) {
		if !strings.ContainsAny(m[1], "rR") {
			continue
		}
		out = append(out, strings.Fields(m[2])...)
	}
	return out
}

// isTestPath is true for files named like tests or kept under a test folder.
func isTestPath(f string) bool {
	low := strings.ToLower(f)
	return strings.Contains(strings.ToLower(filepath.Base(f)), "test") || strings.Contains(low, "/test/") || strings.HasPrefix(low, "test/") || strings.Contains(low, "src/test")
}

var reflectRe = regexp.MustCompile(`getDeclaredField|getDeclaredMethod|isAccessible\s*=\s*true|setAccessible\(\s*true|trySetAccessible|ReflectionTestUtils|FieldUtils\.`)

// reflectionInTests lists the lines of changed test files that open up
// private members by reflection.
func reflectionInTests(files []string) []string {
	var out []string
	for _, f := range files {
		if !isTestPath(f) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, l := range strings.Split(string(b), "\n") {
			if reflectRe.MatchString(l) {
				out = append(out, fmt.Sprintf("%s:%d: %s", filepath.Base(f), i+1, strings.TrimSpace(l)))
			}
		}
	}
	return out
}

var annDecl = regexp.MustCompile(`((?:@\w+(?:\([^)]*\))?\s+)+)(?:(?:public|private|internal|protected|open|abstract|final|data|sealed|enum|static|suspend|override)\s+)*(class|object|interface|fun|record)\s+(\w+)`)

var annName = regexp.MustCompile(`@(\w+)`)

// missingAnnotations compares the annotations on classes and functions in
// the task's code blocks with the changed files that declare them, and
// lists the ones the files lack.
func missingAnnotations(prompt string, files []string) []string {
	var code strings.Builder
	parts := strings.Split(prompt, "```")
	for i := 1; i < len(parts); i += 2 {
		code.WriteString(parts[i] + "\n")
	}
	type want struct {
		kind, name string
		anns       []string
	}
	var wants []want
	for _, m := range annDecl.FindAllStringSubmatch(code.String(), -1) {
		var anns []string
		for _, a := range annName.FindAllStringSubmatch(m[1], -1) {
			anns = append(anns, a[1])
		}
		wants = append(wants, want{m[2], m[3], anns})
	}
	if len(wants) == 0 {
		return nil
	}
	srcs := map[string]string{}
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			srcs[f] = string(b)
		}
	}
	var out []string
	for _, w := range wants {
		decl := regexp.MustCompile(`(?m)^((?:\s*@\w+(?:\([^)]*\))?\s*\n?)*)[^\n@]*\b` + w.kind + `\s+` + w.name + `\b`)
		for f, src := range srcs {
			m := decl.FindStringSubmatch(src)
			if m == nil {
				continue
			}
			have := map[string]bool{}
			for _, a := range annName.FindAllStringSubmatch(m[0], -1) {
				have[a[1]] = true
			}
			for _, a := range w.anns {
				if !have[a] {
					out = append(out, fmt.Sprintf("%s: %s %s has no @%s", filepath.Base(f), w.kind, w.name, a))
				}
			}
			break
		}
	}
	return out
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

// expect is what the task says a check command must print.
type expect struct {
	empty bool   // "prints nothing", "is empty"
	none  string // "shows no .bak file": no line may contain this
	times int    // "x4", "4 times": every run must pass
}

var expTimes = regexp.MustCompile(`(?i)^\s*(?:(?:x|×)\s*(\d+)\b|(\d+|two|three|four|five|ten)\s+times\b)`)

var timesWords = map[string]int{"two": 2, "three": 3, "four": 4, "five": 5, "ten": 10}

var (
	expEmpty = regexp.MustCompile(`(?i)^\s*(?:\)\s*)?(?:shows|prints|lists|outputs|returns|gives|is|are)\s+(?:nothing|empty|no output|no lines)`)
	expNone  = regexp.MustCompile("(?i)^\\s*(?:\\)\\s*)?(?:shows|prints|lists|outputs|returns|gives|has)\\s+no\\s+`?([^\\s`,;]+)")
)

// checkExpects reads, for each backticked command of the "Done when" part,
// the words after it that say what it must print.
func checkExpects(prompt string) map[string]expect {
	out := map[string]expect{}
	part := doneWhen(prompt)
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
		var e expect
		rest := part
		if m := expTimes.FindStringSubmatch(part); m != nil {
			n := m[1] + m[2]
			if v, err := strconv.Atoi(n); err == nil {
				e.times = v
			} else {
				e.times = timesWords[strings.ToLower(n)]
			}
			e.times = min(e.times, 10)
			rest = part[len(m[0]):]
		}
		if expEmpty.MatchString(rest) {
			e.empty = true
		} else if m := expNone.FindStringSubmatch(rest); m != nil {
			e.none = strings.TrimRight(m[1], ".")
		}
		if e != (expect{}) {
			out[cmd] = e
		}
	}
	return out
}

// miss says why the output breaks the expectation, or "".
func (e expect) miss(out string) string {
	lines := outLines(out, 1000)
	switch {
	case e.empty && len(lines) > 0:
		return "the task says this prints nothing, but it printed: " + lines[0]
	case e.none != "":
		for _, l := range lines {
			if strings.Contains(l, e.none) {
				return fmt.Sprintf("the task says this shows no %s, but it printed: %s", e.none, l)
			}
		}
	}
	return ""
}

// outLines is the command output without qh's own lines and the exit line.
func outLines(out string, max int) []string {
	var ls []string
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "[exit ") || strings.HasPrefix(t, "[qh:") || t == "no matches [exit 1]" {
			continue
		}
		if len(ls) < max {
			ls = append(ls, oneLine(t, 200))
		}
	}
	return ls
}

var printOnlyRe = regexp.MustCompile(`^(git (status|log|show|branch)|ls|cat|echo|find|wc|head|tail|tree|pwd)\b`)

// printOnly is true for commands that exit 0 whatever they print, so their
// exit code is no check.
func printOnly(c string) bool {
	if printOnlyRe.MatchString(c) && !strings.ContainsAny(c, "|&;") {
		return true
	}
	return strings.HasPrefix(c, "git diff") && !strings.Contains(c, "--exit-code") && !strings.Contains(c, "--quiet") && !strings.ContainsAny(c, "|&;")
}

var testsLine = regexp.MustCompile(`\[qh: tests: ([^\]\n]+)\]`)

// runChecks runs the "Done when" commands and keeps one line per command in
// st.Checks. When one fails, it returns a message for the model; else "".
func (a *Agent) runChecks(ctx context.Context, tools *Tools, cmds []string, exps map[string]expect, wantClean, rerun bool, st *Stats, emit func(Event)) string {
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
		exp := exps[c]
		if ok {
			if why := exp.miss(out); why != "" {
				ok, summary = false, why
				out = why + "\n\n" + out
			}
		}
		emit(Event{T: "result", Tool: "check", OK: ok, Text: summary})
		if ok && exp.times > 1 {
			passed := 1
			for i := 2; i <= exp.times && ok; i++ {
				emit(Event{T: "tool", Tool: "check", Text: fmt.Sprintf("%s (run %d of %d)", c, i, exp.times)})
				args, _ := json.Marshal(map[string]any{"command": rerunCmd(c), "timeout": 900})
				o, k := tools.Run(ctx, "bash", string(args))
				if k {
					if why := exp.miss(o); why != "" {
						k, o = false, why+"\n\n"+o
					}
				}
				emit(Event{T: "result", Tool: "check", OK: k, Text: firstLine(o)})
				if k {
					passed++
				} else {
					ok, out = false, o
				}
			}
			if !ok {
				st.Checks = append(st.Checks, fmt.Sprintf("qh check FAILED (passed %d of %d runs): %s", passed, exp.times, c))
				tail := out
				if len(tail) > 3000 {
					tail = tail[len(tail)-3000:]
				}
				fails = append(fails, fmt.Sprintf("`%s` must pass %d times in a row; it passed %d, then failed:\n%s", c, exp.times, passed, tail))
				continue
			}
			st.Checks = append(st.Checks, fmt.Sprintf("qh check passed %d of %d runs: %s", passed, exp.times, c))
			continue
		}
		if ok && rerun && testCmd.MatchString(c) && !printOnly(c) {
			again := rerunCmd(c)
			emit(Event{T: "tool", Tool: "check", Text: again + " (again, the run changed tests)"})
			args, _ := json.Marshal(map[string]any{"command": again, "timeout": 900})
			out2, ok2 := tools.Run(ctx, "bash", string(args))
			if ok2 {
				if why := exp.miss(out2); why != "" {
					ok2, out2 = false, why+"\n\n"+out2
				}
			}
			emit(Event{T: "result", Tool: "check", OK: ok2, Text: firstLine(out2)})
			if !ok2 {
				st.Checks = append(st.Checks, "qh check FAILED (flaky: passed 1 of 2 runs): "+c)
				tail := out2
				if len(tail) > 3000 {
					tail = tail[len(tail)-3000:]
				}
				fails = append(fails, "`"+c+"` passed once and failed when qh ran it again, so a test is flaky (timing, ordering, shared state, a server closing early). Make it pass every time; do not just retry it:\n"+tail)
				continue
			}
		}
		if ok && exp == (expect{}) && printOnly(c) {
			st.Checks = append(st.Checks, "qh check info (its exit code proves nothing, read the output): "+c+"\n    "+strings.Join(outLines(out, 5), "\n    "))
			continue
		}
		if ok {
			w := warningLines(out, a.Cfg.WarnIgnore)
			if wantClean && len(w) > 0 {
				st.Checks = append(st.Checks, fmt.Sprintf("qh check passed with %d warnings: %s", len(w), c))
				warns = append(warns, w...)
			} else {
				line := "qh check passed: " + c
				if m := testsLine.FindStringSubmatch(out); m != nil {
					line += " (tests: " + m[1] + ")"
				}
				st.Checks = append(st.Checks, line)
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
		strings.Join(fails, "\n\n") + "\n\nFix the cause, run the command again, then give the final report. Do not call it an environment or test-setup problem unless you can name the failing assertion and show that the main code it runs is right."
}

// noWarnings matches a task that asks for code without warnings.
var noWarnings = regexp.MustCompile(`(?i)unused|no warnings|without warnings|warning-free|zero warnings`)

// warnLine matches compiler warnings only: Kotlin "w: ", javac, gcc and
// clang "file:line: warning:", Maven compiler "[WARNING] File.java:[l,c]", Go
// "declared and not used". JVM runtime notices ("WARNING: sun.misc.Unsafe
// ...") are not compiler warnings and do not count.
var warnLine = regexp.MustCompile(`^w: |\S+\.\w+:\d+(:\d+)?: warning:|^\[WARNING\] \S+\.(java|kt|scala):\[\d+|is never used|unused import|declared and not used|imported and not used`)

// warningLines returns the compiler warnings in out, minus deprecation
// notices and lines that match one of the ignore patterns.
func warningLines(out string, ignore []string) []string {
	var ign []*regexp.Regexp
	for _, p := range ignore {
		if re, err := regexp.Compile(p); err == nil {
			ign = append(ign, re)
		}
	}
	var w []string
	seen := map[string]bool{}
next:
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if !warnLine.MatchString(l) || seen[l] || strings.Contains(strings.ToLower(l), "deprecat") {
			continue
		}
		for _, re := range ign {
			if re.MatchString(l) {
				continue next
			}
		}
		seen[l] = true
		w = append(w, oneLine(l, 300))
	}
	return w
}

// libDig matches commands that read library jars or caches.
var libDig = regexp.MustCompile(`unzip|\bjar\s+[tx]|javap|sources\.jar|\.gradle/caches|\.m2/repository|go/pkg/mod|site-packages|node_modules/`)

var strongErr = regexp.MustCompile(`^e: |^\S+:\d+(:\d+)?:? (error|fatal)|\berror:|Exception:|Error:|panic:|expected:|AssertionFailed`)

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
	// A compiler or test message beats a generic "FAILED" line.
	for _, re := range []*regexp.Regexp{strongErr, errWords} {
		for _, l := range lines {
			if re.MatchString(l) {
				return oneLine(l, 140) + exit
			}
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
	// The tools stay in the request (they are part of the cached prompt), so
	// the model may still call one. Then it gets "tools are off" and one more try.
	var msg Message
	for try := 0; try < 3; try++ {
		var err error
		msg, _, err = a.chat(ctx, Request{Messages: append([]Message{system}, s.Msgs...), Tools: schemas, Think: false, MaxTok: a.Cfg.MaxTokens},
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
		if len(msg.ToolCalls) == 0 || try == 2 {
			break
		}
		s.Msgs = append(s.Msgs, msg)
		for _, tc := range msg.ToolCalls {
			s.Msgs = append(s.Msgs, Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name,
				Content: "error: the tools are off after the step limit. Write the final report now, as text."})
		}
	}
	msg.ToolCalls = nil // calls without results would break the next request
	if strings.TrimSpace(msg.Content) == "" {
		msg.Content = "(no report: the run stopped at the step limit)"
	}
	s.Msgs = append(s.Msgs, msg)
}
