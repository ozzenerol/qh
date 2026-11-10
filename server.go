package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// FormatCfg says what qh does to the files a run changed, after the model's
// final answer and before the checks.
type FormatCfg struct {
	Format      map[string]string `json:"format,omitempty"`       // file extension -> command; the file paths go at the end
	SortImports *bool             `json:"sort_imports,omitempty"` // sort the imports of Kotlin and Java files

	Diagnostics map[string]string `json:"diagnostics,omitempty"`         // file extension -> checker run after each write or edit; {file}, {dir}
	DiagTimeout int               `json:"diagnostics_timeout,omitempty"` // seconds per check, default 20
}

type Config struct {
	URL          string   `json:"url"`
	Model        string   `json:"model"`
	MaxSteps     int      `json:"max_steps"`
	MaxTokens    int      `json:"max_tokens"`
	ThinkBudget  int      `json:"think_budget"`  // chars of thinking in one step
	Context      int      `json:"context"`       // the model's context window in tokens
	CompactAt    int      `json:"compact_at"`    // compact when a request passes this; 0 means 80% of context
	WarnIgnore   []string `json:"warn_ignore"`   // regular expressions of warning lines that the checks ignore
	Explore      *bool    `json:"explore"`       // the explore tool; nil means on
	ExploreSteps int      `json:"explore_steps"` // step limit of one explore sub-run; 0 means 15
	Confine      *bool    `json:"confine"`       // file tools stay in the project; nil means on
	ReadRoots    []string `json:"read_roots"`    // more folders read, grep, ls may use; nil means the dependency caches
	Web          bool     `json:"web"`           // new sessions get web_search and fetch_url
	SearchURL    string   `json:"search_url"`    // SearXNG base URL for web_search, e.g. http://192.168.1.142:8080
	WriteNote    *bool    `json:"write_note"`    // note when write rewrites a big file for a small change; nil means on
	ReportLines  *int     `json:"report_lines"`  // line limit for the final report in the system prompt; nil means 8, 0 off
	EditThink    *bool    `json:"edit_think"`    // think on the step after a clean edit/write/todo step; nil means off
	FormatCfg
	Repos map[string]FormatCfg `json:"repos"` // settings for one repo, by the folder name of its git root
}

func qhDir() (string, error) {
	if d := os.Getenv("QH_HOME"); d != "" {
		return d, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".qh"), nil
}

func sockPath() string {
	if p := os.Getenv("QH_SOCK"); p != "" {
		return p
	}
	d, _ := qhDir()
	return filepath.Join(d, "qh.sock")
}

func loadConfig() Config {
	c := Config{URL: "http://127.0.0.1:8080/v1", MaxSteps: 120, MaxTokens: 16384, ThinkBudget: 30000, Context: 262144}
	if d, err := qhDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(d, "config.json")); err == nil {
			if err := json.Unmarshal(b, &c); err != nil {
				log.Printf("config.json: %v", err)
			}
		}
	}
	if u := os.Getenv("QH_URL"); u != "" {
		c.URL = u
	}
	if m := os.Getenv("QH_MODEL"); m != "" {
		c.Model = m
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c
}

// ClientReq is one line of JSON that a client sends after it connects.
type ClientReq struct {
	Op       string `json:"op"` // run, attach, cancel, set, reset, sessions, ping, stop
	Session  string `json:"session,omitempty"`
	From     string `json:"from,omitempty"`    // label shown to watchers, e.g. "opus"
	Detach   bool   `json:"detach,omitempty"`  // run: answer at once; events go to watchers only
	Compact  bool   `json:"compact,omitempty"` // run: compact the session; Prompt is the focus
	Cwd      string `json:"cwd,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	New      bool   `json:"new,omitempty"`
	Think    *bool  `json:"think,omitempty"`
	Web      *bool  `json:"web,omitempty"` // run, set: web_search and fetch_url for the session
	ReadOnly bool   `json:"read_only,omitempty"`
	Timeout  int    `json:"timeout,omitempty"` // run: seconds for the model loop; the checks still run after it
}

type chatReg struct{ from, session string }

type Server struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*Session
	chats    []chatReg // open terminal chats, the newest last
	stop     chan struct{}
	gate     chan struct{} // one model request at a time
}

var validName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func serve() error {
	dir, err := qhDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		return err
	}
	sp := sockPath()
	if c, err := net.DialTimeout("unix", sp, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("qh is already running on %s", sp)
	}
	os.Remove(sp)
	ln, err := net.Listen("unix", sp)
	if err != nil {
		return err
	}
	os.Chmod(sp, 0o600)
	s := &Server{cfg: loadConfig(), sessions: map[string]*Session{}, stop: make(chan struct{}), gate: make(chan struct{}, 1)}
	log.Printf("qh %s listening on %s, model server %s", version, sp, s.cfg.URL)
	defer killAllBG()
	go func() {
		<-s.stop
		ln.Close()
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		<-sig
		killAllBG()
		os.Remove(sp)
		os.Exit(1)
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-s.stop:
				os.Remove(sp)
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Printf("accept: %v", err)
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) session(name, cwd string) (*Session, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("bad session name %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss, ok := s.sessions[name]; ok {
		return ss, nil
	}
	ss := &Session{Name: name, Cwd: cwd, Think: true, Web: s.cfg.Web}
	if d, err := qhDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(d, "sessions", name+".json")); err == nil {
			json.Unmarshal(b, ss)
			ss.Name = name
		}
	}
	s.sessions[name] = ss
	return ss, nil
}

// deleteSession removes a session file. A session with a run in progress or
// an open chat stays.
func (s *Server) deleteSession(name string) Event {
	if !validName.MatchString(name) {
		return Event{T: "error", Text: fmt.Sprintf("bad session name %q", name)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.chats {
		if c.session == name {
			return Event{T: "error", Text: name + " is open in a chat"}
		}
	}
	if ss, ok := s.sessions[name]; ok {
		if run, _ := ss.state(); run {
			return Event{T: "error", Text: name + " is running"}
		}
		delete(s.sessions, name)
	}
	d, err := qhDir()
	if err != nil {
		return Event{T: "error", Text: err.Error()}
	}
	if err := os.Remove(filepath.Join(d, "sessions", name+".json")); err != nil {
		return Event{T: "error", Text: err.Error()}
	}
	return Event{T: "done", Text: "deleted " + name}
}

func (s *Server) save(ss *Session) {
	d, err := qhDir()
	if err != nil {
		return
	}
	b, _ := json.Marshal(ss)
	p := filepath.Join(d, "sessions", ss.Name+".json")
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req ClientReq
	enc := json.NewEncoder(c)
	var wmu sync.Mutex
	emit := func(e Event) {
		wmu.Lock()
		enc.Encode(e)
		wmu.Unlock()
	}
	if err := json.Unmarshal(line, &req); err != nil {
		emit(Event{T: "error", Text: "bad request: " + err.Error()})
		return
	}
	if req.Session == "" {
		req.Session = "main"
	}
	if req.Session == "@chat" {
		s.mu.Lock()
		if n := len(s.chats); n > 0 {
			req.Session = s.chats[n-1].session
		}
		s.mu.Unlock()
		if req.Session == "@chat" {
			emit(Event{T: "error", Text: "no chat is open; start qh in a terminal first"})
			return
		}
	}

	switch req.Op {
	case "ping":
		emit(Event{T: "done", Text: version})
	case "stop":
		emit(Event{T: "done", Text: "stopping"})
		close(s.stop)
	case "sessions":
		s.listSessions(emit)
	case "list":
		b, _ := json.Marshal(s.sessionList())
		emit(Event{T: "done", Text: string(b)})
	case "reset":
		ss, err := s.session(req.Session, req.Cwd)
		if err != nil {
			emit(Event{T: "error", Text: err.Error()})
			return
		}
		if !ss.mu.TryLock() {
			emit(Event{T: "error", Text: "session " + req.Session + " is busy"})
			return
		}
		ss.Msgs, ss.Todo = nil, nil
		clear(ss.seen)
		if req.Cwd != "" {
			ss.Cwd = req.Cwd
		}
		s.save(ss)
		ss.mu.Unlock()
		emit(Event{T: "done", Text: "reset " + req.Session})
	case "set":
		ss, err := s.session(req.Session, req.Cwd)
		if err != nil {
			emit(Event{T: "error", Text: err.Error()})
			return
		}
		if req.Think != nil || req.Web != nil {
			ss.mu.Lock()
			if req.Think != nil {
				ss.Think = *req.Think
			}
			if req.Web != nil {
				ss.Web = *req.Web
			}
			s.save(ss)
			ss.mu.Unlock()
		}
		emit(Event{T: "done", Text: fmt.Sprintf("thinking %s, web %s for %s", onOff(ss.Think), onOff(ss.Web), req.Session)})
	case "run":
		s.run(c, r, req, emit)
	case "compact":
		req.Compact = true
		if req.Cwd == "" {
			req.Cwd = "/"
		}
		s.run(c, r, req, emit)
	case "attach":
		s.attach(r, req, emit)
	case "delete":
		emit(s.deleteSession(req.Session))
	case "cancel":
		ss, err := s.session(req.Session, req.Cwd)
		if err != nil {
			emit(Event{T: "error", Text: err.Error()})
			return
		}
		if ss.cancelRun() {
			emit(Event{T: "done", Text: "cancelled"})
		} else {
			emit(Event{T: "done", Text: "nothing runs"})
		}
	default:
		emit(Event{T: "error", Text: "unknown op " + req.Op})
	}
}

func (s *Server) run(c net.Conn, r *bufio.Reader, req ClientReq, emit func(Event)) {
	if req.Cwd == "" || !filepath.IsAbs(req.Cwd) {
		emit(Event{T: "error", Text: "run needs an absolute cwd"})
		return
	}
	ss, err := s.session(req.Session, req.Cwd)
	if err != nil {
		emit(Event{T: "error", Text: err.Error()})
		return
	}
	if req.From == "" {
		req.From = "qh run"
	}
	ctx, cancel := context.WithCancel(context.Background())
	if req.Detach {
		emit(Event{T: "done", Text: "accepted"})
		go func() {
			defer cancel()
			s.execRun(ctx, cancel, ss, req, func(Event) {})
		}()
		return
	}
	defer cancel()
	// The client closes the connection to cancel the run.
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := r.Read(buf); err != nil {
				cancel()
				return
			}
		}
	}()
	s.execRun(ctx, cancel, ss, req, emit)
}

// execRun waits for its turn on the session, then runs the prompt. Every
// event goes to the requester (emit) and to the watchers of the session.
func (s *Server) execRun(ctx context.Context, cancel context.CancelFunc, ss *Session, req ClientReq, emit func(Event)) {
	all := func(e Event) {
		emit(e)
		ss.broadcast(e)
	}
	if !ss.mu.TryLock() {
		all(Event{T: "queued", From: req.From, Text: req.Prompt})
		if !lockCtx(ctx, &ss.mu) {
			all(Event{T: "dropped", From: req.From, Text: req.Prompt})
			return
		}
	}
	defer ss.mu.Unlock()
	ss.setCancel(cancel)
	defer ss.setCancel(nil)
	ss.setFrom(req.From)
	defer ss.setFrom("")
	if req.New {
		ss.Msgs, ss.Todo = nil, nil
		clear(ss.seen)
	}
	if !req.Compact {
		ss.Cwd = req.Cwd
		ss.ReadOnly = req.ReadOnly
	}
	if req.Think != nil {
		ss.Think = *req.Think
	}
	if req.Web != nil {
		ss.Web = *req.Web
	}
	s.save(ss) // the session is listed while it runs
	llm := &LLM{BaseURL: s.cfg.URL, Model: s.cfg.Model, HTTP: &http.Client{}}
	if err := llm.ResolveModel(ctx); err != nil {
		all(Event{T: "error", Text: err.Error()})
		all(Event{T: "done", From: req.From})
		return
	}
	a := &Agent{LLM: llm, Cfg: s.cfg, Gate: s.gate, Save: func() { s.save(ss) }, Wait: ss.setWaiting}
	a.UndoDir = undoRoot(ss.Name)
	if req.Compact {
		if err := a.Compact(ctx, ss, req.Prompt, all); err != nil {
			all(Event{T: "error", Text: err.Error()})
		}
		s.save(ss)
		all(Event{T: "done", From: req.From, Stats: &Stats{Model: filepath.Base(llm.Model), Window: s.cfg.Context, Prompt: estimateTokens(ss.Msgs)}})
		return
	}
	all(Event{T: "user", From: req.From, Text: req.Prompt})
	runCtx := ctx
	if req.Timeout > 0 {
		var stop context.CancelFunc
		runCtx, stop = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Second)
		defer stop()
		a.Wrap = ctx
	}
	st := a.Run(runCtx, ss, req.Prompt, req.From, all)
	s.save(ss)
	final := ""
	if n := len(ss.Msgs); n > 0 && ss.Msgs[n-1].Role == "assistant" {
		final = ss.Msgs[n-1].Content
	}
	if len(st.Checks) > 0 {
		blame := blameLine(final, st.Checks)
		final = checkVerdict(st.Checks) + blame + "\n\n" + strings.TrimSpace(final) + "\n\n" + strings.Join(st.Checks, "\n")
	}
	if len(st.Todo) > 0 {
		final = strings.TrimSpace(final) + "\n\nqh: todo items not done: " + strings.Join(st.Todo, "; ")
	}
	if len(st.Facts) > 0 {
		final = strings.TrimSpace(final) + "\n\n" + strings.Join(st.Facts, "\n")
	}
	all(Event{T: "done", From: req.From, Text: strings.TrimSpace(final), Stats: &st})
}

// lockCtx takes the mutex, or gives up when ctx ends.
func lockCtx(ctx context.Context, mu *sync.Mutex) bool {
	for {
		if mu.TryLock() {
			if ctx.Err() != nil {
				mu.Unlock()
				return false
			}
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// attach streams every event of the session to the client until it leaves.
// It starts with the history, marked as replay.
func (s *Server) attach(r *bufio.Reader, req ClientReq, emit func(Event)) {
	ss, err := s.session(req.Session, req.Cwd)
	if err != nil {
		emit(Event{T: "error", Text: err.Error()})
		return
	}
	ch := make(chan Event, 4096)
	gone := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := r.Read(buf); err != nil {
				close(gone)
				return
			}
		}
	}()
	// Take the history and join the watchers under one lock, so no event is lost
	// between them. The run lock is not needed: Msgs only grows during a run, and
	// a replay that misses the newest message of a running step is harmless.
	ss.wmu.Lock()
	hist := replay(ss.Msgs)
	if ss.watchers == nil {
		ss.watchers = map[chan Event]struct{}{}
	}
	ss.watchers[ch] = struct{}{}
	running := ss.cancel != nil
	ss.wmu.Unlock()
	if strings.HasPrefix(req.From, "chat:") {
		s.mu.Lock()
		s.chats = append(s.chats, chatReg{req.From, req.Session})
		s.mu.Unlock()
	}
	defer func() {
		ss.wmu.Lock()
		delete(ss.watchers, ch)
		ss.wmu.Unlock()
		s.mu.Lock()
		for i := len(s.chats) - 1; i >= 0; i-- {
			if s.chats[i].from == req.From && s.chats[i].session == req.Session {
				s.chats = append(s.chats[:i], s.chats[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	for _, e := range hist {
		emit(e)
	}
	emit(Event{T: "attached", OK: running, Text: ss.Cwd})
	for {
		select {
		case e := <-ch:
			emit(e)
		case <-gone:
			return
		case <-s.stop:
			return
		}
	}
}

func replay(msgs []Message) []Event {
	var out []Event
	args := map[string]string{} // tool call id -> arguments
	for _, m := range msgs {
		switch m.Role {
		case "user":
			if m.From == "compact" {
				if strings.HasPrefix(m.Content, summaryHead) {
					out = append(out, Event{T: "compacted", Text: strings.TrimPrefix(m.Content, summaryHead), Replay: true})
				}
				continue
			}
			out = append(out, Event{T: "user", From: m.From, Text: m.Content, Replay: true})
		case "assistant":
			if m.From == "compact" {
				continue
			}
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, Event{T: "text", Text: strings.TrimSpace(m.Content), Replay: true})
			}
			for _, tc := range m.ToolCalls {
				args[tc.ID] = tc.Function.Arguments
				out = append(out, Event{T: "tool", Tool: tc.Function.Name, Text: argSummary(tc.Function.Name, tc.Function.Arguments), Replay: true})
			}
		case "tool":
			ok := !strings.HasPrefix(m.Content, "error:") && !strings.Contains(m.Content, "[exit ") || strings.Contains(m.Content, "[exit 0]")
			out = append(out, Event{T: "result", Tool: m.Name, OK: ok, Text: firstLine(m.Content), Short: shortResult(m.Name, args[m.ToolCallID], m.Content, ok), Replay: true})
		}
	}
	return out
}

// SessionInfo is one row of the session list.
type SessionInfo struct {
	Name    string    `json:"name"`
	Updated time.Time `json:"updated"`
	Msgs    int       `json:"msgs"`
	Turns   int       `json:"turns"` // prompts from people or programs, not tool results
	Ctx     int       `json:"ctx"`   // tokens of the last request
	Running bool      `json:"running"`
	Step    int       `json:"step"`
	Waiting bool      `json:"waiting"`
	From    string    `json:"from,omitempty"` // who started the run in progress
	By      string    `json:"by,omitempty"`   // who sent the first message: "chat:PID", "opus", "qh run"
	Cwd     string    `json:"cwd"`
	First   string    `json:"first"` // the first user message
	Last    string    `json:"last"`  // the last user message
}

func (s *Server) sessionList() []SessionInfo {
	d, _ := qhDir()
	files, _ := filepath.Glob(filepath.Join(d, "sessions", "*.json"))
	var rows []SessionInfo
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		var ss Session
		if b, err := os.ReadFile(f); err == nil {
			json.Unmarshal(b, &ss)
		}
		r := SessionInfo{Name: strings.TrimSuffix(filepath.Base(f), ".json"), Updated: st.ModTime(), Msgs: len(ss.Msgs), Cwd: ss.Cwd, Ctx: ss.LastPrompt}
		if r.Ctx == 0 && len(ss.Msgs) > 0 {
			r.Ctx = estimateTokens(ss.Msgs)
		}
		s.mu.Lock()
		if live, ok := s.sessions[r.Name]; ok {
			r.Running, r.Step = live.state()
			r.Waiting = live.waitingNow()
			r.From = live.fromNow()
		}
		s.mu.Unlock()
		for _, m := range ss.Msgs {
			if m.Role == "user" && m.From != "compact" && m.From != "qh" {
				r.Turns++
			}
			if m.Role == "user" {
				if r.First == "" {
					r.By = m.From
					r.First = oneLine(m.Content, 200)
				}
				r.Last = oneLine(m.Content, 200)
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Updated.After(rows[j].Updated) })
	return rows
}

func (s *Server) listSessions(emit func(Event)) {
	var b strings.Builder
	for _, r := range s.sessionList() {
		state := ""
		if r.Running {
			state = fmt.Sprintf("  RUNNING step %d", r.Step)
			if r.Waiting {
				state = fmt.Sprintf("  WAITING for the model (step %d)", r.Step)
			}
			from := r.From
			if strings.HasPrefix(from, "chat:") {
				from = "a terminal chat"
			}
			if from != "" {
				state += " from " + from
			}
		}
		ctx := "ctx " + human(r.Ctx)
		if s.cfg.Context > 0 {
			ctx += fmt.Sprintf(" (%d%%)", r.Ctx*100/s.cfg.Context)
		}
		fmt.Fprintf(&b, "%-16s %s  %3d turns  %-16s %s%s\n", r.Name, r.Updated.Format("2006-01-02 15:04"), r.Turns, ctx, shortPath(r.Cwd), state)
	}
	emit(Event{T: "done", Text: strings.TrimRight(b.String(), "\n")})
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// checkVerdict is one line for the top of the report, so a reader sees what
// qh measured before what the model claims.
var blameRe = regexp.MustCompile(`(?i)[^.\n]*\b(test environment|environment(al)? (issue|problem)|not (a|due to a|caused by a) (code )?(defect|bug|issue)|the code (logic )?(itself )?is correct|infrastructure (issue|problem)|flaky|test (setup|harness|configuration) (issue|problem))\b[^.\n]*`)

// blameLine flags a report that blames the environment while checks failed.
// The model says this when it cannot find its bug, and it is usually wrong.
func blameLine(report string, checks []string) string {
	failed := false
	for _, c := range checks {
		if strings.HasPrefix(c, "qh check FAILED") {
			failed = true
		}
	}
	m := blameRe.FindString(report)
	if !failed || m == "" {
		return ""
	}
	return "\nqh: the report says \"" + oneLine(strings.TrimSpace(m), 160) + "\" while checks failed. That claim is not checked; assume a code bug until the failing assertion is explained."
}

func checkVerdict(lines []string) string {
	pass, fail, skip, info := 0, 0, 0, 0
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "qh check info"):
			info++
		case strings.HasPrefix(l, "qh check skipped"):
			skip++
		case strings.HasPrefix(l, "qh check passed"):
			pass++
		case strings.HasPrefix(l, "qh check FAILED"):
			fail++
		}
	}
	sk := ""
	if skip > 0 {
		sk = fmt.Sprintf("; %d not run, check them yourself", skip)
	}
	if info > 0 {
		sk += fmt.Sprintf("; %d only print state, read their output", info)
	}
	switch {
	case pass+fail == 0:
		return "qh: no Done-when checks ran" + sk + " (see the qh lines at the end)"
	case fail == 0:
		return fmt.Sprintf("qh: all %d Done-when checks passed after the run%s (trust this over the report below)", pass, sk)
	}
	return fmt.Sprintf("qh: %d of %d Done-when checks FAILED after the run%s (see the end of the report)", fail, pass+fail, sk)
}

// confine returns whether file tools stay in the project, and the extra
// folders they may read. The default read roots are the dependency caches,
// so a run can read library sources and jars.
func (c Config) confine() (bool, []string) {
	if c.Confine != nil && !*c.Confine {
		return false, nil
	}
	roots := c.ReadRoots
	if roots == nil {
		roots = []string{"~/.m2/repository", "~/.gradle/caches", "~/go/pkg/mod", "/usr/local/go/src"}
	}
	h, _ := os.UserHomeDir()
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if strings.HasPrefix(r, "~/") && h != "" {
			r = filepath.Join(h, r[2:])
		}
		out = append(out, filepath.Clean(r))
	}
	return true, out
}
