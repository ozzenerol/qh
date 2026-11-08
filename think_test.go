package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// thinkModel thinks a lot, makes two bash calls, then answers; it does this
// for each run (a user message that is not from qh starts one).
type thinkModel struct {
	reqs  [][]json.RawMessage
	think int // chars of reasoning per answer
}

func (m *thinkModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []json.RawMessage `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	m.reqs = append(m.reqs, body.Messages)
	var last Message
	json.Unmarshal(body.Messages[len(body.Messages)-1], &last)
	// steps since the last user message
	steps := 0
	for i := len(body.Messages) - 1; i >= 0; i-- {
		var x Message
		json.Unmarshal(body.Messages[i], &x)
		if x.Role == "user" {
			break
		}
		if x.Role == "assistant" {
			steps++
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	var b strings.Builder
	for i := 0; b.Len() < m.think; i++ {
		fmt.Fprintf(&b, "t%d ", i) // no repeats, or the loop guard stops it
	}
	r1, _ := json.Marshal(b.String()[:m.think])
	if steps < 2 {
		a, _ := json.Marshal(fmt.Sprintf(`{"command":"echo %d"}`, len(m.reqs)))
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":%s,\"tool_calls\":[{\"index\":0,\"id\":\"c%d\",\"function\":{\"name\":\"bash\",\"arguments\":%s}}]}}]}\n\n", r1, len(m.reqs), a)
	} else {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":%s,\"content\":\"done\"}}]}\n\n", r1)
	}
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
}

// extends reports if every request from..to-1 starts with the messages of
// the one before it.
func extends(t *testing.T, reqs [][]json.RawMessage, from, to int) {
	t.Helper()
	for i := from + 1; i < to; i++ {
		prev, cur := reqs[i-1], reqs[i]
		if len(cur) <= len(prev) {
			t.Fatalf("request %d is not longer than %d", i, i-1)
		}
		for j := range prev {
			if !bytes.Equal(prev[j], cur[j]) {
				t.Fatalf("request %d changes message %d", i, j)
			}
		}
	}
}

func TestDropOldThinking(t *testing.T) {
	m := &thinkModel{think: 12000} // 3 answers x 3000 tokens = 9000 tokens of thinking per run
	ts := httptest.NewServer(m)
	defer ts.Close()
	a := &Agent{LLM: &LLM{BaseURL: ts.URL, Model: "m", HTTP: ts.Client()}, Cfg: Config{MaxSteps: 10, MaxTokens: 100, ThinkBudget: 1 << 20}}
	s := &Session{Name: "t", Cwd: t.TempDir(), Think: true}
	var infos []string
	emit := func(e Event) {
		if e.T == "info" {
			infos = append(infos, e.Text)
		}
	}
	st := a.Run(context.Background(), s, "one", "you", emit)
	if st.ThinkDropped != 0 || len(m.reqs) != 3 {
		t.Fatalf("run 1: dropped %d, %d requests", st.ThinkDropped, len(m.reqs))
	}
	extends(t, m.reqs, 0, 3)
	if !bytes.Contains(m.reqs[2][2], []byte(`"reasoning_content":"t0 t1`)) {
		t.Fatal("thinking of this run is not sent back")
	}
	st = a.Run(context.Background(), s, "two", "you", emit)
	if st.ThinkDropped < 8990 || len(m.reqs) != 6 { // the stored thinking is trimmed
		t.Fatalf("run 2: dropped %d, %d requests, infos %q", st.ThinkDropped, len(m.reqs), infos)
	}
	// The first request of run 2 has no old thinking; inside the run the
	// requests extend each other again, and the new thinking is kept.
	for j, raw := range m.reqs[3][:len(m.reqs[3])-1] {
		if bytes.Contains(raw, []byte("reasoning_content")) {
			t.Fatalf("old message %d still has thinking", j)
		}
	}
	extends(t, m.reqs, 3, 6)
	if !bytes.Contains(m.reqs[5][len(m.reqs[5])-2], []byte(`"reasoning_content":"t0 t1`)) {
		t.Fatal("thinking of run 2 is not sent back")
	}
	for _, x := range s.Msgs[:7] {
		if x.ReasoningContent != "" {
			t.Fatal("the stored session keeps old thinking")
		}
	}
	if !strings.Contains(strings.Join(infos, "\n"), "tokens of thinking from earlier turns") {
		t.Fatalf("infos %q", infos)
	}
}

func TestDropOldThinkingThreshold(t *testing.T) {
	small := []Message{{Role: "user", Content: "x"}, {Role: "assistant", ReasoningContent: strings.Repeat("t", 4*7999)}}
	if dropOldThinking(small) != 0 || small[1].ReasoningContent == "" {
		t.Fatal("dropped under thinkDropMin")
	}
	// 8000 tokens of thinking in 60000 tokens of context is under 15%
	share := []Message{{Role: "tool", Content: strings.Repeat("x", 4*52000)}, {Role: "assistant", ReasoningContent: strings.Repeat("t", 4*8000)}}
	if dropOldThinking(share) != 0 {
		t.Fatal("dropped under thinkDropShare")
	}
}
