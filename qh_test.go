package main

import (
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
	"time"
)

func TestParseTextToolCalls(t *testing.T) {
	in := "I will look.\n<tool_call>\n<function=bash>\n<parameter=command>\nls -la\n</parameter>\n<parameter=timeout>\n30\n</parameter>\n</function>\n</tool_call>"
	text, calls := parseTextToolCalls(in)
	if text != "I will look." || len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("got %q %+v", text, calls)
	}
	var args map[string]any
	json.Unmarshal([]byte(calls[0].Function.Arguments), &args)
	if args["command"] != "ls -la" || args["timeout"] != 30.0 {
		t.Fatalf("args %v", args)
	}
}

func TestEdit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	os.WriteFile(p, []byte("func a() {   \n\treturn 1\n}\n\nfunc b() {\n\treturn 1\n}\n"), 0o644)
	tl := &Tools{Cwd: dir}
	ctx := context.Background()

	if out, ok := tl.Run(ctx, "edit", `{"path":"a.go","old":"\treturn 1","new":"\treturn 2"}`); ok || !strings.Contains(out, "2 times") {
		t.Fatalf("duplicate: %s", out)
	}
	// trailing spaces and read-tool line numbers in old still match
	if out, ok := tl.Run(ctx, "edit", `{"path":"a.go","old":"1\tfunc a() {\n2\t\treturn 1","new":"func a() {\n\treturn 3"}`); !ok {
		t.Fatalf("loose: %s", out)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "func a() {\n\treturn 3\n}") {
		t.Fatalf("file: %q", b)
	}
	if out, ok := tl.Run(ctx, "edit", `{"path":"a.go","old":"func c()","new":"x"}`); ok || !strings.Contains(out, "not found") {
		t.Fatalf("missing: %s", out)
	}
}

func TestReadAndBash(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(sb.String()), 0o644)
	tl := &Tools{Cwd: dir}
	out, _ := tl.Run(context.Background(), "read", `{"path":"big.txt","offset":"995"}`)
	if !strings.HasPrefix(out, "995\tline 995\n") || strings.Contains(out, "offset=") {
		t.Fatalf("read tail: %q", out)
	}
	out, _ = tl.Run(context.Background(), "read", `{"path":"big.txt"}`)
	if !strings.Contains(out, "use offset=401") {
		t.Fatalf("read head: %q", out[len(out)-80:])
	}
	out, ok := tl.Run(context.Background(), "bash", `{"command":"echo hi; exit 3"}`)
	if ok || out != "hi\n[exit 3]" {
		t.Fatalf("bash: %q", out)
	}
	out, _ = tl.Run(context.Background(), "bash", `{"command":"seq 1 100000"}`)
	if len(out) > maxToolOut+200 || !strings.Contains(out, "lines cut") || !strings.Contains(out, "100000") {
		t.Fatalf("clip: %d", len(out))
	}
}

func TestLooping(t *testing.T) {
	if looping(strings.Repeat("abcdefghij ", 100)[:900] + "unique ending that is long enough to not repeat anywhere else in the text at all ok") {
		t.Fatal("false positive")
	}
	if !looping(strings.Repeat("Wait, let me check the parser again. Actually no. ", 40)) {
		t.Fatal("missed loop")
	}
}

// fakeModel streams one tool call, then a final answer, and records requests.
type fakeModel struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		fmt.Fprint(w, `{"data":[{"id":"/m/Qwen-test"}]}`)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.reqs = append(f.reqs, body)
	n := len(f.reqs)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s) }
	if n == 1 {
		chunk(`{"choices":[{"delta":{"reasoning":"I should list files."}}]}`)
		chunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{\"command\":"}}]}}]}`)
		chunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"echo ok\"}"}}]}}]}`)
	} else {
		chunk(`{"choices":[{"delta":{"content":"Done. ran echo."}}]}`)
	}
	chunk(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":90}}}`)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestAgentLoop(t *testing.T) {
	fm := &fakeModel{}
	ts := httptest.NewServer(fm)
	defer ts.Close()
	llm := &LLM{BaseURL: ts.URL + "/v1", HTTP: ts.Client()}
	if err := llm.ResolveModel(context.Background()); err != nil || llm.Model != "/m/Qwen-test" {
		t.Fatalf("model %q %v", llm.Model, err)
	}
	a := &Agent{LLM: llm, Cfg: Config{MaxSteps: 5, MaxTokens: 100, ThinkBudget: 1000}}
	s := &Session{Cwd: t.TempDir(), Think: true}
	var events []string
	st := a.Run(context.Background(), s, "list files", "test", func(e Event) { events = append(events, e.T+":"+e.Tool) })
	if st.Steps != 2 || st.Output != 14 {
		t.Fatalf("stats %+v", st)
	}
	last := s.Msgs[len(s.Msgs)-1]
	if last.Content != "Done. ran echo." {
		t.Fatalf("final %q", last.Content)
	}
	if s.Msgs[2].Role != "tool" || s.Msgs[2].Content != "ok\n[exit 0]" {
		t.Fatalf("tool msg %+v", s.Msgs[2])
	}
	// The second request carries the first answer's thinking as reasoning_content.
	msgs := fm.reqs[1]["messages"].([]any)
	asst := msgs[2].(map[string]any)
	if asst["reasoning_content"] != "I should list files." {
		t.Fatalf("reasoning not sent back: %v", asst)
	}
	kw := fm.reqs[1]["chat_template_kwargs"].(map[string]any)
	if kw["preserve_thinking"] != true || kw["enable_thinking"] != true {
		t.Fatalf("kwargs %v", kw)
	}
	if !strings.Contains(strings.Join(events, " "), "tool:bash") {
		t.Fatalf("events %v", events)
	}
}

func TestDaemonSocket(t *testing.T) {
	fm := &fakeModel{}
	ts := httptest.NewServer(fm)
	defer ts.Close()
	home := t.TempDir()
	t.Setenv("QH_HOME", home)
	t.Setenv("QH_URL", ts.URL+"/v1")
	done := make(chan error, 1)
	go func() { done <- serve() }()
	cwd := t.TempDir()
	var c interface{ Close() error }
	var dec *json.Decoder
	var err error
	for i := 0; i < 50; i++ {
		if _, err = os.Stat(sockPath()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	c, dec, err = send(ClientReq{Op: "run", Session: "t1", Cwd: cwd, Prompt: "list files"})
	if err != nil {
		t.Fatal(err)
	}
	var final Event
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		if e.T == "done" {
			final = e
			break
		}
	}
	c.Close()
	if final.Text != "Done. ran echo." || final.Stats == nil || final.Stats.Steps != 2 {
		t.Fatalf("final %+v", final)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions", "t1.json")); err != nil {
		t.Fatal("session not saved")
	}
	if err := simple(ClientReq{Op: "stop"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyTools(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "a.go"), []byte("package a\nfunc Parse() {}\n"), 0o644)
	tl := &Tools{Cwd: dir, ReadOnly: true}
	ctx := context.Background()
	if out, ok := tl.Run(ctx, "bash", `{"command":"touch x"}`); ok || !strings.Contains(out, "read-only") {
		t.Fatalf("bash allowed: %s", out)
	}
	if out, ok := tl.Run(ctx, "grep", `{"pattern":"func Pa[a-z]+","glob":"*.go"}`); !ok || !strings.Contains(out, "src/a.go:2:func Parse") {
		t.Fatalf("grep: %q", out)
	}
	if out, _ := tl.Run(ctx, "grep", `{"pattern":"nothing here"}`); out != "no matches" {
		t.Fatalf("grep none: %q", out)
	}
	if out, ok := tl.Run(ctx, "ls", `{}`); !ok || out != "src/\n" {
		t.Fatalf("ls: %q", out)
	}
	if len(toolSchemas(true)) != 3 || len(toolSchemas(false)) != 4 {
		t.Fatal("schemas")
	}
}

// slowModel answers after a delay, or until the request is cancelled.
type slowModel struct{ delay time.Duration }

func (m slowModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		fmt.Fprint(w, `{"data":[{"id":"/m/slow"}]}`)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	w.Header().Set("Content-Type", "text/event-stream")
	select {
	case <-time.After(m.delay):
	case <-r.Context().Done():
		return
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "echo: " + last}}}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
}

func TestAttachQueueCancel(t *testing.T) {
	ts := httptest.NewServer(slowModel{delay: 300 * time.Millisecond})
	defer ts.Close()
	t.Setenv("QH_HOME", t.TempDir())
	t.Setenv("QH_URL", ts.URL+"/v1")
	done := make(chan error, 1)
	go func() { done <- serve() }()
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sockPath()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cwd := t.TempDir()

	// a chat attaches to the session
	ac, adec, err := send(ClientReq{Op: "attach", Session: "s", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	var e Event
	if adec.Decode(&e); e.T != "attached" {
		t.Fatalf("first event %+v", e)
	}
	got := make(chan Event, 100)
	go func() {
		for {
			var e Event
			if adec.Decode(&e) != nil {
				return
			}
			got <- e
		}
	}()

	// opus sends one run and, while it runs, a second one; the chat sends a third detached
	res := make(chan string, 2)
	for _, p := range []string{"one", "two"} {
		go func(p string) {
			c, dec, err := send(ClientReq{Op: "run", Session: "s", Cwd: cwd, Prompt: p, From: "opus"})
			if err != nil {
				res <- err.Error()
				return
			}
			defer c.Close()
			for {
				var e Event
				if dec.Decode(&e) != nil {
					res <- "eof"
					return
				}
				if e.T == "done" {
					res <- e.Text
					return
				}
			}
		}(p)
		time.Sleep(50 * time.Millisecond)
	}
	r1, r2 := <-res, <-res
	if r1 != "echo: one" || r2 != "echo: two" {
		t.Fatalf("results %q %q", r1, r2)
	}
	var seen []string
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case e := <-got:
			seen = append(seen, e.T+":"+e.From+":"+e.Text)
			if e.T == "done" && e.Text == "echo: two" {
				break collect
			}
		case <-timeout:
			t.Fatalf("watcher saw %v", seen)
		}
	}
	want := []string{"user:opus:one", "queued:opus:two", "text::echo: one", "done:opus:echo: one", "user:opus:two", "text::echo: two", "done:opus:echo: two"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("watcher saw\n%v\nwant\n%v", seen, want)
	}

	// a detached run can be cancelled with the cancel op
	ts.Config.Handler = slowModel{delay: 5 * time.Second}
	if err := simpleQuiet(ClientReq{Op: "run", Session: "s", Cwd: cwd, Prompt: "long", From: "chat:1", Detach: true}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := simpleQuiet(ClientReq{Op: "cancel", Session: "s"}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case e := <-got:
			if e.T == "done" {
				if time.Since(start) > time.Second {
					t.Fatal("cancel was slow")
				}
				simple(ClientReq{Op: "stop"})
				<-done
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no done after cancel")
		}
	}
}

func TestWrapAndVisible(t *testing.T) {
	s := inline("a **bold** word and `code` here, then more words to wrap around", "")
	for _, l := range wrap(s, 20) {
		if visible(l) > 20 {
			t.Fatalf("line too long: %q", l)
		}
	}
	if visible(cBold+"héllo"+cReset) != 5 {
		t.Fatal("visible")
	}
}
