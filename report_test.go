package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFactsAndClaimsInRun(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "A.kt"), []byte("class A {\n    val a = 1\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "F.kt"), []byte("class F {\n  val f = 1\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "Old.kt"), []byte("x\n"), 0o644)
	report := "Done.\n\n## Report\n\n**Files changed:**\n- `src/A.kt` - new value\n- `Other.kt` - cleanup\n\n**Commands run:** none, 13 tests passed"
	srv, _ := callModel(t, func(n int) (string, string) {
		switch n {
		case 1:
			return "edit", `{"path":"A.kt","old":"val a = 1","new":"val a = 2"}`
		case 2:
			return "write", `{"path":"B.kt","content":"class B\n"}`
		case 3:
			return "edit", `{"path":"F.kt","old":"  val f = 1","new":"    val f = 1"}`
		case 4:
			return "bash", `{"command":"rm Old.kt"}`
		}
		return "", ""
	})
	root := filepath.Join(t.TempDir(), "u")
	a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 10, MaxTokens: 100, ThinkBudget: 30000}, UndoDir: root}
	s := &Session{Name: "f", Cwd: dir}
	st := a.Run(context.Background(), s, "change", "", func(Event) {})
	// The fake model's answer is "report N"; check the facts against a real report text.
	_ = st
	tl := &Tools{}
	tl.noteTests("10 passed, 2 failed, 0 skipped (ATest 4, BTest 8)")
	cp := listRuns(root)[0]
	got := strings.Join(facts(report, cp.changes(), tl.Tests, nil, true), "\n")
	for _, want := range []string{
		"qh facts: the run changed 3 files:",
		"  created B.kt (+1 -0)",
		"  modified A.kt (+1 -1)",
		"  modified F.kt (+1 -1) whitespace only",
		"qh facts: last test run qh saw: 10 passed, 2 failed, 0 skipped",
		"qh note: the report lists Other.kt as changed, but this run did not change it",
		"qh note: the run created B.kt, and the report does not mention it",
		`qh note: the report says "13 tests passed", but no test run qh saw had 13 tests`,
		`qh note: the report says "tests passed", but the last test run qh saw had 2 failed`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "F.kt, and the report") || strings.Contains(got, "A.kt as changed") {
		t.Errorf("false note:\n%s", got)
	}
	// Old.kt went away through bash outside git: not in the checkpoint, so not claimed.
	if strings.Contains(got, "Old.kt") {
		t.Errorf("Old.kt:\n%s", got)
	}
	// The facts go into the stats of a real run too.
	if len(st.Facts) == 0 || !strings.HasPrefix(st.Facts[0], "qh facts: the run changed 3 files") {
		t.Fatalf("run facts %q", st.Facts)
	}
}

func TestTestClaims(t *testing.T) {
	tests := []testCount{{passed: 27, failed: 8}, {passed: 35, classes: []int{15, 16, 4}}}
	for _, c := range []struct {
		report string
		want   []string
	}{
		{"35 tests passed (15+16+4); ClientIpResolverTest: 15 passed", nil},
		{"All 8 test names use backticks", nil},
		{"Build green, 323 tests passed", []string{`"323 tests passed", but no test run qh saw had 323 tests`}},
		{"27 passed, 8 failed", nil},
	} {
		got := strings.Join(testClaims(c.report, tests), "\n")
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%q: no %q in %q", c.report, w, got)
			}
		}
		if len(c.want) == 0 && got != "" {
			t.Errorf("%q: %q", c.report, got)
		}
	}
	failing := []testCount{{passed: 27, failed: 8}}
	if got := strings.Join(testClaims("All tests pass now. 0 failed.", failing), "\n"); !strings.Contains(got, `"All tests pass", but the last test run qh saw had 8 failed`) {
		t.Errorf("pass claim: %q", got)
	}
	if got := testClaims("27 passed, 8 failed; fixing next", failing); got != nil {
		t.Errorf("honest report: %q", got)
	}
	if testClaims("100 tests passed", nil) != nil {
		t.Error("claims checked with no counts seen")
	}
}

func TestClaimedFilesRealReports(t *testing.T) {
	for _, c := range []struct{ report, want string }{
		{"## Report\n\n**Files changed:**\n- `common/src/main/kotlin/a/JobMessage.kt` - x\n- `common/src/test/kotlin/a/JobMessageTest.kt` - y\n\n**Commands run:**\n- `./gradlew :common:test` - 27 tests passed", "JobMessage.kt JobMessageTest.kt"},
		{"**File changed:** `services/x/GatewaySecurityIT.kt`\n\n**Summary of changes:**\n1. Fixed `a wrong audience` in Other.kt", "GatewaySecurityIT.kt"},
		{"### Files Read\n- `TECHNICAL_SPEC.md`\n- `SecurityConfig.kt`\n\n### Files Created\n1. **`BlockedUserFilter.kt`** - new\n2. **`BlockedUserIT.kt`** - test\n\n### Commands", "BlockedUserFilter.kt BlockedUserIT.kt"},
		{"Files changed: none", ""},
	} {
		if got := strings.Join(claimedFiles(c.report), " "); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestUnifiedMatchesPatch(t *testing.T) {
	if _, err := exec.LookPath("patch"); err != nil {
		t.Skip("no patch")
	}
	before := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\n"
	now := "a\nB\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\nn\n"
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte(before), 0o644)
	d := unified(fileFact{rel: "x.txt", kind: "modified", before: before, now: now})
	if strings.Count(d, "@@") != 4 {
		t.Fatalf("want 2 hunks:\n%s", d)
	}
	cmd := exec.Command("patch", "-p1", "-s")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(d)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("patch: %v %s\n%s", err, out, d)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "x.txt")); string(b) != now {
		t.Fatalf("patched to %q", b)
	}
	// A created file.
	d = unified(fileFact{rel: "n.txt", kind: "created", now: "1\n2\n"})
	if !strings.HasPrefix(d, "--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1,2 @@\n+1\n+2\n") {
		t.Fatalf("created:\n%s", d)
	}
}

func TestQhDiff(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(t.TempDir(), "u")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	cp := newCheckpoint(root, dir, "first")
	tl := &Tools{Cwd: dir, Undo: cp}
	tl.Run(context.Background(), "edit", `{"path":"a.txt","old":"one","new":"two"}`)
	cp.finish()
	out, err := lastDiff(root)
	if err != nil || !strings.Contains(out, "run "+cp.ID+": first") || !strings.Contains(out, "-one\n+two\n") {
		t.Fatalf("%v\n%s", err, out)
	}
	// A later run that changes nothing.
	cp2 := newCheckpoint(root, dir, "second")
	cp2.ID += "x"
	cp2.finish()
	if out, _ := lastDiff(root); !strings.Contains(out, "the last run ("+cp2.ID+") changed no files") {
		t.Fatalf("%s", out)
	}
	if _, err := lastDiff(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Fatal("no error without runs")
	}
}
