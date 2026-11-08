package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644)
	}
	for _, a := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", a, out)
		}
	}
	return dir
}

func readFile(t *testing.T, p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestUndoRun(t *testing.T) {
	if lookTool("git") == "" {
		t.Skip("no git")
	}
	dir := gitRepo(t, map[string]string{"a.txt": "a1\n", "b.txt": "x\n", "d.txt": "x\n"})
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x dirty\n"), 0o644) // changed before the run
	steps := [][2]string{
		{"edit", `{"path":"a.txt","old":"a1","new":"a2"}`},
		{"write", `{"path":"sub/c.txt","content":"new\n"}`},
		{"bash", `{"command":"sed -i s/x/y/ b.txt d.txt && echo hi > e.txt"}`},
		{"edit", `{"path":"d.txt","old":"y","new":"z"}`},
	}
	srv, _ := callModel(t, func(n int) (string, string) {
		if n <= len(steps) {
			return steps[n-1][0], steps[n-1][1]
		}
		return "", ""
	})
	root := filepath.Join(t.TempDir(), "s.undo")
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 10, MaxTokens: 100, ThinkBudget: 30000}, UndoDir: root}
	a.Run(context.Background(), &Session{Name: "s", Cwd: dir}, "change things", "", func(Event) {})
	if readFile(t, filepath.Join(dir, "d.txt")) != "z\n" || readFile(t, filepath.Join(dir, "e.txt")) != "hi\n" {
		t.Fatal("run did not change the files")
	}
	if l := undoList(root); !strings.Contains(l, "5 files  change things") {
		t.Fatalf("list: %s", l)
	}
	out, err := undo(root, false)
	if err != nil {
		t.Fatalf("undo: %v\n%s", err, out)
	}
	for p, want := range map[string]string{"a.txt": "a1\n", "b.txt": "x dirty\n", "d.txt": "x\n"} {
		if got := readFile(t, filepath.Join(dir, p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{"sub/c.txt", "e.txt"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s not deleted", p)
		}
	}
	if !strings.Contains(out, "deleted   "+filepath.Join(dir, "e.txt")) || !strings.Contains(out, "restored  "+filepath.Join(dir, "a.txt")) {
		t.Fatalf("out: %s", out)
	}
	if _, err := undo(root, false); err == nil {
		t.Fatal("second undo of one run")
	}
}

func TestUndoRefuseForce(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)
	root := filepath.Join(t.TempDir(), "s.undo")
	tl := &Tools{Cwd: dir, Undo: newCheckpoint(root, dir, "p")}
	ctx := context.Background()
	tl.Run(ctx, "read", `{"path":"a.txt"}`)
	tl.Run(ctx, "write", `{"path":"a.txt","content":"two\n"}`)
	tl.Run(ctx, "edit", `{"path":"a.txt","old":"two","new":"three"}`)
	if _, err := undo(root, false); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Fatalf("unfinished: %v", err)
	}
	tl.Undo.finish()
	os.WriteFile(p, []byte("user\n"), 0o644)
	_, err := undo(root, false)
	if err == nil || !strings.Contains(err.Error(), "changed after run") || !strings.Contains(err.Error(), p) {
		t.Fatalf("refuse: %v", err)
	}
	if readFile(t, p) != "user\n" {
		t.Fatal("refused undo changed the file")
	}
	if _, err := undo(root, true); err != nil || readFile(t, p) != "one\n" {
		t.Fatalf("force: %v %q", err, readFile(t, p))
	}
}

func TestUndoKeepsFiveAndSkipsBig(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(t.TempDir(), "s.undo")
	big := filepath.Join(dir, "big.txt")
	os.WriteFile(big, append([]byte("MARK\n"), make([]byte, undoMaxFile)...), 0o644)
	for i := 0; i < 7; i++ {
		cp := newCheckpoint(root, dir, "run")
		cp.ID += string(rune('a' + i)) // same millisecond
		tl := &Tools{Cwd: dir, Undo: cp}
		tl.Run(context.Background(), "read", `{"path":"f.txt"}`)
		tl.Run(context.Background(), "write", `{"path":"f.txt","content":"x"}`)
		if i == 6 {
			if out, ok := tl.Run(context.Background(), "edit", `{"path":"big.txt","old":"MARK","new":"y"}`); !ok {
				t.Fatal(out)
			}
		}
		cp.finish()
	}
	if n := len(listRuns(root)); n != 5 {
		t.Fatalf("%d runs kept", n)
	}
	out, err := undo(root, false)
	if err != nil || !strings.Contains(out, "over 2 MB") {
		t.Fatalf("%v %s", err, out)
	}
}

func TestCompactClearsSeen(t *testing.T) {
	srv, _ := callModel(t, func(int) (string, string) { return "", "" })
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxTokens: 100, ThinkBudget: 30000}}
	s := &Session{Cwd: t.TempDir(), seen: map[string]*fileSeen{"/x": {}},
		Msgs: []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "ok"}}}
	if err := a.Compact(context.Background(), s, "", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if len(s.seen) != 0 {
		t.Fatal("seen kept after compaction")
	}
}
