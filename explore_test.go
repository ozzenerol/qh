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
)

func TestExplore(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "sec.go"), []byte("package x\n\nfunc filterChain() {}\n"), 0o644)
	var mu sync.Mutex
	var mainReqs, subReqs [][]Message
	var mainTools []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message       `json:"messages"`
			Tools    json.RawMessage `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		sub := strings.HasPrefix(body.Messages[0].Content, "You answer one question")
		mu.Lock()
		if sub {
			subReqs = append(subReqs, body.Messages)
		} else {
			mainReqs = append(mainReqs, body.Messages)
			mainTools = append(mainTools, string(body.Tools))
		}
		mu.Unlock()
		last := body.Messages[len(body.Messages)-1]
		w.Header().Set("Content-Type", "text/event-stream")
		call := func(id, name, args string) {
			b, _ := json.Marshal(args)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%q,\"function\":{\"name\":%q,\"arguments\":%s}}]}}]}\n\n", id, name, b)
		}
		switch {
		case sub && last.Role == "user":
			call("s1", "grep", `{"pattern":"filterChain"}`)
		case sub:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"filterChain is at sec.go:3\"}}]}\n\n")
		case last.Role == "user":
			call("m1", "explore", `{"question":"where is the filter chain?"}`)
		default:
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":3,\"prompt_tokens_details\":{\"cached_tokens\":10}}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	gate := make(chan struct{}, 1)
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Gate: gate, Cfg: Config{MaxSteps: 5, MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Name: "e", Cwd: dir, Think: true}
	var evs []string
	st := a.Run(context.Background(), s, "find it", "you", func(e Event) {
		if e.T == "tool" || e.T == "result" {
			evs = append(evs, fmt.Sprintf("%s:%s:%v", e.T, e.Tool, e.Sub))
		}
	})
	if got := strings.Join(evs, " "); got != "tool:explore:false tool:grep:true result:grep:true result:explore:false" {
		t.Fatalf("events %s", got)
	}
	if st.Steps != 2 || st.Explores != 1 || st.ExploreSteps != 2 || st.ExploreOut != 6 || st.ExploreFresh != 60 || st.Output != 6 {
		t.Fatalf("stats %+v", st)
	}
	if !strings.Contains(statLine(&st), "explore: 1 runs, 2 steps") {
		t.Fatal(statLine(&st))
	}
	if res := s.Msgs[2]; res.Role != "tool" || res.Content != "filterChain is at sec.go:3" {
		t.Fatalf("explore result %+v", res)
	}
	// The helper's grep result stays out of the main context.
	for _, m := range mainReqs[1] {
		if strings.Contains(m.Content, "sec.go:3:func") {
			t.Fatal("the helper's search output reached the main run")
		}
	}
	if !strings.Contains(subReqs[1][3].Content, "sec.go:3") || len(subReqs[0]) != 2 {
		t.Fatalf("sub-run %+v", subReqs)
	}
	if !strings.Contains(mainReqs[0][0].Content, "use explore") || !strings.Contains(mainTools[0], `"explore"`) {
		t.Fatal("explore missing from the main run's prompt or tools")
	}
	// The sub-run's system prompt is the same for every call in a folder.
	if subReqs[0][0].Content != fmt.Sprintf(exploreSystem, dir) {
		t.Fatal("explore system prompt is not fixed")
	}

	off := false
	a.Cfg.Explore = &off
	mainReqs, mainTools = nil, nil
	s2 := &Session{Name: "e2", Cwd: dir}
	a.Run(context.Background(), s2, "find it", "you", func(Event) {})
	if strings.Contains(mainReqs[0][0].Content, "explore") || strings.Contains(mainTools[0], `"explore"`) {
		t.Fatal("explore is off but still offered")
	}
	if !strings.Contains(s2.Msgs[2].Content, "unknown tool") && !strings.HasPrefix(s2.Msgs[2].Content, "error") {
		t.Fatalf("explore call with explore off: %q", s2.Msgs[2].Content)
	}
}

func TestExploreStepLimit(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Messages []Message }
		json.NewDecoder(r.Body).Decode(&body)
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasPrefix(body.Messages[len(body.Messages)-1].Content, "Stop searching") {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", strings.Repeat("line\n", 80))
		} else {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c%d\",\"function\":{\"name\":\"ls\",\"arguments\":\"{}\"}}]}}]}\n\n", n)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxTokens: 100, ThinkBudget: 30000, ExploreSteps: 3}}
	var st Stats
	told := false
	out, ok := a.explore(context.Background(), t.TempDir(), `{"question":"q"}`, &st, func(Event) {}, &told)
	if !ok || n != 4 || st.ExploreSteps != 4 || strings.Count(out, "\n") != exploreMaxLines || !strings.Contains(out, "answer cut at 60 lines") {
		t.Fatalf("n=%d st=%+v ok=%v out=%q", n, st, ok, out[len(out)-60:])
	}
	if out, ok := a.explore(context.Background(), t.TempDir(), `{}`, &st, func(Event) {}, &told); ok || !strings.Contains(out, "question") {
		t.Fatal(out)
	}
}

func TestExploreNudge(t *testing.T) {
	var n exploreNudge
	calls := []struct{ name, args string }{
		{"grep", `{"pattern":"a"}`}, {"read", `{"path":"x"}`}, {"bash", `{"command":"cd src && rg foo"}`},
		{"glob", `{"pattern":"*.go"}`},
	}
	for _, c := range calls {
		if n.after(c.name, c.args, 40000) != "" {
			t.Fatal("nudge before 5 searches")
		}
	}
	if n.after("edit", `{}`, 40000) != "" || n.run != 0 {
		t.Fatal("an edit must reset the count")
	}
	for i := 0; i < 5; i++ {
		if got := n.after("grep", `{}`, 20000); got != "" {
			t.Fatal("nudge below 30k tokens")
		}
	}
	if n.after("bash", `{"command":"go test ./..."}`, 40000) != "" || n.run != 0 {
		t.Fatal("a build command is not a search")
	}
	for i := 1; i <= 5; i++ {
		got := n.after("bash", `{"command":"find . -name '*.go'"}`, 40000)
		if (i == 5) != (got == exploreNudgeText) {
			t.Fatalf("call %d: %q", i, got)
		}
	}
	if n.after("grep", `{}`, 40000) != "" {
		t.Fatal("nudged twice in a run")
	}
}

// The nudge lands in the tool result of the 5th search in a row of a run.
func TestExploreNudgeInRun(t *testing.T) {
	for _, tc := range []struct {
		prompt int
		off    bool
		want   int
	}{{40000, false, 1}, {1000, false, 0}, {40000, true, 0}} {
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			w.Header().Set("Content-Type", "text/event-stream")
			if n <= 7 {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c%d\",\"function\":{\"name\":\"grep\",\"arguments\":\"{\\\"pattern\\\":\\\"x%d\\\"}\"}}]}}]}\n\n", n, n)
			} else {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
			}
			fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n", tc.prompt)
		}))
		a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 20, MaxTokens: 100, ThinkBudget: 30000}}
		if tc.off {
			off := false
			a.Cfg.Explore = &off
		}
		s := &Session{Name: "n", Cwd: t.TempDir()}
		a.Run(context.Background(), s, "look", "you", func(Event) {})
		srv.Close()
		var got []int
		k := 0
		for _, m := range s.Msgs {
			if m.Role == "tool" {
				k++
				if strings.Contains(m.Content, "explore keeps the results") {
					got = append(got, k)
				}
			}
		}
		if len(got) != tc.want || tc.want == 1 && got[0] != 5 {
			t.Fatalf("%+v: nudge in tool results %v", tc, got)
		}
	}
}
