package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	diagMaxLines   = 10
	diagDefaultSec = 20
	staleDiffLines = 20
)

// diagCfg merges the diagnostics commands: built-in defaults for the tools
// on PATH, then the global config, then the repo's. An empty command turns
// a default off.
func (a *Agent) diagCfg(cwd string) (map[string]string, time.Duration) {
	m := defaultDiag()
	sec := a.Cfg.DiagTimeout
	merge := func(c map[string]string) {
		for k, v := range c {
			if v == "" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
	}
	merge(a.Cfg.Diagnostics)
	if r, ok := a.Cfg.Repos[repoName(cwd)]; ok {
		merge(r.Diagnostics)
		if r.DiagTimeout > 0 {
			sec = r.DiagTimeout
		}
	}
	if sec <= 0 {
		sec = diagDefaultSec
	}
	return m, time.Duration(sec) * time.Second
}

// pyCheck compiles a Python file without writing a .pyc and prints a
// syntax error as file:line:col: message.
const pyCheck = `import sys
f = sys.argv[1]
try:
    compile(open(f, encoding="utf-8").read(), f, "exec")
except SyntaxError as e:
    print("%s:%s:%s: %s" % (f, e.lineno, e.offset or 1, e.msg))`

func defaultDiag() map[string]string {
	m := map[string]string{}
	// go vet type-checks the package in about 0.1 s on qh; gopls check
	// takes 1.1 s on a clean file because it runs every analyzer.
	if p := lookTool("go"); p != "" {
		m[".go"] = "cd {dir} && " + shq(p) + " vet ."
	} else if p := lookTool("gopls"); p != "" {
		m[".go"] = shq(p) + " check -severity=error {file}"
	}
	if p := lookTool("python3"); p != "" {
		m[".py"] = shq(p) + " -c " + shq(pyCheck) + " {file}"
	}
	// ktlint parses with the Kotlin compiler and needs no classpath, so it
	// only gives syntax errors here; type errors need the Gradle build.
	if p := lookTool("ktlint"); p != "" {
		m[".kt"] = shq(p) + " --relative {file} 2>&1 | grep 'Not a valid Kotlin file'"
	}
	return m
}

// lookTool finds a tool on PATH or in the usual Go and user bin folders,
// which a daemon started from a desktop session may not have on PATH.
func lookTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	h, _ := os.UserHomeDir()
	for _, d := range []string{filepath.Join(h, "go", "bin"), "/usr/local/go/bin", filepath.Join(h, ".local", "bin")} {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var diagLine = regexp.MustCompile(`(\S+?):(\d+):(\d+)(?:-\d+)?(?::\d+(?:-\d+)?)?:?\s+(.*)`)

// diagnose runs the checker for fp's extension and returns the errors in fp
// as "file:line:col message" lines, or "" when it is clean, has no checker
// or times out.
func (t *Tools) diagnose(fp, shown string) string {
	cmd, ok := t.Diag[filepath.Ext(fp)]
	if !ok || cmd == "" {
		return ""
	}
	if !strings.Contains(cmd, "{file}") && !strings.Contains(cmd, "{dir}") {
		cmd += " {file}"
	}
	cmd = strings.ReplaceAll(cmd, "{file}", shq(fp))
	cmd = strings.ReplaceAll(cmd, "{dir}", shq(filepath.Dir(fp)))
	to := t.DiagTimeout
	if to <= 0 {
		to = diagDefaultSec * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = t.Cwd
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
	out, _ := c.CombinedOutput()
	if ctx.Err() != nil {
		return ""
	}
	return diagLines(string(out), fp, shown, c.Dir)
}

// diagLines keeps the lines of out that point into fp, at most diagMaxLines.
func diagLines(out, fp, shown, cwd string) string {
	var keep []string
	seen := map[string]bool{}
	more := 0
	for _, l := range strings.Split(out, "\n") {
		m := diagLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		p := m[1]
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if filepath.Clean(p) != fp {
			// go vet prints paths relative to the package dir.
			if q := filepath.Join(filepath.Dir(fp), m[1]); filepath.Clean(q) != fp {
				continue
			}
		}
		e := fmt.Sprintf("%s:%s:%s %s", shown, m[2], m[3], strings.TrimSpace(m[4]))
		if seen[e] {
			continue
		}
		seen[e] = true
		if len(keep) == diagMaxLines {
			more++
			continue
		}
		keep = append(keep, e)
	}
	if len(keep) == 0 {
		return ""
	}
	s := strings.Join(keep, "\n")
	if more > 0 {
		s += fmt.Sprintf("\n(%d more)", more)
	}
	return s
}

// withDiag appends the checker's errors for fp to a successful write or
// edit result.
func (t *Tools) withDiag(out string, ok bool, fp, shown string) (string, bool) {
	if !ok {
		return out, ok
	}
	if d := t.diagnose(fp, shown); d != "" {
		out += "\n[qh: errors in " + shown + " after this change]\n" + d
	}
	return out, ok
}

// fileSeen is what qh last showed the model of one file.
type fileSeen struct {
	content string
	mtime   time.Time
}

func (t *Tools) saw(fp, content string) {
	if t.Seen == nil {
		t.Seen = map[string]*fileSeen{}
	}
	var mt time.Time
	if fi, err := os.Stat(fp); err == nil {
		mt = fi.ModTime()
	}
	t.Seen[fp] = &fileSeen{content: content, mtime: mt}
}

// Refresh records the current content of the given files that the model
// has seen, after qh itself changed them (formatters, import fixes).
func (t *Tools) Refresh(files map[string]bool) {
	for f := range files {
		if _, ok := t.Seen[f]; !ok {
			continue
		}
		if b, err := os.ReadFile(f); err == nil {
			t.saw(f, string(b))
		}
	}
}

// mtimes notes the modification time of every seen file, so files the
// model's own bash command changed can be refreshed after it.
func (t *Tools) mtimes() map[string]time.Time {
	m := make(map[string]time.Time, len(t.Seen))
	for f := range t.Seen {
		if fi, err := os.Stat(f); err == nil {
			m[f] = fi.ModTime()
		}
	}
	return m
}

func (t *Tools) refreshChanged(before map[string]time.Time) {
	for f, mt := range before {
		if fi, err := os.Stat(f); err == nil && !fi.ModTime().Equal(mt) {
			if b, err := os.ReadFile(f); err == nil {
				t.saw(f, string(b))
			}
		}
	}
}

// stale returns an error for the model when fp exists and changed since qh
// last showed it, or when a write would replace a file the model never read.
// It records the current content, so the next call goes through.
func (t *Tools) stale(fp, shown string, write bool) string {
	b, err := os.ReadFile(fp)
	if err != nil {
		return ""
	}
	cur := string(b)
	rec, ok := t.Seen[fp]
	switch {
	case !ok && write:
		t.saw(fp, cur)
		return fmt.Sprintf("error: %s exists (%d lines) and you have not read it. Read it first, write replaces the whole file. Use edit for a change; the next write goes through.", shown, strings.Count(strings.TrimSuffix(cur, "\n"), "\n")+1)
	case !ok, rec.content == cur:
		return ""
	}
	t.saw(fp, cur)
	return "error: " + shown + " changed since you read it (another process, a formatter or the user). The changes, - what you saw, + what is there now:\n" +
		shortDiff(rec.content, cur, staleDiffLines) + "\nRedo the call on the new content; the next call goes through."
}

// shortDiff shows the changed lines between a and b: the common head and
// tail are cut, the rest is printed as - and + lines with line numbers.
func shortDiff(a, b string, max int) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	p := 0
	for p < len(al) && p < len(bl) && al[p] == bl[p] {
		p++
	}
	s := 0
	for s < len(al)-p && s < len(bl)-p && al[len(al)-1-s] == bl[len(bl)-1-s] {
		s++
	}
	var out []string
	for i := p; i < len(al)-s; i++ {
		out = append(out, strconv.Itoa(i+1)+" -"+al[i])
	}
	for i := p; i < len(bl)-s; i++ {
		out = append(out, strconv.Itoa(i+1)+" +"+bl[i])
	}
	if len(out) > max {
		n := len(out) - max
		out = append(out[:max], fmt.Sprintf("(%d more changed lines)", n))
	}
	return strings.Join(out, "\n")
}
