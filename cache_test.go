package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// prefixModel calls bash until a request ends with a "stop" marker in the
// task, then answers. It keeps the raw messages of every request.
type prefixModel struct {
	mu    sync.Mutex
	reqs  [][]json.RawMessage
	tools []string
	calls int
}

func (m *prefixModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    json.RawMessage   `json:"tools"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.reqs = append(m.reqs, body.Messages)
	m.tools = append(m.tools, string(body.Tools))
	var last Message
	json.Unmarshal(body.Messages[len(body.Messages)-1], &last)
	m.calls++
	n := m.calls
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s) }
	switch {
	case strings.HasPrefix(last.Content, "Stop the task for a moment"):
		chunk(`{"choices":[{"delta":{"content":"Goal: the summary."}}]}`)
	case last.Role == "tool" && strings.Contains(last.Content, "[exit 0]") && n%13 == 0, last.Role == "user" && last.From == "" && strings.HasPrefix(last.Content, "Before you report"):
		chunk(`{"choices":[{"delta":{"content":"done"}}]}`)
	default:
		// The first call writes AGENTS.md and a source folder, which buildSystem
		// would list; the system prompt must not change because of it.
		cmd := "echo step"
		if n == 1 {
			cmd = "echo rule > AGENTS.md && mkdir -p src/main/kotlin"
		}
		a, _ := json.Marshal(map[string]string{"command": cmd})
		c, _ := json.Marshal(string(a))
		chunk(fmt.Sprintf(`{"choices":[{"delta":{"reasoning":"think %d","tool_calls":[{"index":0,"id":"c%d","function":{"name":"bash","arguments":%s}}]}}]}`, n, n, c))
	}
	chunk(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5}}`)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestPromptOnlyGrows checks that each request of a session starts with the
// exact messages of the request before it, so the server can reuse its cache.
// Only the request right after a compaction may start over.
func TestPromptOnlyGrows(t *testing.T) {
	t.Setenv("QH_HOME", t.TempDir())
	dir := t.TempDir()
	fm := &prefixModel{}
	ts := httptest.NewServer(fm)
	defer ts.Close()
	a := &Agent{LLM: &LLM{BaseURL: ts.URL, Model: "m", HTTP: ts.Client()}, Cfg: Config{MaxSteps: 16, MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Name: "p", Cwd: dir, Think: true}
	// Turn 1: 12 tool steps (the step note at 10 and the self-check), then the
	// answer. Turn 2 runs into the step limit (5-steps-left note, limit report).
	a.Run(context.Background(), s, "do it\n\nDone when: `test -f AGENTS.md` passes", "you", func(Event) {})
	if !exists(filepath.Join(dir, "AGENTS.md")) {
		t.Fatal("the fake model did not write AGENTS.md")
	}
	a.Run(context.Background(), s, "again", "you", func(Event) {})
	mark := len(fm.reqs)
	if err := a.Compact(context.Background(), s, "", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	after := len(fm.reqs)
	a.Cfg.MaxSteps = 2
	a.Run(context.Background(), s, "third", "you", func(Event) {})

	for i := 1; i < len(fm.reqs); i++ {
		if i == after {
			// The first request after the compaction starts over, with the new AGENTS.md.
			if !bytes.Contains(fm.reqs[i][0], []byte("rule")) {
				t.Fatalf("request %d: the system prompt was not rebuilt after compaction", i)
			}
			continue
		}
		prev, cur := fm.reqs[i-1], fm.reqs[i]
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
	if bytes.Contains(fm.reqs[mark-1][0], []byte("rule")) {
		t.Fatal("the system prompt picked up AGENTS.md in the middle of the session")
	}
	if mark < 15 {
		t.Fatalf("only %d requests before the compaction", mark)
	}
}

func TestAnswerOpen(t *testing.T) {
	s := &Session{Msgs: []Message{{Role: "user", Content: "x"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", ToolCallID: "a", Content: "ok"}}}
	s.answerOpen()
	if len(s.Msgs) != 4 || s.Msgs[3].ToolCallID != "b" || !strings.Contains(s.Msgs[3].Content, "interrupted") {
		t.Fatalf("%+v", s.Msgs)
	}
	s.answerOpen()
	if len(s.Msgs) != 4 {
		t.Fatal("answered twice")
	}
}
