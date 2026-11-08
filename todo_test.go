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
	"testing"
)

// callModel answers each request with the tool call that script returns for
// it (1-based), or with a text answer when script returns "".
func callModel(t *testing.T, script func(n int) (string, string)) (*httptest.Server, *[][]Message) {
	var reqs [][]Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		reqs = append(reqs, body.Messages)
		w.Header().Set("Content-Type", "text/event-stream")
		name, args := script(len(reqs))
		if name == "" {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"report %d\"}}]}\n\ndata: [DONE]\n\n", len(reqs))
			return
		}
		call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": fmt.Sprint("c", len(reqs)), "function": map[string]any{"name": name, "arguments": args}}}}}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", call)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestTodoTool(t *testing.T) {
	list := `{"items":[{"text":"read the code","status":"done"},{"text":"fix parse","status":"doing"},{"text":"add a test","status":"pending"}]}`
	srv, reqs := callModel(t, func(n int) (string, string) {
		switch {
		case n == 1:
			return "todo", list
		case n <= 15:
			return "bash", fmt.Sprintf(`{"command":"echo %d"}`, n)
		}
		return "", ""
	})
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 30, MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Name: "t", Cwd: t.TempDir()}
	var infos []string
	st := a.Run(context.Background(), s, "Fix it.\n\nDone when: it works", "", func(e Event) {
		if e.T == "info" {
			infos = append(infos, e.Text)
		}
	})
	if len(s.Todo) != 3 || s.Todo[1].Status != "doing" {
		t.Fatalf("todo %+v", s.Todo)
	}
	if s.Msgs[2].Content != "3 items, 1 done" {
		t.Fatalf("result %q", s.Msgs[2].Content)
	}
	if !strings.HasPrefix(infos[0], "todo 3 items: read the code; fix parse") {
		t.Fatalf("infos %q", infos)
	}
	// the tool result of step 15 is the last message of request 16
	r16 := (*reqs)[15]
	if c := r16[len(r16)-1].Content; !strings.Contains(c, "[qh: open todo items: doing: fix parse; pending: add a test]") {
		t.Fatalf("step 15 result %q", c)
	}
	for i, r := range (*reqs)[:15] {
		if strings.Contains(r[len(r)-1].Content, "open todo items") {
			t.Fatalf("reminder in request %d", i+1)
		}
	}
	r17 := (*reqs)[16]
	if c := r17[len(r17)-1].Content; !strings.Contains(c, "Before you report") || !strings.Contains(c, "not done yet: doing: fix parse; pending: add a test") {
		t.Fatalf("self-check %q", c)
	}
	if len(st.Todo) != 2 {
		t.Fatalf("open at the end %q", st.Todo)
	}

	// The list is saved with the session and comes back on resume.
	b, _ := json.Marshal(s)
	var s2 Session
	json.Unmarshal(b, &s2)
	if len(s2.Todo) != 3 {
		t.Fatalf("resumed %+v", s2.Todo)
	}
	srv2, _ := callModel(t, func(n int) (string, string) {
		if n == 1 {
			return "todo", strings.ReplaceAll(list, `"doing"`, `"done"`)
		}
		return "", ""
	})
	a.LLM = &LLM{BaseURL: srv2.URL, Model: "m", HTTP: srv2.Client()}
	infos = nil
	st = a.Run(context.Background(), &s2, "go on", "", func(e Event) {
		if e.T == "info" {
			infos = append(infos, e.Text)
		}
	})
	if len(infos) != 1 || infos[0] != "todo done: fix parse" || len(st.Todo) != 1 {
		t.Fatalf("infos %q open %q", infos, st.Todo)
	}
}

func TestParseTodo(t *testing.T) {
	it, err := parseTodo(`{"items":"[{\"text\":\"a\",\"status\":\"in_progress\"},\"b\"]"}`)
	if err != nil || len(it) != 2 || it[0].Status != "doing" || it[1] != (TodoItem{"b", "pending"}) {
		t.Fatalf("%+v %v", it, err)
	}
	if _, err := parseTodo(`{"items":[{"text":"a","status":"later"}]}`); err == nil {
		t.Fatal("bad status accepted")
	}
	if _, err := parseTodo(`{}`); err == nil {
		t.Fatal("missing items accepted")
	}
	ch := todoChanges([]TodoItem{{"a", "pending"}, {"b", "pending"}}, []TodoItem{{"a", "done"}, {"c", "pending"}})
	if strings.Join(ch, "|") != "done: a|+ c (pending)|- b" {
		t.Fatalf("changes %q", ch)
	}
}

func TestGlobAndGrepTools(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"src/test/kotlin/a/FooTest.kt", "src/main/kotlin/a/Foo.kt", "build/x/BarTest.kt", "node_modules/m/CTest.kt", "FooTest.kt"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte("fun any(x) = 1\n"), 0o644)
	}
	tl := &Tools{Cwd: dir}
	ctx := context.Background()
	if out, ok := tl.Run(ctx, "glob", `{"pattern":"src/**/*Test.kt"}`); !ok || out != "src/test/kotlin/a/FooTest.kt" {
		t.Fatalf("glob: %q", out)
	}
	if out, _ := tl.Run(ctx, "glob", `{"pattern":"**/*Test.kt"}`); out != "FooTest.kt\nsrc/test/kotlin/a/FooTest.kt" {
		t.Fatalf("glob **: %q", out)
	}
	if out, _ := tl.Run(ctx, "glob", `{"pattern":"*.kt","path":"src/main"}`); out != "src/main/kotlin/a/Foo.kt" {
		t.Fatalf("glob in path: %q", out)
	}
	if out, ok := tl.Run(ctx, "grep", `{"pattern":"any(x)","literal":true,"glob":"src/**/*.kt"}`); !ok || out != "src/main/kotlin/a/Foo.kt:1:fun any(x) = 1\nsrc/test/kotlin/a/FooTest.kt:1:fun any(x) = 1\n" {
		t.Fatalf("grep literal: %q", out)
	}
	var big strings.Builder
	for i := 0; i < 130; i++ {
		big.WriteString("hit\n")
	}
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big.String()), 0o644)
	out, _ := tl.Run(ctx, "grep", `{"pattern":"^hit$"}`)
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 101 || lines[100] != "[30 more]" || lines[99] != "big.txt:100:hit" {
		t.Fatalf("grep cap: %d %q", len(lines), lines[len(lines)-1])
	}
}
