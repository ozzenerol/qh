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
	if w := warningLines(out); len(w) != 2 {
		t.Fatalf("warnings: %q", w)
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
	if out, _ := tl.Run(ctx, "read", `{"path":"auth/id/UuidV7.kt"}`); !strings.Contains(out, "did you mean: common/id/UuidV7.kt") {
		t.Fatalf("did you mean: %s", out)
	}
}
