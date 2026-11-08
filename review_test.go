package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mixModel plays a run that uses every tool kind at once. Explore requests
// (their own system prompt) get an answer at once and are kept apart.
type mixModel struct {
	mu      sync.Mutex
	main    [][]json.RawMessage
	tools   []string
	explore int
	script  []string // tool name + "\x00" + arguments, one per main request
}

func (m *mixModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    json.RawMessage   `json:"tools"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	var sys Message
	json.Unmarshal(body.Messages[0], &sys)
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s) }
	defer func() {
		chunk(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5}}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.HasPrefix(sys.Content, "You answer one question") {
		m.explore++
		chunk(`{"choices":[{"delta":{"content":"x is in a.go:3"}}]}`)
		return
	}
	m.main = append(m.main, body.Messages)
	m.tools = append(m.tools, string(body.Tools))
	n := len(m.main)
	if n > len(m.script) {
		chunk(`{"choices":[{"delta":{"content":"done"}}]}`)
		return
	}
	name, args, _ := strings.Cut(m.script[n-1], "\x00")
	a, _ := json.Marshal(args)
	chunk(fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c%d","function":{"name":%q,"arguments":%s}}]}}]}`, n, name, a))
}

func TestMixedToolsPromptOnlyGrows(t *testing.T) {
	t.Setenv("QH_HOME", t.TempDir())
	dir := t.TempDir()
	fm := &mixModel{script: []string{
		"todo\x00" + `{"items":[{"text":"write a.go","status":"doing"}]}`,
		"bash\x00" + `{"command":"sleep 0.3; echo hi","background":true}`,
		"write\x00" + `{"path":"a.go","content":"package a\n\nvar x = 1\n"}`,
		"edit\x00" + `{"path":"a.go","old":"x = 1","new":"x = 2"}`,
		"explore\x00" + `{"question":"where is x"}`,
		"read\x00" + `{"path":"a.go"}`,
		"read\x00" + `{"path":"a.go"}`,
		"bg\x00" + `{"id":"1"}`,
	}}
	for i := 0; i < 14; i++ {
		fm.script = append(fm.script, "bash\x00"+fmt.Sprintf(`{"command":"echo %d"}`, i))
	}
	ts := httptest.NewServer(fm)
	defer ts.Close()
	cfg := Config{MaxSteps: 40, MaxTokens: 100, ThinkBudget: 30000}
	cfg.Diagnostics = map[string]string{".go": "echo {file}:3:5: fake error"}
	a := &Agent{LLM: &LLM{BaseURL: ts.URL, Model: "m", HTTP: ts.Client()}, Cfg: cfg, UndoDir: filepath.Join(t.TempDir(), "u")}
	s := &Session{Name: "mix", Cwd: dir, Think: true}
	var results []string
	st := a.Run(context.Background(), s, "do it", "you", func(e Event) {
		if e.T == "result" && !e.Sub {
			results = append(results, e.Tool+": "+e.Text)
		}
	})
	if fm.explore == 0 || st.Explores != 1 {
		t.Fatalf("explore did not run: %d requests, %d runs", fm.explore, st.Explores)
	}
	if want := len(fm.script) + 1; st.Steps != want {
		t.Fatalf("steps %d, want %d (explore steps must not count)", st.Steps, want)
	}
	for i := 1; i < len(fm.main); i++ {
		prev, cur := fm.main[i-1], fm.main[i]
		if len(cur) <= len(prev) {
			t.Fatalf("request %d has %d messages, the one before %d", i, len(cur), len(prev))
		}
		for j := range prev {
			if !bytes.Equal(prev[j], cur[j]) {
				t.Fatalf("request %d changes message %d:\nbefore %s\nafter  %s", i, j, prev[j], cur[j])
			}
		}
		if fm.tools[i] != fm.tools[i-1] {
			t.Fatalf("request %d changes the tools", i)
		}
	}
	all := strings.Join(results, "\n")
	for _, want := range []string{"edit: edited a.go", "read: unchanged since your read"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in results:\n%s", want, all)
		}
	}
	var msgs []Message
	for _, m := range s.Msgs {
		if m.Role == "tool" {
			msgs = append(msgs, m)
		}
	}
	if !strings.Contains(msgs[3].Content, "[qh: errors in a.go after this change]") {
		t.Errorf("edit result has no diagnostics: %s", msgs[3].Content)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.go")); !strings.Contains(string(b), "x = 2") {
		t.Fatalf("a.go: %s", b)
	}
	if runs := listRuns(a.UndoDir); len(runs) != 1 || len(runs[0].Files) != 1 {
		t.Fatalf("undo runs: %+v", runs)
	}
}

// A session saved by an older qh keeps its system prompt only while the
// prompt template and the tools are the same.
func TestKeptPromptRebuiltForNewTools(t *testing.T) {
	dir := t.TempDir()
	extra := (&Agent{}).systemExtra(false)
	s := &Session{Cwd: dir, System: "You are a coding agent. Your tools are read, write, edit, bash.",
		SysKey: dir + "\x00false\x00" + extra, Msgs: []Message{{Role: "user", Content: "hi"}}}
	if p := s.systemPrompt(extra); !strings.Contains(p, "todo") {
		t.Fatalf("old prompt kept: %s", p)
	}
	// The same build keeps it.
	s.System = s.System + " kept"
	if p := s.systemPrompt(extra); !strings.HasSuffix(p, " kept") {
		t.Fatal("prompt rebuilt with nothing changed")
	}
}

// Files edited through an argument alias or a path-less edit are in the
// run's changed set, so qh's formatting and import fixes reach them.
func TestChangedSetCoversAliases(t *testing.T) {
	t.Setenv("QH_HOME", t.TempDir())
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "A.kt"), []byte("class A {\n    val a = 1\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "B.kt"), []byte("class B {\n    val b = 1\n}\n"), 0o644)
	srv, _ := callModel(t, func(n int) (string, string) {
		switch n {
		case 1:
			return "edit", `{"file":"A.kt","old":"    val a = 1","new":"    val a = 1\nfun f() = 2"}`
		case 2:
			return "edit", `{"old":"    val b = 1","new":"    val b = 1\nfun g() = 2"}`
		}
		return "", ""
	})
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 10, MaxTokens: 100, ThinkBudget: 30000}}
	a.Run(context.Background(), &Session{Name: "c", Cwd: dir}, "add f and g", "", func(Event) {})
	for f, want := range map[string]string{"A.kt": "\n    fun f() = 2\n", "B.kt": "\n    fun g() = 2\n"} {
		if b, _ := os.ReadFile(filepath.Join(dir, f)); !strings.Contains(string(b), want) {
			t.Errorf("%s not indented by qh:\n%s", f, b)
		}
	}
}

// A background command that changes a tracked file is covered by undo.
func TestUndoCoversBackground(t *testing.T) {
	if lookTool("git") == "" {
		t.Skip("no git")
	}
	dir := gitRepo(t, map[string]string{"a.txt": "x\n"})
	root := filepath.Join(t.TempDir(), "u")
	srv, _ := callModel(t, func(n int) (string, string) {
		switch n {
		case 1:
			return "bash", `{"command":"sed -i s/x/y/ a.txt && echo new > n.txt","background":true}`
		case 2:
			return "bash", `{"command":"sleep 0.5"}`
		}
		return "", ""
	})
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 10, MaxTokens: 100, ThinkBudget: 30000}, UndoDir: root}
	a.Run(context.Background(), &Session{Name: "b", Cwd: dir}, "run it", "", func(Event) {})
	if readFile(t, filepath.Join(dir, "a.txt")) != "y\n" {
		t.Fatal("the background command did not run")
	}
	if out, err := undo(root, false); err != nil {
		t.Fatalf("undo: %v %s", err, out)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "x\n" {
		t.Errorf("a.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "n.txt")); err == nil {
		t.Error("n.txt not deleted")
	}
}

func TestEvalErrsKeepOldMeaning(t *testing.T) {
	m, _ := aggregate([]EvalResult{{Task: "a", ToolErrs: 1, CmdFails: 2}})
	if m["a"].errs != 3 {
		t.Fatalf("errs %d", m["a"].errs)
	}
}
