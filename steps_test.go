package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditSnippet(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("1\n2\n3\n4\nold\n6\n7\n8\n9\n"), 0o644)
	tl := &Tools{Cwd: dir, Snippet: true}
	out, ok := tl.Run(context.Background(), "edit", `{"path":"a.go","old":"old","new":"new\nnewer"}`)
	want := "edited a.go at line 5 (1 replacement(s))\nthe file now:\n3\t3\n4\t4\n5\tnew\n6\tnewer\n7\t6\n8\t7"
	if !ok || out != want {
		t.Fatalf("got %q", out)
	}
	tl.Snippet = false
	out, _ = tl.Run(context.Background(), "edit", `{"path":"a.go","old":"newer","new":"x"}`)
	if strings.Contains(out, "the file now") {
		t.Fatalf("snippet while off: %q", out)
	}
}

func TestEditSnippetCut(t *testing.T) {
	src := "a\n" + strings.Repeat("n\n", 40) + "z\n"
	out := editSnippet(src, 2, 40)
	if !strings.Contains(out, "1\ta\n2\tn") || !strings.Contains(out, "[14 more lines]") || strings.Count(out, "\n") != 30 {
		t.Fatalf("got %q", out)
	}
}

func TestGrepContext(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("l1\nl2\nl3\nhit\nl5\nhit\nl7\nl8\nl9\nl10\nl11\nl12\nhit\n"), 0o644)
	tl := &Tools{Cwd: dir, GrepCtx: true}
	out, _ := tl.Run(context.Background(), "grep", `{"pattern":"hit"}`)
	want := "a.txt-1-l1\na.txt-2-l2\na.txt-3-l3\na.txt:4:hit\na.txt-5-l5\na.txt:6:hit\na.txt-7-l7\na.txt-8-l8\na.txt-9-l9\na.txt-10-l10\na.txt-11-l11\na.txt-12-l12\na.txt:13:hit\n"
	if out != want {
		t.Fatalf("got\n%s", out)
	}
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte(strings.Repeat("hit\n", 5)), 0o644)
	out, _ = tl.Run(context.Background(), "grep", `{"pattern":"hit"}`)
	if strings.Contains(out, "-l1") || strings.Count(out, "\n") != 8 {
		t.Fatalf("context with more than %d matches:\n%s", grepCtxMax, out)
	}
}

func TestStepsCfg(t *testing.T) {
	var c Config
	a := &Agent{Cfg: c}
	if !strings.Contains(a.systemExtra(false), "in one answer") || a.systemExtra(true) != "" {
		t.Fatal("batch line")
	}
	desc := func(a *Agent) string {
		for _, s := range a.schemas(false) {
			if f := s["function"].(map[string]any); f["name"] == "edit" {
				return f["description"].(string)
			}
		}
		return ""
	}
	if !strings.Contains(desc(a), "one answer") {
		t.Fatal("edit description")
	}
	a.Cfg.setAll(false)
	if strings.Contains(a.systemExtra(false), "in one answer") || strings.Contains(desc(a), "one answer") || a.Cfg.snippet() || a.Cfg.grepCtx() {
		t.Fatal("still on after setAll(false)")
	}
	if strings.Contains(toolSchemas(false)[2]["function"].(map[string]any)["description"].(string), "one answer") {
		t.Fatal("batchSchemas changed the shared schemas")
	}
}

func TestGrepContextSplit(t *testing.T) {
	h := func(at int) grepHit {
		return grepHit{"f", [][]byte{[]byte("0"), []byte("1"), []byte("2"), []byte("3"), []byte("4"), []byte("5"), []byte("6"), []byte("7"), []byte("8"), []byte("9")}, at}
	}
	out := grepWithContext([]grepHit{h(0), h(9)})
	if out != "f:1:0\nf-2-1\nf-3-2\nf-4-3\n--\nf-7-6\nf-8-7\nf-9-8\nf:10:9\n" {
		t.Fatalf("got\n%s", out)
	}
}
