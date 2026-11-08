package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

//go:embed eval/*.task
var evalFS embed.FS

// EvalTask is one benchmark task. A .task file holds the prompt, then
// "-- path --" sections: check.sh and the files of the repo.
type EvalTask struct {
	Name   string
	Prompt string
	Check  string
	Files  map[string]string
}

type EvalResult struct {
	Task     string  `json:"task"`
	Run      int     `json:"run"`
	Pass     bool    `json:"pass"`
	Steps    int     `json:"steps"`
	ToolErrs int     `json:"tool_errors"`
	Prompt   int     `json:"prompt_tokens"` // the last request
	Fresh    int     `json:"fresh_tokens"`  // prompt tokens the server did not have in its cache, all requests
	Output   int     `json:"output_tokens"`
	Secs     float64 `json:"secs"`
	Limit    bool    `json:"step_limit,omitempty"`
	Error    string  `json:"error,omitempty"`
	CheckOut string  `json:"check_output,omitempty"`
}

type EvalFile struct {
	Date    string       `json:"date"`
	Version string       `json:"version"`
	Commit  string       `json:"commit,omitempty"`
	Model   string       `json:"model"`
	Results []EvalResult `json:"results"`
}

func parseTask(name, src string) (EvalTask, error) {
	t := EvalTask{Name: name, Files: map[string]string{}}
	cur, body := "", &strings.Builder{}
	flush := func() {
		if cur == "" {
			t.Prompt = strings.TrimSpace(body.String())
		} else if cur == "check.sh" {
			t.Check = body.String()
		} else {
			t.Files[cur] = body.String()
		}
		body.Reset()
	}
	for _, l := range strings.SplitAfter(src, "\n") {
		if s := strings.TrimSpace(l); strings.HasPrefix(s, "-- ") && strings.HasSuffix(s, " --") && len(s) > 6 {
			flush()
			cur = strings.TrimSpace(s[3 : len(s)-3])
			continue
		}
		body.WriteString(l)
	}
	flush()
	if t.Prompt == "" || t.Check == "" {
		return t, fmt.Errorf("%s: needs a prompt and a check.sh section", name)
	}
	return t, nil
}

func loadTasks(fsys fs.FS) ([]EvalTask, error) {
	names, err := fs.Glob(fsys, "*.task")
	if err != nil {
		return nil, err
	}
	var ts []EvalTask
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		t, err := parseTask(strings.TrimSuffix(n, ".task"), string(b))
		if err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, nil
}

// setupRepo writes the files of t into a new git repo under dir.
func setupRepo(t EvalTask, dir string) error {
	for p, c := range t.Files {
		f := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			return err
		}
	}
	cmd := exec.Command("sh", "-c", "git init -q && git add -A && git -c user.name=eval -c user.email=eval@localhost commit -qm start")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git: %v: %s", err, out)
	}
	return nil
}

// runTask runs one task through the agent in a temp repo, then its check.
func (a *Agent) runTask(ctx context.Context, t EvalTask, run int, timeout time.Duration, log io.Writer) EvalResult {
	r := EvalResult{Task: t.Name, Run: run}
	tmp, err := os.MkdirTemp("", "qh-eval-")
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer os.RemoveAll(tmp)
	repo := filepath.Join(tmp, "repo")
	os.Mkdir(repo, 0o755)
	if err := setupRepo(t, repo); err != nil {
		r.Error = err.Error()
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	s := &Session{Name: "eval-" + t.Name, Cwd: repo, Think: true}
	st := a.Run(ctx, s, t.Prompt, "eval", func(e Event) {
		switch e.T {
		case "tool":
			fmt.Fprintf(log, "  %s %s\n", e.Tool, oneLine(e.Text, 100))
		case "error":
			fmt.Fprintf(log, "  error: %s\n", e.Text)
		}
	})
	r.Steps, r.ToolErrs, r.Prompt, r.Fresh, r.Output, r.Secs, r.Limit = st.Steps, st.ToolErrs, st.Prompt, st.Fresh, st.Output, st.Secs, st.Limit
	if ctx.Err() != nil {
		r.Error = "timeout"
	}
	// check.sh lives outside the repo, so the model never sees it.
	cp := filepath.Join(tmp, "check.sh")
	os.WriteFile(cp, []byte(t.Check), 0o755)
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer ccancel()
	cmd := exec.CommandContext(cctx, "bash", cp)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	r.Pass = err == nil
	if !r.Pass {
		r.CheckOut = string(out)
		if len(r.CheckOut) > 2000 {
			r.CheckOut = r.CheckOut[len(r.CheckOut)-2000:]
		}
	}
	return r
}

func buildCommit() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", ""
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev + dirty
}

func resultLine(r EvalResult) string {
	v := "PASS"
	if !r.Pass {
		v = "FAIL"
	}
	extra := ""
	if r.Limit {
		extra += "  step-limit"
	}
	if r.Error != "" {
		extra += "  " + r.Error
	}
	return fmt.Sprintf("%s %-22s run %d  steps %3d  errs %2d  ctx %6d  fresh %7d  out %6d  %5.0fs%s",
		v, r.Task, r.Run, r.Steps, r.ToolErrs, r.Prompt, r.Fresh, r.Output, r.Secs, extra)
}

// runEval runs the tasks (all, or the names in only) n times each and
// returns the results. Lines go to out, tool calls to log.
func (a *Agent) runEval(ctx context.Context, tasks []EvalTask, only []string, n int, timeout time.Duration, out, log io.Writer) ([]EvalResult, error) {
	want := map[string]bool{}
	for _, o := range only {
		want[o] = true
	}
	var pick []EvalTask
	for _, t := range tasks {
		if len(want) == 0 || want[t.Name] {
			pick = append(pick, t)
			delete(want, t.Name)
		}
	}
	if len(want) > 0 {
		var names []string
		for _, t := range tasks {
			names = append(names, t.Name)
		}
		var miss []string
		for w := range want {
			miss = append(miss, w)
		}
		return nil, fmt.Errorf("no task %s (tasks: %s)", strings.Join(miss, ", "), strings.Join(names, " "))
	}
	var rs []EvalResult
	for run := 1; run <= n; run++ {
		for _, t := range pick {
			if ctx.Err() != nil {
				return rs, ctx.Err()
			}
			fmt.Fprintf(log, "%s run %d\n", t.Name, run)
			r := a.runTask(ctx, t, run, timeout, log)
			fmt.Fprintln(out, resultLine(r))
			rs = append(rs, r)
		}
	}
	return rs, nil
}

type evalAgg struct {
	runs, pass, steps, errs, fresh, out int
	secs                                float64
}

func aggregate(rs []EvalResult) (map[string]*evalAgg, []string) {
	m := map[string]*evalAgg{}
	var names []string
	for _, r := range rs {
		g := m[r.Task]
		if g == nil {
			g = &evalAgg{}
			m[r.Task] = g
			names = append(names, r.Task)
		}
		g.runs++
		if r.Pass {
			g.pass++
		}
		g.steps += r.Steps
		g.errs += r.ToolErrs
		g.fresh += r.Fresh
		g.out += r.Output
		g.secs += r.Secs
	}
	sort.Strings(names)
	return m, names
}

func (g *evalAgg) String() string {
	if g == nil {
		return fmt.Sprintf("%-38s", "-")
	}
	n := float64(g.runs)
	return fmt.Sprintf("%d/%d  st %5.1f  err %4.1f  fresh %6.0f  %4.0fs", g.pass, g.runs, float64(g.steps)/n, float64(g.errs)/n, float64(g.fresh)/n, g.secs/n)
}

// compareEval prints two result files side by side, per task (means per run).
func compareEval(w io.Writer, pa, pb string) error {
	var fa, fb EvalFile
	for _, x := range []struct {
		p string
		f *EvalFile
	}{{pa, &fa}, {pb, &fb}} {
		b, err := os.ReadFile(x.p)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, x.f); err != nil {
			return fmt.Errorf("%s: %v", x.p, err)
		}
	}
	ma, na := aggregate(fa.Results)
	mb, nb := aggregate(fb.Results)
	seen := map[string]bool{}
	var names []string
	for _, n := range append(na, nb...) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	fmt.Fprintf(w, "A %s %s %s\nB %s %s %s\n", fa.Date, fa.Commit, fa.Model, fb.Date, fb.Commit, fb.Model)
	ta, tb := &evalAgg{}, &evalAgg{}
	for _, n := range names {
		fmt.Fprintf(w, "%-22s A %s | B %s\n", n, ma[n], mb[n])
		for _, x := range []struct{ t, g *evalAgg }{{ta, ma[n]}, {tb, mb[n]}} {
			if x.g != nil {
				x.t.runs += x.g.runs
				x.t.pass += x.g.pass
				x.t.steps += x.g.steps
				x.t.errs += x.g.errs
				x.t.fresh += x.g.fresh
				x.t.secs += x.g.secs
			}
		}
	}
	fmt.Fprintf(w, "%-22s A %s | B %s\n", "total", ta, tb)
	return nil
}

func evalCmd(args []string) error {
	fset := flag.NewFlagSet("eval", flag.ExitOnError)
	only := fset.String("only", "", "comma-separated task names")
	n := fset.Int("n", 1, "runs per task")
	dir := fset.String("dir", "", "read the .task files from this folder instead of the built-in ones")
	timeout := fset.Duration("timeout", 20*time.Minute, "time limit per task")
	verbose := fset.Bool("v", false, "print each tool call")
	list := fset.Bool("list", false, "list the tasks")
	compare := fset.Bool("compare", false, "compare two result files: qh eval -compare A.json B.json")
	fset.Parse(args)
	if *compare {
		if fset.NArg() != 2 {
			return fmt.Errorf("usage: qh eval -compare A.json B.json")
		}
		return compareEval(os.Stdout, fset.Arg(0), fset.Arg(1))
	}
	var fsys fs.FS
	if *dir != "" {
		fsys = os.DirFS(*dir)
	} else {
		sub, _ := fs.Sub(evalFS, "eval")
		fsys = sub
	}
	tasks, err := loadTasks(fsys)
	if err != nil {
		return err
	}
	if *list {
		for _, t := range tasks {
			fmt.Printf("%-22s %s\n", t.Name, oneLine(t.Prompt, 80))
		}
		return nil
	}
	var names []string
	if *only != "" {
		names = strings.Split(*only, ",")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg := loadConfig()
	llm := &LLM{BaseURL: cfg.URL, Model: cfg.Model, HTTP: &http.Client{}}
	if err := llm.ResolveModel(ctx); err != nil {
		return err
	}
	a := &Agent{LLM: llm, Cfg: cfg}
	log := io.Discard
	if *verbose {
		log = os.Stderr
	}
	rs, err := a.runEval(ctx, tasks, names, *n, *timeout, os.Stdout, log)
	if len(rs) > 0 {
		p, werr := saveEval(EvalFile{Date: time.Now().Format(time.RFC3339), Version: version, Commit: buildCommit(), Model: filepath.Base(llm.Model), Results: rs})
		if werr != nil {
			return werr
		}
		pass := 0
		for _, r := range rs {
			if r.Pass {
				pass++
			}
		}
		fmt.Printf("%d/%d passed; results in %s\n", pass, len(rs), p)
	}
	return err
}

func saveEval(f EvalFile) (string, error) {
	d, err := qhDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(d, "eval")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, time.Now().Format("20060102-150405")+".json")
	b, _ := json.MarshalIndent(f, "", " ")
	return p, os.WriteFile(p, b, 0o600)
}
