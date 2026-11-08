package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

	if out, ok := tl.Run(ctx, "edit", `{"path":"a.go","old":"\treturn 1","new":"\treturn 2"}`); ok || !strings.Contains(out, "2 times") || !strings.Contains(out, "5\tfunc b() {") {
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
	if !strings.Contains(out, "use offset=201") {
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
	if len(toolSchemas(true)) != 3 || len(toolSchemas(false)) != 9 {
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

// compactModel: a tool call with a big prompt, then a summary, then an answer.
type compactModel struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (m *compactModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		fmt.Fprint(w, `{"data":[{"id":"/m/c"}]}`)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.reqs = append(m.reqs, body)
	m.mu.Unlock()
	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	chunk := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s) }
	switch {
	case strings.HasPrefix(fmt.Sprint(last["content"]), "Stop the task for a moment"):
		chunk(`{"choices":[{"delta":{"content":"Goal: list files. Done: ran ls."}}]}`)
	case last["role"] == "user" && fmt.Sprint(last["content"]) == "list files":
		chunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash","arguments":"{\"command\":\"echo hi\"}"}}]}}]}`)
		chunk(`{"choices":[],"usage":{"prompt_tokens":900,"completion_tokens":10}}`)
	default:
		chunk(`{"choices":[{"delta":{"content":"all done"}}]}`)
		chunk(`{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":3}}`)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestAutoCompact(t *testing.T) {
	fm := &compactModel{}
	ts := httptest.NewServer(fm)
	defer ts.Close()
	home := t.TempDir()
	t.Setenv("QH_HOME", home)
	llm := &LLM{BaseURL: ts.URL + "/v1", HTTP: ts.Client()}
	llm.ResolveModel(context.Background())
	a := &Agent{LLM: llm, Cfg: Config{MaxSteps: 5, MaxTokens: 100, ThinkBudget: 1000, Context: 1000, CompactAt: 500}}
	s := &Session{Name: "c1", Cwd: t.TempDir()}
	var evs []string
	a.Run(context.Background(), s, "list files", "you", func(e Event) {
		if e.T == "compacting" || e.T == "compacted" || e.T == "tool" {
			evs = append(evs, e.T)
		}
	})
	if strings.Join(evs, ",") != "tool,compacting,compacted" {
		t.Fatalf("events %v", evs)
	}
	if !strings.HasPrefix(s.Msgs[0].Content, summaryHead+"Goal: list files.") || s.Msgs[len(s.Msgs)-1].Content != "all done" {
		t.Fatalf("msgs %+v", s.Msgs)
	}
	// the summary request starts with the same system prompt and tools as the steps
	sys0 := fm.reqs[0]["messages"].([]any)[0].(map[string]any)["content"]
	sys1 := fm.reqs[1]["messages"].([]any)[0].(map[string]any)["content"]
	if sys0 != sys1 || fmt.Sprint(fm.reqs[0]["tools"]) != fmt.Sprint(fm.reqs[1]["tools"]) {
		t.Fatal("the compaction request does not share the prefix")
	}
	if files, _ := filepath.Glob(filepath.Join(home, "sessions", "archive", "c1-*.json")); len(files) != 1 {
		t.Fatal("no archive")
	}
	if got := replay(s.Msgs); got[0].T != "compacted" {
		t.Fatalf("replay %+v", got[0])
	}
}

func TestAtChat(t *testing.T) {
	ts := httptest.NewServer(slowModel{delay: 10 * time.Millisecond})
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
	if err := simpleQuiet(ClientReq{Op: "run", Session: "@chat", Cwd: cwd, Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "no chat") {
		t.Fatalf("want no chat error, got %v", err)
	}
	ac, adec, _ := send(ClientReq{Op: "attach", Session: "1108-120000", Cwd: cwd, From: "chat:42"})
	var e Event
	adec.Decode(&e)
	c, dec, _ := send(ClientReq{Op: "run", Session: "@chat", Cwd: cwd, Prompt: "hello", From: "opus"})
	for dec.Decode(&e) == nil && e.T != "done" {
	}
	c.Close()
	for adec.Decode(&e) == nil && e.T != "user" {
	}
	if e.From != "opus" || e.Text != "hello" {
		t.Fatalf("chat got %+v", e)
	}
	ac.Close()
	simple(ClientReq{Op: "stop"})
	<-done
}

func TestEditHelpers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.yml"), []byte("spring:\n  name: a\n  port: 1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.yml"), []byte("spring:\n  name: b\n"), 0o644)
	tl := &Tools{Cwd: dir}
	ctx := context.Background()
	// no path, old in one file: qh uses that file
	if out, ok := tl.Run(ctx, "edit", `{"old":"  port: 1","new":"  port: 2"}`); !ok || !strings.Contains(out, "a.yml") {
		t.Fatalf("no path: %s", out)
	}
	// no path, old in two files: error that lists both
	if out, ok := tl.Run(ctx, "edit", `{"old":"spring:","new":"x"}`); ok || !strings.Contains(out, "a.yml") || !strings.Contains(out, "b.yml") {
		t.Fatalf("two files: %s", out)
	}
	// other names for the fields, and edit with content is a write
	if out, ok := tl.Run(ctx, "edit", `{"file_path":"b.yml","old_string":"name: b","new_string":"name: c"}`); !ok {
		t.Fatalf("aliases: %s", out)
	}
	if out, ok := tl.Run(ctx, "edit", `{"path":"c.txt","content":"hi\n"}`); !ok || !strings.Contains(out, "wrote c.txt") {
		t.Fatalf("content: %s", out)
	}
	// old not found: the closest lines come back with numbers
	out, ok := tl.Run(ctx, "edit", `{"path":"a.yml","old":"spring:\n  name: a\n  port: 9","new":"x"}`)
	if ok || !strings.Contains(out, "2 of the 3 lines") || !strings.Contains(out, "3\t  port: 2") {
		t.Fatalf("nearest: %s", out)
	}
}

func TestDoneWhen(t *testing.T) {
	p := "Fix the parser.\n\nDone when: `./mvnw -q test -Dtest=FooTest` passes and `the docs` say so.\nAlso `go test ./...`\n\nNotes: `make lint`"
	got := checkCommands(p)
	if len(got) != 2 || got[0] != "./mvnw -q test -Dtest=FooTest" || got[1] != "go test ./..." {
		t.Fatalf("checks: %q", got)
	}
	if doneWhen("no criteria here") != "" {
		t.Fatal("doneWhen found a part that is not there")
	}
}

func TestSelfCheckAndChecks(t *testing.T) {
	dir := t.TempDir()
	var asks []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		asks = append(asks, body.Messages[len(body.Messages)-1].Content)
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"report %d\"}}]}\n\ndata: [DONE]\n\n", n)
	}))
	defer srv.Close()
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 10, MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Name: "t", Cwd: dir}
	st := a.Run(context.Background(), s, "Do it.\n\nDone when: `bash -c 'exit 1'` passes", "", func(Event) {})
	// 1 answer, 2 self-check, 3 after the failed check; the check runs again
	// after the fix turn and its line goes in the report
	if n != 3 || !strings.Contains(asks[1], "Before you report") || !strings.Contains(asks[2], "these failed") {
		t.Fatalf("n=%d asks=%q", n, asks)
	}
	if len(st.Checks) != 1 || !strings.Contains(st.Checks[0], "FAILED") {
		t.Fatalf("checks after the retry: %q", st.Checks)
	}
	if last := s.Msgs[len(s.Msgs)-1]; last.Content != "report 3" {
		t.Fatalf("last: %q", last.Content)
	}
}

func TestFailLine(t *testing.T) {
	out := "Calculating task graph as no cached configuration is available\nReusing configuration cache.\ne: file.kt:3:5 Named arguments are prohibited for non-Kotlin functions\nBUILD FAILED in 4s\n[exit 1]"
	if got := failLine(out); !strings.Contains(got, "Named arguments") || !strings.HasSuffix(got, "[exit 1]") {
		t.Fatalf("failLine: %q", got)
	}
	if got := failLine("one\ntwo\n[exit 2]"); got != "two [exit 2]" {
		t.Fatalf("failLine: %q", got)
	}
}

func TestJUnitFailures(t *testing.T) {
	dir := t.TempDir()
	rep := filepath.Join(dir, "svc", "build", "test-results", "test")
	os.MkdirAll(rep, 0o755)
	tl := &Tools{Cwd: dir}
	xmlText := `<?xml version="1.0"?><testsuite name="FooTest"><testcase name="ok" classname="a.FooTest"/>` +
		`<testcase name="bad" classname="a.FooTest"><failure message="expected 1 but was 2">org.opentest4j.AssertionFailedError: expected 1 but was 2
	at a.FooTest.bad(FooTest.kt:12)
	at java.base/java.lang.reflect.Method.invoke(Method.java:580)</failure></testcase></testsuite>`
	cmd := fmt.Sprintf("cat > %s/TEST-a.FooTest.xml <<'X'\n%s\nX\nexit 1", rep, xmlText)
	args, _ := json.Marshal(map[string]any{"command": cmd})
	out, ok := tl.Run(context.Background(), "bash", string(args))
	if ok || !strings.Contains(out, "FAILED a.FooTest.bad: expected 1 but was 2") || !strings.Contains(out, "FooTest.kt:12") || strings.Contains(out, "a.FooTest.ok") {
		t.Fatalf("junit: %s", out)
	}
}

func TestStepLimit(t *testing.T) {
	dir := t.TempDir()
	n := 0
	var last []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		last = append(last, body.Messages[len(body.Messages)-1].Content)
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= 6 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c%d\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"echo %d\\\"}\"}}]}}]}\n\ndata: [DONE]\n\n", n, n)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial report\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 6, MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Name: "t", Cwd: dir}
	st := a.Run(context.Background(), s, "loop", "", func(Event) {})
	if !st.Limit || n != 7 || !strings.Contains(last[6], "step limit") || !strings.Contains(last[1], "5 steps left") {
		t.Fatalf("limit=%v n=%d last=%q", st.Limit, n, last)
	}
	if m := s.Msgs[len(s.Msgs)-1]; m.Content != "partial report" || len(m.ToolCalls) != 0 {
		t.Fatalf("report: %+v", m)
	}
}

func TestWarningLines(t *testing.T) {
	out := "> Task :compileKotlin\nw: file:///a/B.kt:3:8 Unused import directive\nw: file:///a/C.kt:9:5 Variable 'x' is never used\nw: file:///a/D.kt:1:1 'foo' is deprecated\nBUILD SUCCESSFUL"
	if w := warningLines(out, nil); len(w) != 2 {
		t.Fatalf("warnings: %q", w)
	}
	jvm := "WARNING: sun.misc.Unsafe::allocateMemory has been called by io.netty.util.internal.PlatformDependent0\nWARNING: Please consider reporting this\nA.java:3: warning: [rawtypes] found raw type\nw: file:///x/Gen.kt:1:1 Unchecked cast"
	if w := warningLines(jvm, nil); len(w) != 2 {
		t.Fatalf("jvm noise counted: %q", w)
	}
	if w := warningLines(jvm, []string{"Gen\\.kt"}); len(w) != 1 {
		t.Fatalf("ignore list: %q", w)
	}
	if !noWarnings.MatchString("Done when: build passes with no unused imports") || noWarnings.MatchString("Done when: tests pass") {
		t.Fatal("noWarnings")
	}
}

func TestNearestFuzzyAndWriteDiff(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "R.kt")
	os.WriteFile(p, []byte("class R {\n    \"verifiedAt\" to user.verifiedAt?.toOffset(),\n    \"blockedAt\" to user.blockedAt?.toOffset(),\n}\n"), 0o644)
	tl := &Tools{Cwd: dir}
	ctx := context.Background()
	out, ok := tl.Run(ctx, "edit", `{"path":"R.kt","old":"\"verifiedAt\" to user.verifiedAt.toOffset(),\n\"blockedAt\" to user.blockedAt.toOffset(),","new":"x"}`)
	if ok || !strings.Contains(out, "look like it") || !strings.Contains(out, "2\t") {
		t.Fatalf("fuzzy: %s", out)
	}
	tl.Run(ctx, "read", `{"path":"R.kt"}`)
	out, _ = tl.Run(ctx, "write", `{"path":"R.kt","content":"class R {\n    \"verifiedAt\" to 1,\n    \"blockedAt\" to user.blockedAt?.toOffset(),\n}\n"}`)
	if !strings.Contains(out, "1 lines added, 1 removed") {
		t.Fatalf("diff: %s", out)
	}
	if out, _ := tl.Run(ctx, "write", `{"path":"N.kt","content":"x"}`); !strings.Contains(out, "new file") {
		t.Fatalf("new: %s", out)
	}
}

func TestJUnitOnPipedTest(t *testing.T) {
	dir := t.TempDir()
	rep := filepath.Join(dir, "build", "test-results", "test")
	os.MkdirAll(rep, 0o755)
	tl := &Tools{Cwd: dir}
	x := `<testsuite><testcase name="bad" classname="a.T"><failure message="boom">e
	at a.T.bad(T.kt:3)</failure></testcase></testsuite>`
	cmd := fmt.Sprintf("echo './gradlew test' >/dev/null; cat > %s/TEST-a.T.xml <<'X'\n%s\nX\necho BUILD FAILED | tail -1", rep, x)
	args, _ := json.Marshal(map[string]any{"command": cmd})
	out, ok := tl.Run(context.Background(), "bash", string(args))
	if !ok || !strings.Contains(out, "FAILED a.T.bad: boom") {
		t.Fatalf("piped: ok=%v %s", ok, out)
	}
}

func TestUnusedImports(t *testing.T) {
	src := `package a

import java.time.Instant
import java.time.ZoneOffset
import org.x.toOffset
import kotlin.properties.getValue
import org.y.Thing as Other
import org.z.*

class A(val at: Instant) {
	val b by lazy { at.toOffset() }
	val c: Other? = null
}
`
	got := unusedImports("A.kt", src)
	if len(got) != 1 || got[0] != "java.time.ZoneOffset" {
		t.Fatalf("unused: %q", got)
	}
	if unusedImports("a.go", src) != nil {
		t.Fatal("go files are the compiler's job")
	}
}

func TestRepoNotes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("QH_HOME", home)
	repo := filepath.Join(t.TempDir(), "myrepo")
	sub := filepath.Join(repo, "svc")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	os.MkdirAll(sub, 0o755)
	os.MkdirAll(filepath.Join(home, "notes"), 0o755)
	os.WriteFile(filepath.Join(home, "notes", "myrepo.md"), []byte("JdbcClient: sql(...).param(...)"), 0o644)
	if !strings.Contains(buildSystem(sub, false), "JdbcClient: sql") {
		t.Fatal("repo notes not loaded")
	}
}

func TestGrepLineHintDidYouMean(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "common", "id"), 0o755)
	os.WriteFile(filepath.Join(dir, "common", "id", "UuidV7.kt"), []byte("a\nx = 1\nb\nx = 1\n"), 0o644)
	tl := &Tools{Cwd: dir}
	ctx := context.Background()
	if out, ok := tl.Run(ctx, "bash", `{"command":"grep nothing-here common/id/UuidV7.kt"}`); !ok || !strings.Contains(out, "no matches") {
		t.Fatalf("grep: ok=%v %s", ok, out)
	}
	if out, ok := tl.Run(ctx, "bash", `{"command":"exit 1"}`); ok {
		t.Fatalf("plain exit 1 must fail: %s", out)
	}
	if out, ok := tl.Run(ctx, "edit", `{"path":"common/id/UuidV7.kt","old":"x = 1","new":"x = 2","line":4}`); !ok || !strings.Contains(out, "line 4") {
		t.Fatalf("line hint: %s", out)
	}
	if out, ok := tl.Run(ctx, "read", `{"path":"auth/id/UuidV7.kt"}`); !ok || !strings.Contains(out, "This is common/id/UuidV7.kt") {
		t.Fatalf("one candidate: %s", out)
	}
	os.MkdirAll(filepath.Join(dir, "other"), 0o755)
	os.WriteFile(filepath.Join(dir, "other", "UuidV7.kt"), nil, 0o644)
	if out, ok := tl.Run(ctx, "read", `{"path":"auth/id/UuidV7.kt"}`); ok || !strings.Contains(out, "did you mean:") {
		t.Fatalf("two candidates: %s", out)
	}
}

func TestFailLineGradleOrder(t *testing.T) {
	out := "> Task :common:compileTestKotlin FAILED\ne: file:///a/MailTest.kt:12:5 Unresolved reference: subject\nFAILURE: Build failed with an exception.\n[exit 1]"
	if got := failLine(out); !strings.Contains(got, "Unresolved reference") {
		t.Fatalf("failLine: %q", got)
	}
}

func TestCompilerErrors(t *testing.T) {
	out := "> Task :x:compileTestKotlin FAILED\ne: file:///repo/svc/src/A.kt:66:60 Unresolved reference 'any'.\ne: file:///repo/svc/src/A.kt:70:1 Unresolved reference 'every'.\nsrc/B.java:3: error: ';' expected\nFAILURE: Build failed"
	got := compilerErrors(out, "/repo")
	if len(got) != 3 || got[0] != "svc/src/A.kt:66:60 Unresolved reference 'any'." || !strings.HasPrefix(got[2], "src/B.java:3: error") {
		t.Fatalf("errors: %q", got)
	}
}

func TestLibraryDigNudge(t *testing.T) {
	dir := t.TempDir()
	n := 0
	var last []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		last = append(last, body.Messages[len(body.Messages)-1].Content)
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		cmd := `echo 'e: file:///x/A.kt:1:2 Unresolved reference' ; exit 1`
		if n > 1 {
			cmd = `echo unzip -p ~/.gradle/caches/mockk-sources.jar`
		}
		if n <= 4 {
			args, _ := json.Marshal(map[string]string{"command": cmd})
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": fmt.Sprint("c", n), "function": map[string]any{"name": "bash", "arguments": string(args)}}}}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", call)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 20, MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Name: "t", Cwd: dir}
	a.Run(context.Background(), s, "go", "", func(Event) {})
	if !strings.Contains(last[4], "stop exploring the library") || !strings.Contains(last[4], "A.kt:1:2 Unresolved reference") {
		t.Fatalf("nudge: %q", last[4])
	}
	if strings.Contains(last[3], "stop exploring") {
		t.Fatal("nudge came too early")
	}
}

func TestPatternHint(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.kt"), []byte("every { any() }\n"), 0o644)
	tl := &Tools{Cwd: dir}
	out, ok := tl.Run(context.Background(), "bash", `{"command":"grep -E 'any(' a.kt"}`)
	if ok || !strings.Contains(out, "grep -F") {
		t.Fatalf("bash hint: %s", out)
	}
	tl.ReadOnly = true
	if out, _ := tl.Run(context.Background(), "grep", `{"pattern":"any("}`); !strings.Contains(out, "literal") {
		t.Fatalf("grep tool hint: %s", out)
	}
}

func TestFindTool(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b", "X.kt"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "a", "Y.java"), nil, 0o644)
	tl := &Tools{Cwd: dir}
	if out, ok := tl.Run(context.Background(), "find", `{"pattern":"**/*.kt"}`); !ok || out != "a/b/X.kt" {
		t.Fatalf("find: %q", out)
	}
	if out, _ := tl.Run(context.Background(), "glob", `{"name":"*.go"}`); out != "no matches" {
		t.Fatalf("glob: %q", out)
	}
}

func TestLimitReportRetryAndDigBlock(t *testing.T) {
	dir := t.TempDir()
	n := 0
	var last []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		last = append(last, body.Messages[len(body.Messages)-1].Content)
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= 9 { // 8 dig steps (the 7th and 8th are blocked), then the first report try is a tool call too
			args, _ := json.Marshal(map[string]string{"command": fmt.Sprintf("echo %d; ls ~/.gradle/caches", n)})
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": fmt.Sprint("c", n), "function": map[string]any{"name": "bash", "arguments": string(args)}}}}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", call)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"the report\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 8, MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Name: "t", Cwd: dir}
	st := a.Run(context.Background(), s, "go", "", func(Event) {})
	all := strings.Join(last, "\n----\n")
	if strings.Count(all, "compile it") < 2 || !strings.Contains(all, "are blocked") || !strings.Contains(all, "blocks digging") || !strings.Contains(all, "tools are off after the step limit") {
		t.Fatalf("nudges: %q", last)
	}
	if !st.Limit || s.Msgs[len(s.Msgs)-1].Content != "the report" {
		t.Fatalf("report: %+v", s.Msgs[len(s.Msgs)-1])
	}
}

func TestSortImports(t *testing.T) {
	src := "package a\n\nimport org.slf4j.MDC\nimport java.time.Instant\nimport org.slf4j.LoggerFactory\nimport kotlin.math.max\nimport org.x.Y as Z\nimport com.b.C\nimport org.slf4j.MDC\n\nclass A\n"
	want := "package a\n\nimport com.b.C\nimport org.slf4j.LoggerFactory\nimport org.slf4j.MDC\nimport java.time.Instant\nimport kotlin.math.max\nimport org.x.Y as Z\n\nclass A\n"
	if got := sortImports("A.kt", src); got != want {
		t.Fatalf("kotlin:\n%s", got)
	}
	j := "package a;\n\nimport static org.junit.Assert.assertTrue;\nimport java.util.List;\nimport com.b.C;\n\nclass A {}\n"
	wantJ := "package a;\n\nimport com.b.C;\nimport java.util.List;\n\nimport static org.junit.Assert.assertTrue;\n\nclass A {}\n"
	if got := sortImports("A.java", j); got != wantJ {
		t.Fatalf("java:\n%s", got)
	}
	if c := "import b.B // keep\nimport a.A\n"; sortImports("A.kt", c) != c {
		t.Fatal("a block with comments must stay")
	}
}

func TestFormatConfig(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "myrepo", ".git"), 0o755)
	f := filepath.Join(dir, "myrepo", "A.kt")
	os.WriteFile(f, []byte("import b.B\nimport a.A\n\nclass A\n"), 0o644)
	on := true
	a := &Agent{Cfg: Config{Repos: map[string]FormatCfg{"myrepo": {SortImports: &on, Format: map[string]string{".kt": "echo formatted >>"}}}}}
	st := &Stats{}
	a.format(context.Background(), &Tools{Cwd: filepath.Join(dir, "myrepo")}, map[string]bool{f: true}, st, func(Event) {})
	b, _ := os.ReadFile(f)
	if string(b) != "import a.A\nimport b.B\n\nclass A\nformatted\n" || len(st.Checks) != 2 {
		t.Fatalf("file %q checks %q", b, st.Checks)
	}
}

func TestFindDecl(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "err"), 0o755)
	os.WriteFile(filepath.Join(dir, "err", "Errors.kt"), []byte("package e\n\nopen class BadRequestException : RuntimeException()\n"), 0o644)
	tl := &Tools{Cwd: dir}
	out, _ := tl.Run(context.Background(), "read", `{"path":"err/BadRequestException.kt"}`)
	if !strings.Contains(out, "This is err/Errors.kt, the file that declares BadRequestException (line 3)") || !strings.Contains(out, "open class BadRequestException") {
		t.Fatalf("decl: %s", out)
	}
}

func TestPassGrace(t *testing.T) {
	dir := t.TempDir()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= 2 {
			args, _ := json.Marshal(map[string]string{"command": "echo hi"})
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": fmt.Sprint("c", n), "function": map[string]any{"name": "bash", "arguments": string(args)}}}}}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", call)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"report\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 3, MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Name: "t", Cwd: dir}
	// the answer comes at step 3 of 3; the self-check still gets its step
	st := a.Run(context.Background(), s, "Do it.\n\nDone when: it works", "", func(Event) {})
	if st.Limit || n != 4 {
		t.Fatalf("limit=%v n=%d", st.Limit, n)
	}
}

func TestCheckVerdict(t *testing.T) {
	if v := checkVerdict([]string{"qh check passed: a", "qh check passed with 1 warnings: b", "qh note: x"}); !strings.Contains(v, "all 2") {
		t.Fatal(v)
	}
	if v := checkVerdict([]string{"qh check passed: a", "qh check FAILED: b"}); !strings.Contains(v, "1 of 2") {
		t.Fatal(v)
	}
}

func TestRemoveUnusedImports(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "A.kt")
	os.WriteFile(f, []byte("package a\n\nimport java.time.Instant\nimport java.time.ZoneOffset\nimport org.y.Thing as Other\n\nclass A(val at: Instant)\n"), 0o644)
	st := &Stats{}
	removeUnusedImports(dir, map[string]bool{f: true}, st, func(Event) {})
	b, _ := os.ReadFile(f)
	if string(b) != "package a\n\nimport java.time.Instant\nimport org.y.Thing as Other\n\nclass A(val at: Instant)\n" || len(st.Checks) != 1 {
		t.Fatalf("file %q checks %q", b, st.Checks)
	}
}

func TestSourceRoots(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"common/build.gradle.kts", "common/src/main/kotlin/a/A.kt", "services/auth/src/main/kotlin/b/B.kt",
		"services/auth/src/test/kotlin/b/BTest.kt", "services/auth/build.gradle.kts", "build.gradle.kts"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	g := exec.Command("sh", "-c", "git init -q && git add -A")
	g.Dir = dir
	if err := g.Run(); err != nil {
		t.Skip("no git")
	}
	got := strings.Join(sourceRoots(filepath.Join(dir, "services")), "\n")
	for _, w := range []string{"common/ (module)", "common/src/main/kotlin/", "services/auth/src/main/kotlin/", "services/auth/src/test/kotlin/"} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
}

func TestNegativeRules(t *testing.T) {
	p := "Add the job.\n\n1. Use concrete instants in verify, not any().\n2. Write 4 tests.\n- Do not touch git.\nNever log the token."
	got := negativeRules(p)
	for _, w := range []string{"not any()", "Do not touch git", "Never log the token"} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in %q", w, got)
		}
	}
	if strings.Contains(got, "Write 4 tests") || negativeRules("Add a test.") != "" {
		t.Fatalf("too much: %q", got)
	}
}

func TestCheckSpans(t *testing.T) {
	p := "Done when: `go test ./...` passes, `grep -n foo a.kt` finds nothing, `ls -la` is fine and `any()` is gone"
	cmds, skipped := checkSpans(p)
	if len(cmds) != 2 || cmds[1] != "grep -n foo a.kt" {
		t.Fatalf("cmds %q", cmds)
	}
	if len(skipped) != 1 || skipped[0] != "ls -la" {
		t.Fatalf("skipped %q", skipped)
	}
	if v := checkVerdict([]string{"qh check passed: a", "qh check skipped (not a command qh runs): ls -la"}); !strings.Contains(v, "1 not run") {
		t.Fatal(v)
	}
}

func TestMissingAnnotations(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "MailRenderer.kt")
	os.WriteFile(f, []byte("package a\n\nclass MailRenderer(val x: Int) {\n    @Scheduled(cron = \"0 0 * * *\")\n    fun run() {}\n}\n"), 0o644)
	g := filepath.Join(dir, "Cfg.kt")
	os.WriteFile(g, []byte("package a\n\n@Configuration\nclass Cfg\n"), 0o644)
	prompt := "Do it.\n\n```kotlin\n@Component\nclass MailRenderer(private val x: Int) {\n    @Scheduled(cron = \"0 0 * * *\")\n    fun run() {}\n}\n```\n\n```kotlin\n@Configuration\nclass Cfg\n```"
	got := missingAnnotations(prompt, []string{f, g})
	if len(got) != 1 || got[0] != "MailRenderer.kt: class MailRenderer has no @Component" {
		t.Fatalf("got %q", got)
	}
}

func TestImportHints(t *testing.T) {
	got := importHints([]string{"src/test/kotlin/A.kt:12:30 Unresolved reference 'just'.", "src/test/kotlin/A.kt:12:35 Unresolved reference: Runs", "A.kt:3:1 Unresolved reference 'foo'."})
	if strings.Join(got, ",") != "import io.mockk.just,import io.mockk.Runs" {
		t.Fatalf("got %q", got)
	}
}

func TestReflectionInTests(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "SenderTest.kt")
	os.WriteFile(f, []byte("val f = Sender::class.java.getDeclaredField(\"host\")\nf.isAccessible = true\n"), 0o644)
	g := filepath.Join(dir, "Sender.kt")
	os.WriteFile(g, []byte("val f = x.getDeclaredField(\"a\")\n"), 0o644)
	if got := reflectionInTests([]string{f, g}); len(got) != 2 || !strings.HasPrefix(got[1], "SenderTest.kt:2:") {
		t.Fatalf("got %q", got)
	}
}

func TestJUnitSummary(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "build", "test-results", "test")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "TEST-a.AlphaTest.xml"), []byte(`<testsuite><testcase name="x" classname="a.AlphaTest"/><testcase name="y" classname="a.AlphaTest"><failure message="boom">boom
	at a.AlphaTest.y(AlphaTest.kt:3)</failure></testcase><testcase name="z" classname="a.AlphaTest"><skipped/></testcase></testsuite>`), 0o644)
	os.WriteFile(filepath.Join(d, "TEST-a.BetaTest.xml"), []byte(`<testsuite><testcase name="x" classname="a.BetaTest"/></testsuite>`), 0o644)
	sum, f := junitFailures(dir, time.Now().Add(-time.Minute))
	if sum != "2 passed, 1 failed, 1 skipped (AlphaTest 3, BetaTest 1)" || !strings.Contains(f, "FAILED a.AlphaTest.y: boom") {
		t.Fatalf("sum %q f %q", sum, f)
	}
}

func TestBSDSedHint(t *testing.T) {
	if !bsdSed.MatchString(`sed: 1: "src/A.kt": invalid command code s`) || bsdSed.MatchString("sed: can't read x: No such file") {
		t.Fatal("bsdSed")
	}
}

func TestStrayFiles(t *testing.T) {
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init", "-q").Run()
	os.WriteFile(filepath.Join(dir, "A.kt"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "A.kt.bak"), nil, 0o644)
	if got := strayFiles(dir); len(got) != 1 || got[0] != "A.kt.bak" {
		t.Fatalf("got %q", got)
	}
}

func TestCheckExpects(t *testing.T) {
	p := "Do it.\n\nDone when: `./gradlew test` passes, `git status --short` shows no `.bak` file, `git diff --name-only src/x` prints nothing, and `ls build`."
	e := checkExpects(p)
	if e["git status --short"].none != ".bak" || !e["git diff --name-only src/x"].empty || e["ls build"] != (expect{}) || e["./gradlew test"] != (expect{}) {
		t.Fatalf("got %+v", e)
	}
	if e["git status --short"].miss(" M a.kt\n?? a.kt.bak\n[exit 0]") == "" || e["git status --short"].miss(" M a.kt\n[exit 0]") != "" {
		t.Fatal("none")
	}
	if e["git diff --name-only src/x"].miss("[exit 0]") != "" || e["git diff --name-only src/x"].miss("src/x/A.kt\n[exit 0]") == "" {
		t.Fatal("empty")
	}
	if !printOnly("git status --short") || printOnly("git diff --exit-code") || printOnly("./gradlew test") || printOnly("ls | grep -q x") {
		t.Fatal("printOnly")
	}
	if v := checkVerdict([]string{"qh check passed: a", "qh check info (x): ls"}); !strings.Contains(v, "1 only print state") {
		t.Fatal(v)
	}
}

func TestFixColumnZero(t *testing.T) {
	src := "package a\n\nclass A {\n    fun ok() = 1\n\ninternal fun isIp(s: String): Boolean {\n    return s.isNotEmpty()\n}\n\n    val s = \"{\"\n}\n\nfun top() {}\n"
	want := "package a\n\nclass A {\n    fun ok() = 1\n\n    internal fun isIp(s: String): Boolean {\n        return s.isNotEmpty()\n    }\n\n    val s = \"{\"\n}\n\nfun top() {}\n"
	got, n := fixColumnZero("A.kt", src)
	if got != want || n != 3 {
		t.Fatalf("n=%d\n%s", n, got)
	}
	one := "class B {\n@Test\nfun x() = 1\n}\n"
	if got, n := fixColumnZero("B.kt", one); n != 2 || got != "class B {\n    @Test\n    fun x() = 1\n}\n" {
		t.Fatalf("n=%d\n%s", n, got)
	}
	if _, n := fixColumnZero("C.kt", "class C {\n    val s = \"\"\"\n{\n\"\"\"\n}\n"); n != 0 {
		t.Fatal("raw string")
	}
}

func TestStatusMismatchBody(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "build", "test-results", "test")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "TEST-a.SecIT.xml"), []byte(`<testsuite><testcase name="x" classname="a.SecIT"><failure message="java.lang.AssertionError: Status expected:&lt;401&gt; but was:&lt;200&gt;">java.lang.AssertionError: Status expected:&lt;401&gt; but was:&lt;200&gt;
	at a.SecIT.x(SecIT.kt:3)</failure></testcase><system-out>MockHttpServletResponse:
           Status = 200
             Body = {&quot;code&quot;:&quot;UNAUTHENTICATED&quot;,&quot;status&quot;:401}
</system-out></testsuite>`), 0o644)
	_, f := junitFailures(dir, time.Now().Add(-time.Minute))
	if !strings.Contains(f, `response body: {"code":"UNAUTHENTICATED","status":401}`) || !strings.Contains(f, "does not set the status") {
		t.Fatal(f)
	}
}

func TestNegativeAsserts(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "src", "test", "kotlin", "JwtIT.kt")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("val s = mapper.writeValueAsString(bytes)\nassertThat(s).doesNotContain(\"aud\")\nassertThat(s).contains(\"x\")\n"), 0o644)
	got := negativeAsserts(map[string]bool{f: true, filepath.Join(dir, "Main.kt"): true})
	if !strings.Contains(got, "JwtIT.kt:2: assertThat(s).doesNotContain") || strings.Contains(got, ":3:") {
		t.Fatal(got)
	}
}

func TestUsesReactor(t *testing.T) {
	dir := t.TempDir()
	m := filepath.Join(dir, "src", "main", "kotlin", "F.kt")
	os.MkdirAll(filepath.Dir(m), 0o755)
	os.WriteFile(m, []byte("import reactor.core.publisher.Mono\nfun f(): Mono<Void> = Mono.empty()\n"), 0o644)
	tf := filepath.Join(dir, "src", "test", "kotlin", "FTest.kt")
	os.MkdirAll(filepath.Dir(tf), 0o755)
	os.WriteFile(tf, []byte("val x: Mono<Int>\n"), 0o644)
	if !usesReactor(map[string]bool{m: true}) || usesReactor(map[string]bool{tf: true}) {
		t.Fatal("usesReactor")
	}
}

func TestGatewayPredicates(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "Routes.kt")
	os.WriteFile(f, []byte(`val r = coRouter {}
fun routes(b: RouteLocatorBuilder) = b.routes {
    route("me") {
        path("/me")
        method(HttpMethod.GET)
        uri("http://u")
    }
    route("ok") {
        path("/ok") and method(HttpMethod.GET)
        uri("http://u")
    }
    route("ok2") {
        path("/a") and
            method(HttpMethod.POST)
        uri("http://u")
    }
}
`), 0o644)
	got := gatewayPredicates([]string{f})
	if len(got) != 1 || !strings.HasPrefix(got[0], "Routes.kt:5: method(HttpMethod.GET)") {
		t.Fatalf("%q", got)
	}
}

func TestBlameLine(t *testing.T) {
	r := "Done. The failures are test environment/interaction issues rather than code defects; the code logic itself is correct."
	if blameLine(r, []string{"qh check FAILED: ./gradlew test"}) == "" || blameLine(r, []string{"qh check passed: x"}) != "" || blameLine("All fixed.", []string{"qh check FAILED: x"}) != "" {
		t.Fatal("blameLine")
	}
}

func TestRerunCmd(t *testing.T) {
	if rerunCmd("./gradlew :a:test") != "./gradlew :a:test --rerun-tasks" || rerunCmd("./gradlew test --rerun-tasks") != "./gradlew test --rerun-tasks" || rerunCmd("go test ./...") != "go test ./..." || rerunCmd("./gradlew test | tail") != "./gradlew test | tail" {
		t.Fatal("rerunCmd")
	}
}

func TestCheckTimes(t *testing.T) {
	e := checkExpects("Do it.\n\nDone when: `./gradlew :a:test` x4 passes, `go test ./...` 3 times in a row, and `git diff --stat` four times prints nothing.")
	if e["./gradlew :a:test"].times != 4 || e["go test ./..."].times != 3 || e["git diff --stat"].times != 4 || !e["git diff --stat"].empty {
		t.Fatalf("%+v", e)
	}
}

func TestCheckTimesRun(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{Cfg: Config{}}
	tools := &Tools{Cwd: dir}
	st := &Stats{}
	cmd := "n=$(cat c 2>/dev/null || echo 0); n=$((n+1)); echo $n > c; [ $n -lt 2 ]"
	msg := a.runChecks(context.Background(), tools, []string{cmd}, map[string]expect{cmd: {times: 3}}, false, false, st, func(Event) {})
	if !strings.Contains(msg, "passed 1, then failed") || !strings.HasPrefix(st.Checks[0], "qh check FAILED (passed 1 of 3 runs)") {
		t.Fatalf("%q %q", msg, st.Checks)
	}
}

func TestGlobSkipsHidden(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".cargo", "registry"), 0o755)
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, ".cargo", "registry", "a.rs"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.rs"), nil, 0o644)
	tools := &Tools{Cwd: dir}
	out, _ := tools.Run(context.Background(), "glob", `{"pattern":"**/*.rs"}`)
	if strings.Contains(out, ".cargo") || !strings.Contains(out, "src/main.rs") {
		t.Fatal(out)
	}
	out, _ = tools.Run(context.Background(), "glob", `{"pattern":"**/*.rs","path":".cargo"}`)
	if !strings.Contains(out, "a.rs") {
		t.Fatal("inside hidden root: " + out)
	}
}

func TestRmTargets(t *testing.T) {
	got := rmTargets("rm -rf .gradle/configuration-cache && ./gradlew test; rm a.txt; rm -r -f build out")
	if strings.Join(got, ",") != ".gradle/configuration-cache,build,out" {
		t.Fatalf("%q", got)
	}
}

func TestTimeoutStillChecks(t *testing.T) {
	dir := t.TempDir()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		cmd := "sleep 0.3"
		if n == 1 {
			cmd = "echo ok > done.txt"
		}
		args, _ := json.Marshal(map[string]string{"command": cmd})
		call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": fmt.Sprint("c", n), "function": map[string]any{"name": "bash", "arguments": string(args)}}}}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", call)
	}))
	defer srv.Close()
	wrap := context.Background()
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 100, MaxTokens: 100, ThinkBudget: 30000}, Wrap: wrap}
	s := &Session{Name: "t", Cwd: dir}
	ctx, cancel := context.WithTimeout(wrap, 1500*time.Millisecond)
	defer cancel()
	st := a.Run(ctx, s, "Write done.txt.\n\nDone when: `test -f done.txt` passes.", "", func(Event) {})
	all := strings.Join(st.Checks, "\n")
	if !st.TimedOut || !strings.Contains(all, "qh check passed: test -f done.txt") || !strings.Contains(all, "time limit") {
		t.Fatalf("%+v", st.Checks)
	}
}

func TestStatLineTiming(t *testing.T) {
	l := statLine(&Stats{Model: "m", Steps: 3, Output: 600, Secs: 100, GenTokens: 600, GenSecs: 12, PrefillSecs: 20, ToolSecs: 60, WaitSecs: 5})
	if !strings.Contains(l, "model: 50.0 tok/s writing, 20s reading the prompt; tools 60s; waited 5s for another session") {
		t.Fatal(l)
	}
}

func TestLineEditKeys(t *testing.T) {
	type tc struct {
		name, keys, want string
		cur              int
	}
	for _, c := range []tc{
		{"alt+backspace", "hello big-world\x1b\x7f", "hello big-", 10},
		{"ctrl+backspace", "hello world\x08", "hello ", 6},
		{"ctrl+w", "hello big-world\x17", "hello ", 6},
		{"ctrl+left then alt+d", "one two three\x1b[1;5D\x1b[1;5D\x1bd", "one  three", 4},
		{"alt+b alt+f", "one two\x1bb\x1bb\x1bfX", "oneX two", 4},
		{"ctrl+u then ctrl+y", "abc def\x15xy\x19", "xyabc def", 9},
		{"ctrl+k", "abc def\x01\x1bf\x0b", "abc", 3},
		{"ctrl+delete", "abc def\x01\x1b[3;5~", " def", 0},
		{"plain backspace", "abc\x7f", "ab", 2},
	} {
		u := &tui{}
		u.feed([]byte(c.keys))
		if string(u.buf) != c.want || u.cur != c.cur {
			t.Errorf("%s: got %q cur %d, want %q cur %d", c.name, string(u.buf), u.cur, c.want, c.cur)
		}
	}
}
