package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
)

// groupAlive reports if any process of the group still exists after 2 s.
// A killed child is a zombie until its new parent reaps it, so it polls.
func groupAlive(pid int) bool {
	for i := 0; i < 40; i++ {
		if syscall.Kill(-pid, 0) != nil {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

func shortBG(t *testing.T) *BG {
	old := bgFirstOut
	bgFirstOut = 400 * time.Millisecond
	b := newBG()
	t.Cleanup(func() {
		b.Close()
		bgFirstOut = old
	})
	return b
}

func TestBGStartReadStop(t *testing.T) {
	b := shortBG(t)
	tl := &Tools{Cwd: t.TempDir(), BG: b}
	out, ok := tl.Run(context.Background(), "bash", `{"command":"echo l1; echo l2; sleep 0.6; echo l3; sleep 30 & sleep 30","background":true}`)
	if !ok || !strings.HasPrefix(out, "started bg1 (pid ") || !strings.HasSuffix(out, "\nl1\nl2") {
		t.Fatalf("start: %q", out)
	}
	pid := b.procs[0].cmd.Process.Pid
	time.Sleep(500 * time.Millisecond)
	if out, _ := tl.Run(context.Background(), "bg", `{"id":"bg1","action":"read"}`); out != "l3" {
		t.Fatalf("read: %q", out)
	}
	if out, _ := tl.Run(context.Background(), "bg", `{"id":"bg1","action":"read"}`); !strings.Contains(out, "no new output") {
		t.Fatalf("read again: %q", out)
	}
	if out, ok := tl.Run(context.Background(), "bg", `{"id":"bg1","action":"stop"}`); !ok || !strings.HasPrefix(out, "bg1 stopped") {
		t.Fatalf("stop: %q", out)
	}
	if groupAlive(pid) {
		t.Fatal("the group lives on after stop")
	}
	if b.Exits() != "" {
		t.Fatal("a stopped command is reported as exited")
	}
	if out, ok := tl.Run(context.Background(), "bg", `{"id":"bg9","action":"read"}`); ok || !strings.Contains(out, "there are bg1") {
		t.Fatalf("bad id: %q", out)
	}
}

func TestBGReadCapAndExit(t *testing.T) {
	b := shortBG(t)
	out, _ := b.Start(t.TempDir(), "seq 1 150; sleep 0.8; exit 3")
	lines := strings.Split(out, "\n")
	if len(lines) != 102 || lines[100] != "100" || lines[101] != "[50 more, read again]" {
		t.Fatalf("first: %d %q", len(lines), lines[len(lines)-1])
	}
	out, _ = b.Run("bg1", "read")
	if !strings.HasPrefix(out, "101\n") || !strings.HasSuffix(out, "\n150") {
		t.Fatalf("second read: %q", out)
	}
	time.Sleep(1200 * time.Millisecond)
	if e := b.Exits(); e != "\n\n[qh: bg1 exited with code 3]" {
		t.Fatalf("exit note: %q", e)
	}
	if e := b.Exits(); e != "" {
		t.Fatalf("exit note twice: %q", e)
	}
}

func TestBGCap(t *testing.T) {
	b := shortBG(t)
	dir := t.TempDir()
	for i := 0; i < bgMax; i++ {
		if _, ok := b.Start(dir, "sleep 30"); !ok {
			t.Fatal("start")
		}
	}
	out, ok := b.Start(dir, "sleep 31")
	if ok || !strings.Contains(out, "bg1: sleep 30\nbg2: sleep 30\nbg3: sleep 30") {
		t.Fatalf("4th: %q", out)
	}
	b.Run("bg2", "stop")
	if out, ok := b.Start(dir, "sleep 31"); !ok || !strings.HasPrefix(out, "started bg4") {
		t.Fatalf("after a stop: %q", out)
	}
}

func TestBGKillAll(t *testing.T) {
	b := shortBG(t)
	b.Start(t.TempDir(), "sleep 30")
	b.Start(t.TempDir(), "trap '' TERM; sleep 30 & sleep 30") // needs the SIGKILL
	killAllBG()
	for _, p := range b.procs {
		if groupAlive(p.cmd.Process.Pid) {
			t.Fatalf("killAllBG left %s", p.command)
		}
	}
}

// A run that ends, normally or by cancel, kills what it started, children too.
func TestBGEndOfRun(t *testing.T) {
	old := bgFirstOut
	bgFirstOut = 300 * time.Millisecond
	defer func() { bgFirstOut = old }()
	for _, cancelIt := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		var started string
		srv, _ := callModel(t, func(n int) (string, string) {
			switch n {
			case 1:
				args, _ := json.Marshal(map[string]any{"command": "sleep 60 & echo up; sleep 60", "background": true})
				return "bash", string(args)
			case 2:
				if cancelIt {
					cancel()
				}
			}
			return "", ""
		})
		a := &Agent{LLM: &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, Cfg: Config{MaxSteps: 5, MaxTokens: 100, ThinkBudget: 30000}}
		s := &Session{Name: "t", Cwd: t.TempDir()}
		st := a.Run(ctx, s, "start it", "", func(e Event) {})
		cancel()
		started = s.Msgs[2].Content
		var pid int
		fmt.Sscanf(started[strings.Index(started, "pid ")+4:], "%d", &pid)
		if pid == 0 || !strings.HasSuffix(started, "\nup") {
			t.Fatalf("cancel=%v started %q", cancelIt, started)
		}
		if groupAlive(pid) {
			syscall.Kill(-pid, syscall.SIGKILL)
			t.Fatalf("cancel=%v: background group %d left running", cancelIt, pid)
		}
		if !cancelIt && !strings.Contains(strings.Join(st.Facts, "\n"), "qh facts: stopped the background commands still running at the end: bg1 (sleep 60 & echo up; sleep 60)") {
			t.Fatalf("facts %q", st.Facts)
		}
	}
}
