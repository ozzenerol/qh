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

func TestOutSplit(t *testing.T) {
	m := map[string]int{}
	outSplit(m, Message{Content: "done", ToolCalls: []ToolCall{
		tcall("edit", `{"path":"a","old":"xx","new":"yyy"}`),
		tcall("write", `{"path":"b","content":"z"}`),
	}}, 7)
	if m["think"] != 7 || m["text"] != 4 || m["edit.old"] != 2 || m["edit.new"] != 3 || m["write"] != 26 {
		t.Fatalf("%v", m)
	}
	if m["edit"] != len(`{"path":"a","old":"xx","new":"yyy"}`)-5 {
		t.Fatalf("edit rest %d", m["edit"])
	}
	if l := splitLine(map[string]int{"think": 60, "write": 39, "bash": 1}); l != "think 60%, write 39%" {
		t.Fatal(l)
	}
}

func tcall(name, args string) ToolCall {
	var tc ToolCall
	tc.Function.Name, tc.Function.Arguments = name, args
	return tc
}

func TestRewriteNote(t *testing.T) {
	if rewriteNote(59, 1, 1) != "" || rewriteNote(90, 20, 10) != "" {
		t.Fatal("note on a small file or a big change")
	}
	if !strings.Contains(rewriteNote(300, 1, 1), "all 300 lines to change 2") {
		t.Fatal(rewriteNote(300, 1, 1))
	}
	dir := t.TempDir()
	fp := filepath.Join(dir, "a.txt")
	old := strings.Repeat("line\n", 100)
	os.WriteFile(fp, []byte(old), 0o644)
	tl := &Tools{Cwd: dir, WriteNote: true}
	tl.saw(fp, old)
	out, ok := tl.write(fp, "a.txt", strings.Replace(old, "line", "LINE", 1))
	if !ok || !strings.Contains(out, "Use edit") {
		t.Fatal(out)
	}
	tl.WriteNote = false
	out, _ = tl.write(fp, "a.txt", old)
	if strings.Contains(out, "Use edit") {
		t.Fatal(out)
	}
}

func TestQuietStep(t *testing.T) {
	call := func(n string) ToolCall { return tcall(n, "{}") }
	if !quietStep([]ToolCall{call("edit"), call("todo")}, []bool{true, true}, []string{"edited", "ok"}) {
		t.Fatal("clean edits")
	}
	if quietStep([]ToolCall{call("edit")}, []bool{false}, []string{"error"}) ||
		quietStep([]ToolCall{call("edit")}, []bool{true}, []string{"edited\n[qh: errors in a.go after this change]"}) ||
		quietStep([]ToolCall{call("edit"), call("read")}, []bool{true, true}, []string{"", ""}) ||
		quietStep(nil, nil, nil) {
		t.Fatal("not quiet")
	}
}

func TestApplySet(t *testing.T) {
	var c Config
	if err := applySet(&c, "write_note=false, report_lines=0,edit_think=true,model=x"); err != nil {
		t.Fatal(err)
	}
	if c.writeNote() || c.reportLines() != 0 || !c.editThink() || c.Model != "x" {
		t.Fatalf("%+v", c)
	}
	if applySet(&c, "nokey") == nil {
		t.Fatal("want an error")
	}
	var d Config
	if !d.writeNote() || d.reportLines() != 8 || d.editThink() {
		t.Fatal("defaults")
	}
}

// writeModel writes a file, then runs a command, then answers. It keeps
// each request's messages and enable_thinking.
type writeModel struct {
	mu    sync.Mutex
	reqs  [][]json.RawMessage
	think []bool
}

func (m *writeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []json.RawMessage `json:"messages"`
		Kw       struct {
			Think bool `json:"enable_thinking"`
		} `json:"chat_template_kwargs"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.reqs = append(m.reqs, body.Messages)
	m.think = append(m.think, body.Kw.Think)
	n := len(m.reqs)
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s) }
	reason := ""
	if body.Kw.Think {
		reason = fmt.Sprintf(`"reasoning":"think %d",`, n)
	}
	call := func(name string, args map[string]string) {
		a, _ := json.Marshal(args)
		c, _ := json.Marshal(string(a))
		chunk(fmt.Sprintf(`{"choices":[{"delta":{%s"tool_calls":[{"index":0,"id":"c%d","function":{"name":%q,"arguments":%s}}]}}]}`, reason, n, name, c))
	}
	switch n {
	case 1:
		call("write", map[string]string{"path": "a.txt", "content": "hi\n"})
	case 2:
		call("bash", map[string]string{"command": "cat a.txt"})
	default:
		chunk(fmt.Sprintf(`{"choices":[{"delta":{%s"content":"done"}}]}`, reason))
	}
	chunk(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5}}`)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestQuietThinkKeepsPrefix checks that the step after a clean write asks
// with thinking off, the step after that thinks again, and every request
// still starts with the messages of the one before.
func TestQuietThinkKeepsPrefix(t *testing.T) {
	t.Setenv("QH_HOME", t.TempDir())
	for _, off := range []bool{false, true} {
		fm := &writeModel{}
		ts := httptest.NewServer(fm)
		a := &Agent{LLM: &LLM{BaseURL: ts.URL, Model: "m", HTTP: ts.Client()}, Cfg: Config{MaxSteps: 10, MaxTokens: 100, ThinkBudget: 30000, EditThink: &off}}
		s := &Session{Name: "q", Cwd: t.TempDir(), Think: true}
		st := a.Run(context.Background(), s, "do it", "you", func(Event) {})
		ts.Close()
		want := []bool{true, off, true}
		if fmt.Sprint(fm.think) != fmt.Sprint(want) {
			t.Fatalf("edit_think=%v: thinking per request %v, want %v", off, fm.think, want)
		}
		if st.QuietSteps != map[bool]int{false: 1, true: 0}[off] || st.OutChars["write"] == 0 || st.OutChars["think"] == 0 {
			t.Fatalf("stats %+v", st)
		}
		for i := 1; i < len(fm.reqs); i++ {
			prev, cur := fm.reqs[i-1], fm.reqs[i]
			for j := range prev {
				if !bytes.Equal(prev[j], cur[j]) {
					t.Fatalf("request %d changes message %d", i, j)
				}
			}
		}
	}
}
