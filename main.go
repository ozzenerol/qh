package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const version = "0.3.0"

const usage = `qh: a coding agent for a local Qwen model

usage:
  qh                           chat in the terminal, in a new session
  qh -s NAME                   chat in session NAME
  qh -c                        continue the latest session of this folder
  qh -r [name]                 pick a session to resume, or resume NAME
  qh run [flags] "task"        run one task and print the report (task "-" reads stdin)
  qh sessions                  list the sessions
  qh wait [-s session] [-q]    wait for the run on a session to end, then print its report and stats
  qh compact [-s session] [focus]  replace the history with a summary
  qh reset [-s session]        clear a session
  qh serve                     run the daemon in the foreground
  qh stop                      stop the daemon
  qh status                    show if the daemon runs

run flags:
  -s name     session (default main; @chat is the chat that is open in a terminal)
  -C dir      working directory (default: the current one)
  -new        clear the session first
  -nothink    turn thinking off for this session
  -ro         read-only run: the tools are read, grep and ls
  -timeout d  cancel the run after d (for example 15m) and exit with 124
  -from name  label that an open chat on the session shows (default $QH_FROM or "qh run")
  -q          print only the report

qh run exits with 4 when the run stops at the step limit, and 124 at -timeout.

The daemon listens on ~/.qh/qh.sock and starts on the first call.
Settings are in ~/.qh/config.json: url, model, max_steps, max_tokens, think_budget.
`

func main() {
	log.SetFlags(log.LstdFlags)
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "":
		err = chat(args)
	case "run":
		err = runCmd(args)
	case "serve":
		err = serve()
	case "wait":
		fs := flag.NewFlagSet("wait", flag.ExitOnError)
		name := fs.String("s", "main", "session")
		q := fs.Bool("q", false, "print only the stats line")
		fs.Parse(args)
		err = waitCmd(*name, *q)
	case "sessions":
		err = simple(ClientReq{Op: "sessions"})
	case "compact":
		fs := flag.NewFlagSet("compact", flag.ExitOnError)
		name := fs.String("s", "main", "session")
		fs.Parse(args)
		err = compactCmd(*name, strings.Join(fs.Args(), " "))
	case "reset":
		fs := flag.NewFlagSet("reset", flag.ExitOnError)
		name := fs.String("s", "main", "session")
		fs.Parse(args)
		err = simple(ClientReq{Op: "reset", Session: *name})
	case "stop":
		if c, e := net.DialTimeout("unix", sockPath(), time.Second); e == nil {
			c.Close()
			err = simple(ClientReq{Op: "stop"})
		} else {
			fmt.Println("qh is not running")
		}
	case "status":
		if c, e := net.DialTimeout("unix", sockPath(), time.Second); e == nil {
			c.Close()
			cfg := loadConfig()
			fmt.Printf("qh runs on %s, model server %s\n", sockPath(), cfg.URL)
		} else {
			fmt.Println("qh is not running")
		}
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "version":
		fmt.Println("qh", version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "qh:", err)
		os.Exit(1)
	}
}

// dial connects to the daemon and starts it when it does not run.
func dial() (net.Conn, error) {
	sp := sockPath()
	if c, err := net.Dial("unix", sp); err == nil {
		return c, nil
	}
	d, err := qhDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(filepath.Join(d, "qh.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	cmd.Process.Release()
	lf.Close()
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if c, err := net.Dial("unix", sp); err == nil {
			return c, nil
		}
	}
	return nil, fmt.Errorf("the daemon did not start; see %s", filepath.Join(d, "qh.log"))
}

func send(req ClientReq) (net.Conn, *json.Decoder, error) {
	c, err := dial()
	if err != nil {
		return nil, nil, err
	}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, json.NewDecoder(c), nil
}

func simple(req ClientReq) error {
	c, dec, err := send(req)
	if err != nil {
		return err
	}
	defer c.Close()
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return nil
		}
		switch e.T {
		case "error":
			return errors.New(e.Text)
		case "done":
			if e.Text != "" {
				fmt.Println(e.Text)
			}
			return nil
		}
	}
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

type view struct {
	out      io.Writer // the answer
	log      io.Writer // progress
	color    bool
	quiet    bool
	stream   bool // print answer text as it comes
	thinking int
	inText   bool
}

func (v *view) dim(s string) string {
	if v.color {
		return "\x1b[2m" + s + "\x1b[0m"
	}
	return s
}

func (v *view) clearStatus() {
	if v.thinking > 0 && v.color {
		fmt.Fprint(v.log, "\r\x1b[K")
	}
	v.thinking = 0
}

func (v *view) event(e Event) {
	switch e.T {
	case "thinking":
		if v.quiet || !v.color {
			return
		}
		v.thinking += len(e.Text)
		fmt.Fprintf(v.log, "\r\x1b[K%s", v.dim(fmt.Sprintf("thinking... %d chars", v.thinking)))
	case "text":
		if !v.stream {
			return
		}
		v.clearStatus()
		v.inText = true
		fmt.Fprint(v.out, e.Text)
	case "tool":
		v.clearStatus()
		if v.inText {
			fmt.Fprintln(v.out)
			v.inText = false
		}
		if !v.quiet {
			fmt.Fprintln(v.log, v.dim("> "+e.Tool+" "+e.Text))
		}
	case "result":
		if !v.quiet && !e.OK {
			fmt.Fprintln(v.log, v.dim("  failed: "+e.Text))
		}
	case "info":
		v.clearStatus()
		if !v.quiet {
			fmt.Fprintln(v.log, v.dim("qh: "+e.Text))
		}
	case "error":
		v.clearStatus()
		fmt.Fprintln(v.log, "qh: error: "+e.Text)
	}
}

func statLine(st *Stats) string {
	if st == nil {
		return ""
	}
	tps := 0.0
	if st.Secs > 0 {
		tps = float64(st.Output) / st.Secs
	}
	line := fmt.Sprintf("%s: %d steps, %d tool errors, %d tokens out, %.0fs (%.1f tok/s overall), context %d tokens",
		st.Model, st.Steps, st.ToolErrs, st.Output, st.Secs, tps, st.Prompt)
	if st.Window > 0 {
		pct := st.Prompt * 100 / st.Window
		line += fmt.Sprintf(" (%d%% of %d)", pct, st.Window)
		if pct >= 60 {
			line += "; the session is getting big: start a new one or run qh compact -s NAME"
		}
	}
	return line
}

// stream sends a run and prints its events until done. Ctrl+C cancels it.
func stream(req ClientReq, v *view) (*Event, error) {
	c, dec, err := send(req)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			c.Close()
		}
	}()
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			v.clearStatus()
			return nil, errors.New("cancelled")
		}
		if e.T == "done" {
			v.clearStatus()
			return &e, nil
		}
		v.event(e)
	}
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	name := fs.String("s", "main", "session")
	dir := fs.String("C", "", "working directory")
	fresh := fs.Bool("new", false, "clear the session first")
	nothink := fs.Bool("nothink", false, "thinking off")
	quiet := fs.Bool("q", false, "print only the report")
	ro := fs.Bool("ro", false, "read-only: the tools are read, grep and ls")
	timeout := fs.Duration("timeout", 0, "cancel the run after this long, for example 15m")
	from := fs.String("from", os.Getenv("QH_FROM"), "label for watchers of the session, for example opus")
	fs.Parse(args)
	task := strings.Join(fs.Args(), " ")
	if task == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		task = string(b)
	}
	if strings.TrimSpace(task) == "" {
		return errors.New(`qh run needs a task, for example: qh run "fix the failing test in parser_test.go"`)
	}
	cwd, err := absDir(*dir)
	if err != nil {
		return err
	}
	req := ClientReq{Op: "run", Session: *name, Cwd: cwd, Prompt: task, New: *fresh, ReadOnly: *ro, From: *from}
	if *nothink {
		f := false
		req.Think = &f
	}
	v := &view{out: os.Stdout, log: os.Stderr, color: isTTY(os.Stderr), quiet: *quiet}
	fmt.Fprintln(os.Stderr, "qh: session "+*name)
	if *timeout > 0 {
		// Exit closes the connection, and the daemon cancels the run.
		time.AfterFunc(*timeout, func() {
			fmt.Fprintf(os.Stderr, "qh: timed out after %s\n", *timeout)
			os.Exit(124)
		})
	}
	e, err := stream(req, v)
	if err != nil {
		return err
	}
	fmt.Println(e.Text)
	if !*quiet {
		fmt.Fprintln(os.Stderr, v.dim(statLine(e.Stats)))
	}
	if e.Stats != nil && e.Stats.Limit {
		fmt.Fprintln(os.Stderr, "qh: the run stopped at the step limit; the task may not be finished")
		os.Exit(4)
	}
	return nil
}

// waitCmd blocks until the run on a session ends, then prints its report
// and stats line like qh run. It exits 4 at the step limit.
func waitCmd(session string, quiet bool) error {
	running := func() bool {
		c, dec, err := send(ClientReq{Op: "list"})
		if err != nil {
			return false
		}
		defer c.Close()
		var e Event
		if dec.Decode(&e) != nil {
			return false
		}
		var rows []SessionInfo
		json.Unmarshal([]byte(e.Text), &rows)
		for _, r := range rows {
			if r.Name == session {
				return r.Running
			}
		}
		return false
	}
	c, dec, err := send(ClientReq{Op: "attach", Session: session, From: "qh wait"})
	if err != nil {
		return err
	}
	defer c.Close()
	if !running() {
		fmt.Fprintln(os.Stderr, "qh: no run on session "+session)
		return nil
	}
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return errors.New("the daemon closed the connection")
		}
		if e.Replay || e.T != "done" {
			continue
		}
		if !quiet {
			fmt.Println(e.Text)
		}
		fmt.Fprintln(os.Stderr, statLine(e.Stats))
		if e.Stats != nil && e.Stats.Limit {
			os.Exit(4)
		}
		return nil
	}
}

func compactCmd(session, focus string) error {
	c, dec, err := send(ClientReq{Op: "compact", Session: session, Prompt: focus, From: os.Getenv("QH_FROM")})
	if err != nil {
		return err
	}
	defer c.Close()
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return errors.New("the daemon closed the connection")
		}
		switch e.T {
		case "queued":
			fmt.Fprintln(os.Stderr, "qh: waiting for the active run")
		case "compacted":
			fmt.Printf("compacted %s: ~%s -> ~%s tokens\n", session, human(e.Before), human(e.After))
		case "error":
			return errors.New(e.Text)
		case "done":
			return nil
		}
	}
}

func absDir(d string) (string, error) {
	if d == "" {
		return os.Getwd()
	}
	a, err := filepath.Abs(d)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(a)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", d)
	}
	return a, nil
}

const chatHelp = `/new          clear the session
/think on|off thinking for this session
/sessions     list the sessions
/exit         quit (or Ctrl+D)
End a line with \ to write more lines. Ctrl+C stops a run.`

func chat(args []string) error {
	fs := flag.NewFlagSet("qh", flag.ExitOnError)
	name := fs.String("s", "", "session")
	cont := fs.Bool("c", false, "continue the latest session of this folder")
	resume := fs.Bool("r", false, "pick a session to resume (or give its name)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)
	pick := false
	named := false
	fs.Visit(func(f *flag.Flag) { named = named || f.Name == "s" })
	switch {
	case !named && !*resume && !*cont:
		*name = newSessionName() // like a new conversation; -c or -r brings an old one back
	case *resume && fs.NArg() > 0:
		*name = fs.Arg(0)
	case *resume:
		pick = true
	case *cont:
		cwd, _ := os.Getwd()
		*name = latestSession(cwd)
	}
	if isTTY(os.Stdin) && isTTY(os.Stdout) && os.Getenv("TERM") != "dumb" {
		return chatTUI(*name, pick)
	}
	if pick {
		return errors.New("qh -r needs a terminal; use qh -r NAME")
	}
	return chatPlain(*name)
}

// chatPlain is the line chat for pipes and dumb terminals.
func chatPlain(session string) error {
	name := &session
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	color := isTTY(os.Stdout)
	v := &view{out: os.Stdout, log: os.Stdout, color: color, stream: true}
	fmt.Println(v.dim(fmt.Sprintf("qh %s, session %s in %s. /help for commands.", version, *name, cwd)))
	in := bufio.NewReader(os.Stdin)
	for {
		prompt := "> "
		if color {
			prompt = "\x1b[1m> \x1b[0m"
		}
		fmt.Print(prompt)
		var lines []string
		for {
			l, err := in.ReadString('\n')
			if err != nil && l == "" {
				fmt.Println()
				return nil
			}
			l = strings.TrimRight(l, "\r\n")
			if strings.HasSuffix(l, "\\") {
				lines = append(lines, strings.TrimSuffix(l, "\\"))
				fmt.Print(v.dim(". "))
				continue
			}
			lines = append(lines, l)
			break
		}
		text := strings.TrimSpace(strings.Join(lines, "\n"))
		switch {
		case text == "":
			continue
		case text == "/exit" || text == "/quit":
			return nil
		case text == "/help":
			fmt.Println(chatHelp)
			continue
		case text == "/new":
			if err := simple(ClientReq{Op: "reset", Session: *name, Cwd: cwd}); err != nil {
				fmt.Println("qh:", err)
			}
			continue
		case text == "/sessions":
			simple(ClientReq{Op: "sessions"})
			continue
		case strings.HasPrefix(text, "/think"):
			on := strings.TrimSpace(strings.TrimPrefix(text, "/think")) != "off"
			if err := simple(ClientReq{Op: "set", Session: *name, Cwd: cwd, Think: &on}); err != nil {
				fmt.Println("qh:", err)
				continue
			}
			continue
		}
		v.inText = false
		e, err := stream(ClientReq{Op: "run", Session: *name, Cwd: cwd, Prompt: text, From: "chat"}, v)
		if err != nil {
			fmt.Println("\nqh:", err)
			continue
		}
		if v.inText {
			fmt.Println()
		}
		fmt.Println(v.dim(statLine(e.Stats)))
	}
}
