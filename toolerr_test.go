package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditIndentRepair(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "A.kt")
	os.WriteFile(p, []byte("class A {\n    companion object {\n        @JvmStatic\n        fun f() {\n            g()\n        }\n    }\n}\n"), 0o644)
	tl := &Tools{Cwd: dir}
	ctx := context.Background()
	out, ok := tl.Run(ctx, "edit", `{"path":"A.kt","old":"    @JvmStatic\n    fun f() {\n        g()\n    }","new":"    @JvmStatic\n    fun f() {\n        h()\n\n    }"}`)
	if !ok || !strings.Contains(out, "note: old matched lines 3-6 with more indentation") {
		t.Fatalf("add: %s", out)
	}
	want := "class A {\n    companion object {\n        @JvmStatic\n        fun f() {\n            h()\n\n        }\n    }\n}\n"
	if b, _ := os.ReadFile(p); string(b) != want {
		t.Fatalf("got:\n%s", b)
	}
	// Less indentation in the file than in old.
	out, ok = tl.Run(ctx, "edit", `{"path":"A.kt","old":"            class A {","new":"            class B {"}`)
	if !ok || !strings.Contains(out, "less indentation") {
		t.Fatalf("less: %s", out)
	}
	if b, _ := os.ReadFile(p); !strings.HasPrefix(string(b), "class B {\n") {
		t.Fatalf("got:\n%s", b)
	}
}

func TestEditIndentNoGuess(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.kt")
	os.WriteFile(p, []byte("  x()\n    y()\n"), 0o644)
	tl := &Tools{Cwd: dir}
	ctx := context.Background()
	// The two lines need different shifts: no repair.
	if out, ok := tl.Run(ctx, "edit", `{"path":"a.kt","old":"x()\ny()","new":"z()"}`); ok {
		t.Fatalf("uneven: %s", out)
	}
	// Two places match: no repair.
	os.WriteFile(p, []byte("  x()\n  x()\n"), 0o644)
	if out, ok := (&Tools{Cwd: dir}).Run(ctx, "edit", `{"path":"a.kt","old":"x()","new":"z()"}`); ok {
		t.Fatalf("twice: %s", out)
	}
	// Outdenting new when a line of new lacks the prefix: no repair.
	os.WriteFile(p, []byte("x()\n"), 0o644)
	if out, ok := (&Tools{Cwd: dir}).Run(ctx, "edit", `{"path":"a.kt","old":"    x()","new":"    z()\n  w()"}`); ok {
		t.Fatalf("outdent: %s", out)
	}
}

func TestEditWholeLinePick(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "S.kt")
	os.WriteFile(p, []byte("import a.User\nimport a.UserRepository\n"), 0o644)
	tl := &Tools{Cwd: dir}
	out, ok := tl.Run(context.Background(), "edit", `{"path":"S.kt","old":"import a.User","new":""}`)
	if !ok || !strings.Contains(out, "only the one at line 1 is whole lines") {
		t.Fatalf("%s", out)
	}
	if b, _ := os.ReadFile(p); string(b) != "\nimport a.UserRepository\n" {
		t.Fatalf("got %q", b)
	}
	os.WriteFile(p, []byte("    fun f() = 1 + x\n    fun f() = 1\n"), 0o644)
	if out, ok := (&Tools{Cwd: dir}).Run(context.Background(), "edit", `{"path":"S.kt","old":"fun f() = 1","new":"fun f() = 3"}`); !ok || !strings.Contains(out, "line 2") {
		t.Fatalf("indented: %s", out)
	}
	// Two whole-line matches stay an error.
	os.WriteFile(p, []byte("x\nx\n"), 0o644)
	if out, ok := (&Tools{Cwd: dir}).Run(context.Background(), "edit", `{"path":"S.kt","old":"x","new":"y"}`); ok {
		t.Fatalf("dup: %s", out)
	}
}

func TestEditOffsetAlias(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("x\ny\nx\n"), 0o644)
	if out, ok := (&Tools{Cwd: dir}).Run(context.Background(), "edit", `{"path":"a.txt","old":"x","new":"z","offset":"3"}`); !ok {
		t.Fatalf("%s", out)
	}
	if b, _ := os.ReadFile(p); string(b) != "x\ny\nz\n" {
		t.Fatalf("got %q", b)
	}
}

func TestReadDirLists(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "f.kt"), []byte("x"), 0o644)
	out, ok := (&Tools{Cwd: dir}).Run(context.Background(), "read", `{"path":"sub"}`)
	if !ok || !strings.Contains(out, "is a folder") || !strings.Contains(out, "f.kt") {
		t.Fatalf("%s", out)
	}
}

func TestFailKind(t *testing.T) {
	for _, c := range []struct{ name, out, want string }{
		{"edit", "error: old text not found in a", "misuse"},
		{"bash", "BUILD FAILED\n[exit 1]", "failed"},
		{"bash", "e: A.kt:3:1 Unresolved reference\n[exit 1]", "failed"},
		{"bash", "bash: line 1: gradle: command not found\n[exit 127]", "misuse"},
		{"bash", "bash: line 1: cd: /x/y: No such file or directory\n[exit 1]", "misuse"},
		{"bash", "bash: -c: line 3: unexpected EOF while looking for matching `''\n[exit 2]", "misuse"},
		{"bash", "grep: unrecognized option '--foo'\n[exit 2]", "misuse"},
		{"bash", "x\n[killed after 2m0s]\n[exit -1]", "misuse"},
		{"bash", "error: qh blocks digging", "misuse"},
		{"read", "[you made this exact call 3 times; the result will not change. Try a different step.]\nerror: open x", "misuse"},
	} {
		if got := failKind(c.name, c.out); got != c.want {
			t.Errorf("%s %q: %s, want %s", c.name, c.out, got, c.want)
		}
	}
}
