package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestBuiltinTasks(t *testing.T) {
	sub, _ := fs.Sub(evalFS, "eval")
	ts, err := loadTasks(sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) < 8 {
		t.Fatalf("%d tasks", len(ts))
	}
	for _, x := range ts {
		if len(x.Files) == 0 || !strings.Contains(x.Prompt, "Done when") {
			t.Errorf("%s: %d files, prompt %q", x.Name, len(x.Files), x.Prompt)
		}
	}
}

func TestEvalRunner(t *testing.T) {
	t.Setenv("QH_HOME", t.TempDir())
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		last := body.Messages[len(body.Messages)-1]
		if last.Role == "user" {
			n++
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"w1","function":{"name":"write","arguments":"{\"path\":\"ok.txt\",\"content\":\"yes\"}"}}]}}]}`)
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":4,\"prompt_tokens_details\":{\"cached_tokens\":30}}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	fsys := fstest.MapFS{
		"good.task": {Data: []byte("write ok.txt\n-- check.sh --\ntest \"$(cat ok.txt)\" = yes && test -f src/a.go\n-- src/a.go --\npackage a\n")},
		"bad.task":  {Data: []byte("write ok.txt\n-- check.sh --\necho missing; test -f nope\n-- x.txt --\nx\n")},
	}
	tasks, err := loadTasks(fsys)
	if err != nil || len(tasks) != 2 {
		t.Fatal(tasks, err)
	}
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 5, MaxTokens: 100, ThinkBudget: 30000}}
	if _, err := a.runEval(context.Background(), tasks, []string{"nope"}, 1, time.Minute, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "bad good") {
		t.Fatalf("unknown task: %v", err)
	}
	if rs, err := a.runEval(context.Background(), tasks, []string{"bad"}, 1, time.Minute, io.Discard, io.Discard); err != nil || len(rs) != 1 || rs[0].Task != "bad" {
		t.Fatalf("-only bad ran %+v %v", rs, err)
	}
	n = 0
	var out bytes.Buffer
	rs, err := a.runEval(context.Background(), tasks, nil, 2, time.Minute, &out, io.Discard)
	if err != nil || len(rs) != 4 || n != 4 {
		t.Fatalf("%v %d %d", err, len(rs), n)
	}
	for _, r := range rs {
		if r.Pass != (r.Task == "good") || r.Steps != 2 || r.Fresh != 40 || r.Output != 8 {
			t.Errorf("%+v", r)
		}
		if r.Task == "bad" && !strings.Contains(r.CheckOut, "missing") {
			t.Errorf("check output %q", r.CheckOut)
		}
	}
	if !strings.HasPrefix(out.String(), "FAIL bad") || strings.Count(out.String(), "PASS good") != 2 {
		t.Fatalf("lines:\n%s", out.String())
	}
	pa, err := saveEval(EvalFile{Date: "a", Model: "m", Results: rs})
	if err != nil {
		t.Fatal(err)
	}
	rs[0].Pass, rs[0].Steps = true, 6
	pb := filepath.Join(t.TempDir(), "b.json")
	b, _ := json.Marshal(EvalFile{Date: "b", Results: rs})
	os.WriteFile(pb, b, 0o600)
	var cmp bytes.Buffer
	if err := compareEval(&cmp, pa, pb); err != nil {
		t.Fatal(err)
	}
	got := cmp.String()
	if !strings.Contains(got, "bad                    A 0/2  st   2.0") || !strings.Contains(got, "| B 1/2  st   4.0") || !strings.Contains(got, "total                  A 2/4") {
		t.Fatalf("compare:\n%s", got)
	}
}
