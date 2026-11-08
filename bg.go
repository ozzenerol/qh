package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	bgMax     = 3    // background commands running at once per run
	bgKeep    = 2000 // lines kept per command
	bgReadMax = 100  // lines that one bg read returns
)

var bgFirstOut = 3 * time.Second // how long Start waits for the first output

// bgProc is one command started with background: true.
type bgProc struct {
	id, command string
	cmd         *exec.Cmd
	exited      chan struct{} // closed when the process has exited
	readDone    chan struct{} // closed when its output is drained

	mu      sync.Mutex
	lines   []string // the last bgKeep lines
	total   int      // lines seen so far
	readTo  int      // lines given to the model so far
	code    int
	told    bool // the model was told that it exited
	stopped bool // qh stopped it
}

func (p *bgProc) add(l string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(l) > 2000 {
		l = l[:2000] + "..."
	}
	p.lines = append(p.lines, l)
	if len(p.lines) > bgKeep {
		p.lines = p.lines[len(p.lines)-bgKeep:]
	}
	p.total++
}

func (p *bgProc) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// unread returns up to max lines that the model has not seen and moves the
// read mark past them.
func (p *bgProc) unread(max int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	first := p.total - len(p.lines) // line number of p.lines[0]
	var b strings.Builder
	if p.readTo < first {
		fmt.Fprintf(&b, "[%d lines dropped, only the last %d are kept]\n", first-p.readTo, bgKeep)
		p.readTo = first
	}
	end := min(p.total, p.readTo+max)
	for _, l := range p.lines[p.readTo-first : end-first] {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if more := p.total - end; more > 0 {
		fmt.Fprintf(&b, "[%d more, read again]\n", more)
	}
	p.readTo = end
	return b.String()
}

// BG holds the background commands of one run.
type BG struct {
	mu    sync.Mutex
	procs []*bgProc
	next  int
}

// every BG of every run in this process, so a daemon stop can kill them all
var (
	bgAllMu sync.Mutex
	bgAll   = map[*BG]bool{}
)

func newBG() *BG {
	b := &BG{}
	bgAllMu.Lock()
	bgAll[b] = true
	bgAllMu.Unlock()
	return b
}

// killAllBG stops the background commands of every run.
func killAllBG() {
	bgAllMu.Lock()
	all := make([]*BG, 0, len(bgAll))
	for b := range bgAll {
		all = append(all, b)
	}
	bgAllMu.Unlock()
	for _, b := range all {
		b.StopAll()
	}
}

func (b *BG) runningList() []string {
	var out []string
	for _, p := range b.procs {
		if p.running() {
			out = append(out, p.id+": "+oneLine(p.command, 80))
		}
	}
	return out
}

// Start runs command in its own process group and returns its first output.
func (b *BG) Start(dir, command string) (string, bool) {
	b.mu.Lock()
	if r := b.runningList(); len(r) >= bgMax {
		b.mu.Unlock()
		return fmt.Sprintf("error: %d background commands are running, the most at once. Stop one first (bg {\"id\": \"bg1\", \"action\": \"stop\"}):\n%s", bgMax, strings.Join(r, "\n")), false
	}
	b.next++
	p := &bgProc{id: fmt.Sprintf("bg%d", b.next), command: command, exited: make(chan struct{}), readDone: make(chan struct{})}
	b.procs = append(b.procs, p)
	b.mu.Unlock()

	pr, pw, err := os.Pipe()
	if err != nil {
		return "error: " + err.Error(), false
	}
	p.cmd = exec.Command("bash", "-c", command)
	p.cmd.Dir = dir
	p.cmd.Stdout, p.cmd.Stderr = pw, pw
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := p.cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		close(p.exited)
		close(p.readDone)
		p.told = true
		return "error: " + err.Error(), false
	}
	pw.Close()
	go func() {
		defer close(p.readDone)
		r := bufio.NewReader(pr)
		for {
			l, err := r.ReadString('\n')
			if l != "" {
				p.add(strings.TrimRight(l, "\r\n"))
			}
			if err != nil {
				pr.Close()
				return
			}
		}
	}()
	go func() {
		p.cmd.Wait()
		p.mu.Lock()
		p.code = p.cmd.ProcessState.ExitCode()
		p.mu.Unlock()
		close(p.exited)
	}()

	select {
	case <-p.exited:
		select { // its output, unless a child still holds the pipe
		case <-p.readDone:
		case <-time.After(200 * time.Millisecond):
		}
	case <-time.After(bgFirstOut):
	}
	out := fmt.Sprintf("started %s (pid %d). Read its output with bg {\"id\": %q, \"action\": \"read\"}.\n", p.id, p.cmd.Process.Pid, p.id)
	out += p.unread(bgReadMax)
	if !p.running() {
		p.mu.Lock()
		p.told = true
		out += fmt.Sprintf("[%s exited with code %d]", p.id, p.code)
		p.mu.Unlock()
	}
	return strings.TrimRight(out, "\n"), true
}

func (b *BG) find(id string) *bgProc {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.procs {
		if p.id == id {
			return p
		}
	}
	return nil
}

// Run is the bg tool: read or stop one background command.
func (b *BG) Run(id, action string) (string, bool) {
	p := b.find(id)
	if p == nil {
		b.mu.Lock()
		var ids []string
		for _, q := range b.procs {
			ids = append(ids, q.id)
		}
		b.mu.Unlock()
		if len(ids) == 0 {
			return fmt.Sprintf("error: no background command %q; start one with bash and background: true", id), false
		}
		return fmt.Sprintf("error: no background command %q; there are %s", id, strings.Join(ids, ", ")), false
	}
	switch action {
	case "", "read":
		out := p.unread(bgReadMax)
		if !p.running() {
			p.mu.Lock()
			p.told = true
			out += fmt.Sprintf("[%s exited with code %d]", p.id, p.code)
			p.mu.Unlock()
		} else if out == "" {
			out = "(no new output; " + p.id + " is running)"
		}
		return strings.TrimRight(out, "\n"), true
	case "stop":
		if !p.running() {
			return fmt.Sprintf("%s had already exited with code %d", p.id, p.code), true
		}
		p.stop()
		return p.id + " stopped\n" + strings.TrimRight(p.unread(20), "\n"), true
	}
	return fmt.Sprintf("error: action is read or stop, not %q", action), false
}

// stop sends SIGTERM to the process group, then SIGKILL after 2 s.
func (p *bgProc) stop() {
	p.mu.Lock()
	p.stopped, p.told = true, true
	p.mu.Unlock()
	pg := -p.cmd.Process.Pid
	syscall.Kill(pg, syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(2 * time.Second):
	}
	// The leader may be gone while others in the group live on.
	syscall.Kill(pg, syscall.SIGKILL)
	<-p.exited
}

// Exits is the note for the next tool result about commands that exited on
// their own since the last note. Each one is told once.
func (b *BG) Exits() string {
	b.mu.Lock()
	procs := append([]*bgProc(nil), b.procs...)
	b.mu.Unlock()
	var out []string
	for _, p := range procs {
		if p.running() {
			continue
		}
		p.mu.Lock()
		if !p.told {
			p.told = true
			out = append(out, fmt.Sprintf("%s exited with code %d", p.id, p.code))
		}
		p.mu.Unlock()
	}
	if len(out) == 0 {
		return ""
	}
	return "\n\n[qh: " + strings.Join(out, "; ") + "]"
}

// StopAll stops every command that still runs and lists them.
func (b *BG) StopAll() []string {
	b.mu.Lock()
	procs := append([]*bgProc(nil), b.procs...)
	b.mu.Unlock()
	var names []string
	var wg sync.WaitGroup
	for _, p := range procs {
		if p.running() {
			names = append(names, p.id+" ("+oneLine(p.command, 60)+")")
			wg.Add(1)
			go func() { defer wg.Done(); p.stop() }()
		}
	}
	wg.Wait()
	return names
}

// Close stops everything and forgets this run.
func (b *BG) Close() []string {
	names := b.StopAll()
	bgAllMu.Lock()
	delete(bgAll, b)
	bgAllMu.Unlock()
	return names
}
