package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxToolOut  = 12000 // chars of tool output sent to the model
	readDefault = 400   // lines that read returns without a limit
)

func toolSchemas() []map[string]any {
	fn := func(name, desc string, props map[string]any, req ...string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": desc,
			"parameters": map[string]any{"type": "object", "properties": props, "required": req},
		}}
	}
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num := func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	return []map[string]any{
		fn("read", "Read a text file. Lines come back numbered. Use offset and limit for big files.",
			map[string]any{"path": str("file path"), "offset": num("first line, 1-based"), "limit": num("number of lines")}, "path"),
		fn("write", "Create or overwrite a file with the full content.",
			map[string]any{"path": str("file path"), "content": str("full file content")}, "path", "content"),
		fn("edit", "Replace one exact piece of text in a file. old must appear once in the file; copy it exactly, without line numbers.",
			map[string]any{"path": str("file path"), "old": str("exact text to replace"), "new": str("replacement text"),
				"all": map[string]any{"type": "boolean", "description": "replace every occurrence"}}, "path", "old", "new"),
		fn("bash", "Run a shell command in the working directory. Use it for ls, grep, find, git, builds and tests.",
			map[string]any{"command": str("the command"), "timeout": num("seconds, default 120, max 900")}, "command"),
	}
}

type Tools struct {
	Cwd string
}

func (t *Tools) path(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.Cwd, p)
	}
	return filepath.Clean(p)
}

// Run executes one tool call and returns the text for the model.
func (t *Tools) Run(ctx context.Context, name, rawArgs string) (string, bool) {
	args := map[string]any{}
	if strings.TrimSpace(rawArgs) != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			return "error: arguments are not valid JSON: " + err.Error(), false
		}
	}
	s := func(k string) (string, bool) {
		v, ok := args[k]
		if !ok || v == nil {
			return "", false
		}
		if x, ok := v.(string); ok {
			return x, true
		}
		b, _ := json.Marshal(v)
		return string(b), true
	}
	n := func(k string, def int) int {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return i
			}
		}
		return def
	}
	need := func(keys ...string) string {
		for _, k := range keys {
			if _, ok := s(k); !ok {
				return fmt.Sprintf("error: %s needs the %q argument", name, k)
			}
		}
		return ""
	}

	switch name {
	case "read":
		if e := need("path"); e != "" {
			return e, false
		}
		p, _ := s("path")
		return t.read(t.path(p), n("offset", 1), n("limit", readDefault))
	case "write":
		if e := need("path", "content"); e != "" {
			return e, false
		}
		p, _ := s("path")
		c, _ := s("content")
		fp := t.path(p)
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			return "error: " + err.Error(), false
		}
		if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
			return "error: " + err.Error(), false
		}
		return fmt.Sprintf("wrote %s (%d lines)", p, strings.Count(c, "\n")+1), true
	case "edit":
		if e := need("path", "old", "new"); e != "" {
			return e, false
		}
		p, _ := s("path")
		o, _ := s("old")
		nw, _ := s("new")
		all := args["all"] == true || args["all"] == "true"
		return t.edit(t.path(p), p, o, nw, all)
	case "bash":
		if e := need("command"); e != "" {
			return e, false
		}
		c, _ := s("command")
		to := n("timeout", 120)
		if to <= 0 || to > 900 {
			to = 900
		}
		return t.bash(ctx, c, time.Duration(to)*time.Second)
	}
	return fmt.Sprintf("error: unknown tool %q; the tools are read, write, edit, bash", name), false
}

func (t *Tools) read(fp string, off, limit int) (string, bool) {
	b, err := os.ReadFile(fp)
	if err != nil {
		return "error: " + err.Error(), false
	}
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return fmt.Sprintf("error: %s is a binary file (%d bytes)", fp, len(b)), false
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if off < 1 {
		off = 1
	}
	if limit <= 0 {
		limit = readDefault
	}
	if off > len(lines) {
		return fmt.Sprintf("error: the file has %d lines", len(lines)), false
	}
	end := min(len(lines), off-1+limit)
	var out strings.Builder
	for i := off - 1; i < end; i++ {
		l := lines[i]
		if len(l) > 500 {
			l = l[:500] + "...[line cut]"
		}
		fmt.Fprintf(&out, "%d\t%s\n", i+1, l)
		if out.Len() > maxToolOut {
			end = i + 1
			break
		}
	}
	if end < len(lines) {
		fmt.Fprintf(&out, "[lines %d-%d of %d; use offset=%d for more]\n", off, end, len(lines), end+1)
	}
	return out.String(), true
}

func (t *Tools) edit(fp, shown, old, nw string, all bool) (string, bool) {
	b, err := os.ReadFile(fp)
	if err != nil {
		return "error: " + err.Error(), false
	}
	src := string(b)
	if old == "" {
		return "error: old is empty; use write to create a file", false
	}
	cnt := strings.Count(src, old)
	if cnt == 0 {
		// Qwen often drops trailing spaces or copies the read tool's line numbers.
		if fixed, ok := looseFind(src, old); ok {
			old, cnt = fixed, strings.Count(src, fixed)
		}
	}
	switch {
	case cnt == 0:
		return "error: old text not found in " + shown + ". " + nearest(src, old) + " Read the file again and copy the text exactly.", false
	case cnt > 1 && !all:
		return fmt.Sprintf("error: old text appears %d times in %s (lines %s). Add more lines around it so it is unique, or set all=true.",
			cnt, shown, lineNums(src, old)), false
	}
	line := strings.Count(src[:strings.Index(src, old)], "\n") + 1
	if all {
		src = strings.ReplaceAll(src, old, nw)
	} else {
		src = strings.Replace(src, old, nw, 1)
	}
	if err := os.WriteFile(fp, []byte(src), 0o644); err != nil {
		return "error: " + err.Error(), false
	}
	return fmt.Sprintf("edited %s at line %d (%d replacement(s))", shown, line, max(1, cnt*boolInt(all))), true
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// looseFind maps old to the exact text in src when they differ only in
// trailing spaces or in "N\t" line-number prefixes from the read tool.
func looseFind(src, old string) (string, bool) {
	strip := func(l string) string {
		i := 0
		for i < len(l) && l[i] >= '0' && l[i] <= '9' {
			i++
		}
		if i > 0 && i < len(l) && l[i] == '\t' {
			return l[i+1:]
		}
		return l
	}
	want := strings.Split(old, "\n")
	for i := range want {
		want[i] = strings.TrimRight(strip(want[i]), " \t\r")
	}
	for len(want) > 0 && want[len(want)-1] == "" {
		want = want[:len(want)-1]
	}
	if len(want) == 0 {
		return "", false
	}
	lines := strings.SplitAfter(src, "\n")
	match := ""
	found := 0
	for i := 0; i+len(want) <= len(lines); i++ {
		ok := true
		for j, w := range want {
			if strings.TrimRight(lines[i+j], " \t\r\n") != w {
				ok = false
				break
			}
		}
		if ok {
			found++
			m := strings.Join(lines[i:i+len(want)], "")
			match = strings.TrimSuffix(m, "\n")
		}
	}
	return match, found == 1
}

func nearest(src, old string) string {
	first := ""
	for l := range strings.SplitSeq(old, "\n") {
		if strings.TrimSpace(l) != "" {
			first = strings.TrimSpace(l)
			break
		}
	}
	if first == "" {
		return ""
	}
	for i, l := range strings.Split(src, "\n") {
		if strings.Contains(l, first) {
			return fmt.Sprintf("The first line of old is at line %d, so a later line differs.", i+1)
		}
	}
	return "Even the first line of old is not in the file."
}

func lineNums(src, sub string) string {
	var out []string
	off := 0
	for len(out) < 8 {
		i := strings.Index(src[off:], sub)
		if i < 0 {
			break
		}
		out = append(out, strconv.Itoa(strings.Count(src[:off+i], "\n")+1))
		off += i + len(sub)
	}
	return strings.Join(out, ", ")
}

func (t *Tools) bash(ctx context.Context, command string, timeout time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = t.Cwd
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return "error: " + err.Error(), false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	res := clip(out.String())
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res += fmt.Sprintf("\n[killed after %s]", timeout)
	case err != nil && code == 0:
		res += "\n[" + err.Error() + "]"
	}
	if strings.TrimSpace(res) == "" {
		res = "(no output)"
	}
	return fmt.Sprintf("%s\n[exit %d]", strings.TrimRight(res, "\n"), code), code == 0
}

// clip keeps the start and the end of long output; errors are usually at the end.
func clip(s string) string {
	if len(s) <= maxToolOut {
		return s
	}
	head, tail := maxToolOut/3, maxToolOut*2/3
	cut := strings.Count(s[head:len(s)-tail], "\n")
	return s[:head] + fmt.Sprintf("\n...[%d lines cut; filter the output with grep, head or tail]...\n", cut) + s[len(s)-tail:]
}
