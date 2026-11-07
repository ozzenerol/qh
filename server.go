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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Config struct {
	URL         string `json:"url"`
	Model       string `json:"model"`
	MaxSteps    int    `json:"max_steps"`
	MaxTokens   int    `json:"max_tokens"`
	ThinkBudget int    `json:"think_budget"` // chars of thinking in one step
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
	c := Config{URL: "http://127.0.0.1:8080/v1", MaxSteps: 80, MaxTokens: 16384, ThinkBudget: 30000}
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
	Op      string `json:"op"` // run, set, reset, sessions, ping, stop
	Session string `json:"session,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
	New     bool   `json:"new,omitempty"`
	Think   *bool  `json:"think,omitempty"`
}

type Server struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*Session
	stop     chan struct{}
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
	s := &Server{cfg: loadConfig(), sessions: map[string]*Session{}, stop: make(chan struct{})}
	log.Printf("qh %s listening on %s, model server %s", version, sp, s.cfg.URL)
	go func() {
		<-s.stop
		ln.Close()
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
	ss := &Session{Name: name, Cwd: cwd, Think: true}
	if d, err := qhDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(d, "sessions", name+".json")); err == nil {
			json.Unmarshal(b, ss)
			ss.Name = name
		}
	}
	s.sessions[name] = ss
	return ss, nil
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

	switch req.Op {
	case "ping":
		emit(Event{T: "done", Text: version})
	case "stop":
		emit(Event{T: "done", Text: "stopping"})
		close(s.stop)
	case "sessions":
		s.listSessions(emit)
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
		ss.Msgs = nil
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
		if req.Think != nil {
			ss.mu.Lock()
			ss.Think = *req.Think
			s.save(ss)
			ss.mu.Unlock()
		}
		emit(Event{T: "done", Text: fmt.Sprintf("thinking %s for %s", onOff(ss.Think), req.Session)})
	case "run":
		s.run(c, r, req, emit)
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
	if !ss.mu.TryLock() {
		emit(Event{T: "error", Text: "session " + req.Session + " is busy"})
		return
	}
	defer ss.mu.Unlock()
	if req.New {
		ss.Msgs = nil
	}
	ss.Cwd = req.Cwd
	if req.Think != nil {
		ss.Think = *req.Think
	}

	ctx, cancel := context.WithCancel(context.Background())
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

	llm := &LLM{BaseURL: s.cfg.URL, Model: s.cfg.Model, HTTP: &http.Client{}}
	if err := llm.ResolveModel(ctx); err != nil {
		emit(Event{T: "error", Text: err.Error()})
		return
	}
	a := &Agent{LLM: llm, Cfg: s.cfg}
	st := a.Run(ctx, ss, req.Prompt, emit)
	s.save(ss)
	final := ""
	if n := len(ss.Msgs); n > 0 && ss.Msgs[n-1].Role == "assistant" {
		final = ss.Msgs[n-1].Content
	}
	emit(Event{T: "done", Text: strings.TrimSpace(final), Stats: &st})
}

func (s *Server) listSessions(emit func(Event)) {
	d, _ := qhDir()
	files, _ := filepath.Glob(filepath.Join(d, "sessions", "*.json"))
	type row struct {
		name string
		t    time.Time
		n    int
		cwd  string
	}
	var rows []row
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			continue
		}
		var ss Session
		if b, err := os.ReadFile(f); err == nil {
			json.Unmarshal(b, &ss)
		}
		rows = append(rows, row{strings.TrimSuffix(filepath.Base(f), ".json"), st.ModTime(), len(ss.Msgs), ss.Cwd})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].t.After(rows[j].t) })
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%-16s %s  %4d msgs  %s\n", r.name, r.t.Format("2006-01-02 15:04"), r.n, r.cwd)
	}
	emit(Event{T: "done", Text: strings.TrimRight(b.String(), "\n")})
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
