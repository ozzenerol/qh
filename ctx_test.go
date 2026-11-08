package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReread(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	steps := []struct{ name, args string }{
		{"read", `{"path":"a.txt"}`},
		{"read", `{"path":"a.txt"}`},                      // unchanged
		{"read", `{"path":"a.txt"}`},                      // second ask in a row: content
		{"read", `{"path":"a.txt","offset":2,"limit":1}`}, // covered by the whole read
		{"bash", `{"command":"echo four >> a.txt"}`},
		{"read", `{"path":"a.txt"}`}, // changed: content
	}
	srv, _ := callModel(t, func(n int) (string, string) {
		if n <= len(steps) {
			return steps[n-1].name, steps[n-1].args
		}
		return "", ""
	})
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 20, MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Name: "t", Cwd: dir}
	st := a.Run(context.Background(), s, "look", "", func(Event) {})
	var res []string
	for _, m := range s.Msgs {
		if m.Role == "tool" {
			res = append(res, m.Content)
		}
	}
	want := []string{"1\tone", "unchanged since your read at step 1 (3 lines)", "1\tone", "unchanged since your read of the whole file at step 3", "", "4\tfour"}
	for i, w := range want {
		if !strings.Contains(res[i], w) {
			t.Fatalf("result %d: %q, want %q", i+1, res[i], w)
		}
	}
	if st.Ctx["read"] == 0 || st.Ctx["bash"] == 0 {
		t.Fatalf("ctx split %v", st.Ctx)
	}

	// An earlier read that is no longer in the context (compacted) does not count.
	s.Msgs = nil
	if note := s.reread(&Tools{Cwd: dir}, "read", `{"path":"a.txt"}`, 1); note != "" {
		t.Fatalf("after compaction: %q", note)
	}
}

func TestReadOutline(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package x\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "func F%d() {\n\tx := 1\n\t_ = x\n\treturn\n}\n", i)
	}
	os.WriteFile(filepath.Join(dir, "x.go"), []byte(b.String()), 0o644)
	tl := &Tools{Cwd: dir}
	out, _ := tl.Run(context.Background(), "read", `{"path":"x.go"}`)
	if !strings.Contains(out, "200\t") || strings.Contains(out, "201\t\tx") || !strings.Contains(out, "[lines 1-200 of 501; use offset=201 for more]\n[outline of lines 201-501") || !strings.Contains(out, "497\tfunc F99() {") || !strings.Contains(out, "the part you need]\n202\tfunc F40() {") {
		t.Fatalf("outline: %s", out[len(out)-600:])
	}
	// a range or a limit gets the plain lines
	if out, _ := tl.Run(context.Background(), "read", `{"path":"x.go","limit":400}`); strings.Contains(out, "outline") || !strings.Contains(out, "of 501; use offset=401") {
		t.Fatalf("limit: %s", out[len(out)-200:])
	}
}

func TestBuildNoise(t *testing.T) {
	tl := &Tools{Cwd: t.TempDir()}
	cmd := func(exit int) string {
		b, _ := json.Marshal(map[string]string{"command": fmt.Sprintf("printf '> Task :compileKotlin\\n> Task :test UP-TO-DATE\\n<-------------> 40%%%% EXECUTING [3s]\\nBUILD SUCCESSFUL in 3s\\n'; exit %d # ./gradlew test", exit)})
		return string(b)
	}
	out, ok := tl.Run(context.Background(), "bash", cmd(0))
	if !ok || out != "BUILD SUCCESSFUL in 3s\n[qh: 3 Gradle/Maven progress and task lines left out]\n[exit 0]" {
		t.Fatalf("pass: %q", out)
	}
	out, _ = tl.Run(context.Background(), "bash", cmd(1))
	if !strings.Contains(out, "> Task :compileKotlin") || strings.Contains(out, "left out") {
		t.Fatalf("a failed build keeps every line: %q", out)
	}
	if o, n := dropBuildNoise("ls", "> Task :x\n"); n != 0 || o != "> Task :x\n" {
		t.Fatal("not a build command")
	}
}

func TestCtxTop(t *testing.T) {
	m := map[string]int{"read": 41000, "bash": 33000, "grep": 9000, "edit": 1000}
	if s := ctxTop(m, 100000); s != "read 41%, bash 33%, grep 9%" {
		t.Fatalf("%q", s)
	}
	st := &Stats{Model: "m", Prompt: 100000, Ctx: m}
	if !strings.Contains(statLine(st), "(ctx: read 41%, bash 33%, grep 9%)") {
		t.Fatal(statLine(st))
	}
	st.Prompt = 30000
	if strings.Contains(statLine(st), "ctx:") {
		t.Fatal("ctx split below 40k")
	}
}
