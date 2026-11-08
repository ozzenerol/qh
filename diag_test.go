package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStaleWriteNeverRead(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	tl := &Tools{Cwd: dir}
	ctx := context.Background()
	out, ok := tl.Run(ctx, "write", `{"path":"a.txt","content":"two\n"}`)
	if ok || !strings.Contains(out, "Read it first") {
		t.Fatalf("first write: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\n" {
		t.Fatalf("file written: %q", b)
	}
	if out, ok := tl.Run(ctx, "write", `{"path":"a.txt","content":"two\n"}`); !ok {
		t.Fatalf("second write: %s", out)
	}
	if out, ok := tl.Run(ctx, "write", `{"path":"new.txt","content":"x"}`); !ok || !strings.Contains(out, "new file") {
		t.Fatalf("new file: %s", out)
	}
}

func TestStaleEditNeverReadAllowed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	tl := &Tools{Cwd: dir}
	if out, ok := tl.Run(context.Background(), "edit", `{"path":"a.txt","old":"one","new":"two"}`); !ok {
		t.Fatalf("edit: %s", out)
	}
}

func TestStaleChangedOutside(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("a\nb\nc\n"), 0o644)
	tl := &Tools{Cwd: dir}
	ctx := context.Background()
	tl.Run(ctx, "read", `{"path":"a.txt"}`)
	os.WriteFile(p, []byte("a\nB\nc\n"), 0o644)
	out, ok := tl.Run(ctx, "edit", `{"path":"a.txt","old":"c","new":"C"}`)
	if ok || !strings.Contains(out, "changed since you read it") || !strings.Contains(out, "2 -b\n2 +B") {
		t.Fatalf("edit: %s", out)
	}
	if out, ok := tl.Run(ctx, "edit", `{"path":"a.txt","old":"c","new":"C"}`); !ok {
		t.Fatalf("retry: %s", out)
	}
	// Its own write and edit keep the record current.
	if out, ok := tl.Run(ctx, "edit", `{"path":"a.txt","old":"a","new":"A"}`); !ok {
		t.Fatalf("second edit: %s", out)
	}
	if out, ok := tl.Run(ctx, "write", `{"path":"a.txt","content":"z\n"}`); !ok {
		t.Fatalf("write after edit: %s", out)
	}
	os.WriteFile(p, []byte("y\n"), 0o644)
	if out, ok := tl.Run(ctx, "write", `{"path":"a.txt","content":"w\n"}`); ok || !strings.Contains(out, "changed since") {
		t.Fatalf("stale write: %s", out)
	}
}

func TestStaleBashAndRefresh(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("a\n"), 0o644)
	tl := &Tools{Cwd: dir}
	ctx := context.Background()
	tl.Run(ctx, "read", `{"path":"a.txt"}`)
	time.Sleep(10 * time.Millisecond)
	tl.Run(ctx, "bash", `{"command":"echo b > a.txt"}`)
	if out, ok := tl.Run(ctx, "edit", `{"path":"a.txt","old":"b","new":"c"}`); !ok {
		t.Fatalf("after own bash: %s", out)
	}
	// qh's formatter path: Refresh after writing the file directly.
	os.WriteFile(p, []byte("formatted\n"), 0o644)
	tl.Refresh(map[string]bool{p: true})
	if out, ok := tl.Run(ctx, "edit", `{"path":"a.txt","old":"formatted","new":"d"}`); !ok {
		t.Fatalf("after refresh: %s", out)
	}
}

func TestShortDiffCap(t *testing.T) {
	a := strings.Repeat("x\n", 30)
	b := strings.Repeat("y\n", 30)
	d := shortDiff(a, b, 20)
	if n := strings.Count(d, "\n"); n != 20 || !strings.Contains(d, "(40 more changed lines)") {
		t.Fatalf("%d lines:\n%s", n, d)
	}
}

func TestDiagAfterEdit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "m.py"), []byte("x = 1\n"), 0o644)
	// A fake checker that prints errors in the file, another file and noise.
	tl := &Tools{Cwd: dir, Diag: map[string]string{
		".py": `echo {file}:3:5: bad thing; echo other.py:1:1: not mine; echo noise`,
	}}
	ctx := context.Background()
	out, ok := tl.Run(ctx, "edit", `{"path":"m.py","old":"x = 1","new":"x = 2"}`)
	if !ok || !strings.Contains(out, "[qh: errors in m.py after this change]\nm.py:3:5 bad thing") || strings.Contains(out, "not mine") || strings.Contains(out, "noise") {
		t.Fatalf("edit: %s", out)
	}
	tl.Diag[".py"] = "true"
	if out, _ := tl.Run(ctx, "write", `{"path":"n.py","content":"y = 1\n"}`); strings.Contains(out, "[qh:") {
		t.Fatalf("clean: %s", out)
	}
	tl.Diag[".py"] = `sleep 5; echo {file}:1:1: late`
	tl.DiagTimeout = 200 * time.Millisecond
	t0 := time.Now()
	if out, _ := tl.Run(ctx, "edit", `{"path":"n.py","old":"y = 1","new":"y = 3"}`); strings.Contains(out, "late") || time.Since(t0) > 3*time.Second {
		t.Fatalf("timeout: %s after %v", out, time.Since(t0))
	}
	// A failed edit runs no checker.
	tl.Diag[".py"] = `echo {file}:1:1: ran`
	if out, _ := tl.Run(ctx, "edit", `{"path":"n.py","old":"nope","new":"z"}`); strings.Contains(out, "ran") {
		t.Fatalf("failed edit: %s", out)
	}
}

func TestDiagLinesCapAndVetPaths(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "pkg", "a.go")
	var b strings.Builder
	for i := 1; i <= 12; i++ {
		b.WriteString("vet: ./a.go:" + strings.Repeat("1", 1) + ":" + string(rune('0'+i%10)) + ": e" + string(rune('a'+i)) + "\n")
	}
	got := diagLines(b.String(), fp, "pkg/a.go", dir)
	if strings.Count(got, "\n") != 10 || !strings.HasPrefix(got, "pkg/a.go:1:1 eb") || !strings.HasSuffix(got, "(2 more)") {
		t.Fatalf("%s", got)
	}
	gopls := fp + ":4:2-9: undefined: x\n"
	if got := diagLines(gopls, fp, "pkg/a.go", dir); got != "pkg/a.go:4:2 undefined: x" {
		t.Fatalf("gopls: %q", got)
	}
}

func TestDiagRealPython(t *testing.T) {
	if lookTool("python3") == "" {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	tl := &Tools{Cwd: dir, Diag: defaultDiag()}
	out, _ := tl.Run(context.Background(), "write", `{"path":"s.py","content":"def f(:\n    pass\n"}`)
	if !strings.Contains(out, "[qh: errors in s.py after this change]\ns.py:1:") {
		t.Fatalf("%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "__pycache__")); err == nil {
		t.Fatal("wrote __pycache__")
	}
}

func TestDiagCfgMerge(t *testing.T) {
	a := &Agent{Cfg: Config{FormatCfg: FormatCfg{Diagnostics: map[string]string{".kt": "kc {file}", ".py": ""}},
		Repos: map[string]FormatCfg{"r": {Diagnostics: map[string]string{".kt": "repo {file}"}, DiagTimeout: 5}}}}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "r", ".git"), 0o755)
	os.MkdirAll(filepath.Join(root, "other", ".git"), 0o755)
	m, to := a.diagCfg(filepath.Join(root, "other"))
	if m[".kt"] != "kc {file}" || m[".py"] != "" || to != 20*time.Second {
		t.Fatalf("global: %v %v", m, to)
	}
	m, to = a.diagCfg(filepath.Join(root, "r"))
	if m[".kt"] != "repo {file}" || to != 5*time.Second {
		t.Fatalf("repo: %v %v", m, to)
	}
}

func TestDiagRealGo(t *testing.T) {
	if lookTool("go") == "" {
		t.Skip("no go")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package main\n\nfunc g() int { return y }\n"), 0o644)
	tl := &Tools{Cwd: dir, Diag: defaultDiag()}
	out, _ := tl.Run(context.Background(), "write", `{"path":"a.go","content":"package main\n\nfunc main() { _ = x }\n"}`)
	if !strings.Contains(out, "a.go:3:19 undefined: x") || strings.Contains(out, "b.go") {
		t.Fatalf("%s", out)
	}
}
