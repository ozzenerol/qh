package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxToolOut  = 12000 // chars of tool output sent to the model
	readDefault = 400   // lines that read returns without a limit
	readHead    = 200   // lines of a bigger file that a read with no range returns, with an outline
	readAuto    = -1    // limit value: no offset or limit given
)

func toolSchemas(readOnly bool) []map[string]any {
	fn := func(name, desc string, props map[string]any, req ...string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": desc,
			"parameters": map[string]any{"type": "object", "properties": props, "required": req},
		}}
	}
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num := func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	read := fn("read", "Read a text file. Lines come back numbered. Use offset and limit for big files.",
		map[string]any{"path": str("file path"), "offset": num("first line, 1-based"), "limit": num("number of lines")}, "path")
	grep := fn("grep", "Search file contents. Returns file:line:text, at most 100 lines.",
		map[string]any{"pattern": str("regular expression, or plain text with literal"), "path": str("file or directory, default ."),
			"glob":    str("only files that match, for example *.go or src/**/*Test.kt"),
			"literal": map[string]any{"type": "boolean", "description": "match the pattern as plain text"}}, "pattern")
	if readOnly {
		return []map[string]any{read, grep,
			fn("ls", "List a directory. Directories end with /.",
				map[string]any{"path": str("directory, default .")}),
		}
	}
	return []map[string]any{read,
		fn("write", "Create or overwrite a file with the full content.",
			map[string]any{"path": str("file path"), "content": str("full file content")}, "path", "content"),
		fn("edit", "Replace one exact piece of text in a file. old must appear once in the file; copy it exactly, without line numbers.",
			map[string]any{"path": str("file path"), "old": str("exact text to replace"), "new": str("replacement text"),
				"all": map[string]any{"type": "boolean", "description": "replace every occurrence"}}, "path", "old", "new"),
		fn("bash", "Run a shell command in the working directory. Use it for git, builds and tests.",
			map[string]any{"command": str("the command"), "timeout": num("seconds, default 120, max 900"),
				"background": map[string]any{"type": "boolean", "description": "start a server or watcher and return at once with an id"}}, "command"),
		fn("bg", "Read the new output of a background command, or stop it.",
			map[string]any{"id": str("id from bash, like bg1"), "action": map[string]any{"type": "string", "enum": []string{"read", "stop"}}}, "id", "action"),
		grep,
		fn("glob", "Find files by path pattern, like src/**/*Test.kt. Returns at most 200 paths.",
			map[string]any{"pattern": str("glob pattern; ** matches any directories"), "path": str("directory, default .")}, "pattern"),
		todoSchema(),
	}
}

type Tools struct {
	Cwd      string
	ReadOnly bool

	Diag        map[string]string    // file extension -> checker command run after a write or edit
	DiagTimeout time.Duration        // per check
	Seen        map[string]*fileSeen // path -> what qh last showed the model of it
	BG          *BG                  // background commands of the run; nil means none allowed
	Undo        *Checkpoint          // the files this run changed, for qh undo; nil keeps nothing
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

	if t.ReadOnly && (name == "write" || name == "edit" || name == "bash" || name == "bg") {
		return "error: this is a read-only run; the tools are read, grep and ls", false
	}
	switch name {
	case "find", "glob":
		// Not in the tool list, but the model calls them; answer them.
		pat, _ := s("pattern")
		if pat == "" {
			pat, _ = s("name")
		}
		if pat == "" {
			pat = "*"
		}
		p, ok := s("path")
		if !ok || p == "" {
			p = "."
		}
		return t.glob(ctx, t.path(p), pat)
	case "grep":
		if e := need("pattern"); e != "" {
			return e, false
		}
		pat, _ := s("pattern")
		p, ok := s("path")
		if !ok || p == "" {
			p = "."
		}
		g, _ := s("glob")
		lit := args["literal"] == true || args["literal"] == "true"
		return t.grep(ctx, pat, t.path(p), g, lit)
	case "ls":
		p, ok := s("path")
		if !ok || p == "" {
			p = "."
		}
		return t.ls(t.path(p))
	case "read":
		if e := need("path"); e != "" {
			return e, false
		}
		p, _ := s("path")
		lim := n("limit", readDefault)
		if args["offset"] == nil && args["limit"] == nil {
			lim = readAuto
		}
		out, ok := t.read(t.path(p), n("offset", 1), lim)
		if !ok && strings.Contains(out, "no such file") {
			// One clear candidate: read it at once, so the guess does not
			// cost a step. Several: list them.
			m := t.findByName(filepath.Base(p))
			note := "the only file with that name"
			if len(m) == 0 {
				if d := t.findDecl(strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))); len(d) > 0 {
					m = nil
					for _, x := range d {
						m = append(m, x[:strings.LastIndex(x, ":")])
					}
					note = "the file that declares " + strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)) + " (line " + d[0][strings.LastIndex(d[0], ":")+1:] + ")"
				}
			}
			switch {
			case len(m) == 1:
				if o2, ok2 := t.read(t.path(m[0]), n("offset", 1), n("limit", readDefault)); ok2 {
					return "note: " + p + " does not exist. This is " + m[0] + ", " + note + ".\n" + o2, true
				}
			case len(m) > 1:
				out += "\ndid you mean: " + strings.Join(m, ", ")
			}
		}
		return out, ok
	case "write":
		if e := need("path", "content"); e != "" {
			return e, false
		}
		p, _ := s("path")
		c, _ := s("content")
		out, ok := t.write(t.path(p), p, c)
		return t.withDiag(out, ok, t.path(p), p)
	case "edit":
		// Names other agents use for the same fields.
		for alias, k := range map[string]string{"old_string": "old", "new_string": "new", "old_text": "old", "new_text": "new",
			"oldText": "old", "newText": "new", "search": "old", "replace": "new", "file_path": "path", "file": "path"} {
			if _, ok := args[k]; !ok && args[alias] != nil {
				args[k] = args[alias]
			}
		}
		if _, hasOld := s("old"); !hasOld {
			if c, ok := s("content"); ok {
				// edit with a whole file in content: that is a write.
				if e := need("path"); e != "" {
					return e + `. Call write {"path": "...", "content": "..."}.`, false
				}
				p, _ := s("path")
				out, ok := t.write(t.path(p), p, c)
				out, ok = t.withDiag(out, ok, t.path(p), p)
				return "note: edit got content and no old, so qh wrote the whole file; use write for that.\n" + out, ok
			}
		}
		p, hasPath := s("path")
		o, _ := s("old")
		note := ""
		if !hasPath && o != "" {
			// Qwen sometimes leaves out path. When one file holds old, use it.
			files := t.filesWith(ctx, o)
			switch len(files) {
			case 1:
				p, args["path"], note = files[0], files[0], "note: path was missing; old is only in "+files[0]+", so qh used it. Always give path.\n"
			case 0:
				return `error: edit needs the "path" argument, and old is in no file of the project. Call edit {"path": "...", "old": "...", "new": "..."}.`, false
			default:
				if len(files) > 10 {
					files = files[:10]
				}
				return `error: edit needs the "path" argument. old is in more than one file: ` + strings.Join(files, ", ") + `. Call edit {"path": "...", "old": "...", "new": "..."}.`, false
			}
		}
		if e := need("path", "old", "new"); e != "" {
			return e + `. Call edit {"path": "...", "old": "...", "new": "..."}.`, false
		}
		nw, _ := s("new")
		all := args["all"] == true || args["all"] == "true"
		out, ok := t.edit(t.path(p), p, o, nw, all, n("line", 0))
		out, ok = t.withDiag(out, ok, t.path(p), p)
		return note + out, ok
	case "bash":
		if e := need("command"); e != "" {
			return e, false
		}
		c, _ := s("command")
		to := n("timeout", 120)
		if to <= 0 || to > 900 {
			to = 900
		}
		if args["background"] == true || args["background"] == "true" {
			if t.BG == nil {
				return "error: background commands are not available here", false
			}
			return t.BG.Start(t.Cwd, c)
		}
		before := t.mtimes()
		defer t.refreshChanged(before)
		snap := t.Undo.bashBefore()
		defer t.Undo.bashAfter(snap)
		return t.bash(ctx, c, time.Duration(to)*time.Second)
	case "bg":
		if t.BG == nil {
			return "error: background commands are not available here", false
		}
		if e := need("id"); e != "" {
			return e, false
		}
		id, _ := s("id")
		act, _ := s("action")
		return t.BG.Run(strings.TrimSpace(id), strings.TrimSpace(act))
	}
	if t.ReadOnly {
		return fmt.Sprintf("error: unknown tool %q; the tools are read, grep and ls", name), false
	}
	return fmt.Sprintf("error: unknown tool %q; the tools are read, write, edit, bash, bg, grep, glob, todo", name), false
}

func (t *Tools) ls(p string) (string, bool) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return "error: " + err.Error(), false
	}
	var b strings.Builder
	for i, e := range ents {
		if i == 1000 {
			fmt.Fprintf(&b, "[%d more]\n", len(ents)-i)
			break
		}
		b.WriteString(e.Name())
		if e.IsDir() {
			b.WriteByte('/')
		}
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "(empty)", true
	}
	return b.String(), true
}

func (t *Tools) read(fp string, off, limit int) (string, bool) {
	b, err := os.ReadFile(fp)
	if err != nil {
		return "error: " + err.Error(), false
	}
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return fmt.Sprintf("error: %s is a binary file (%d bytes)", fp, len(b)), false
	}
	t.saw(fp, string(b))
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if off < 1 {
		off = 1
	}
	big := limit == readAuto && len(lines) > readDefault
	switch {
	case big:
		limit = readHead
	case limit <= 0:
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
	if big && end < len(lines) {
		if o := outline(lines, end+1, 80); len(o) > 0 {
			fmt.Fprintf(&out, "[outline of lines %d-%d; read with offset for the part you need]\n%s\n", end+1, len(lines), strings.Join(o, "\n"))
		}
	}
	return out.String(), true
}

func (t *Tools) write(fp, shown, c string) (string, bool) {
	if e := t.stale(fp, shown, true); e != "" {
		return e, false
	}
	prev, hadFile := os.ReadFile(fp)
	t.Undo.keep(fp)
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return "error: " + err.Error(), false
	}
	if err := os.WriteFile(fp, []byte(c), 0o644); err != nil {
		return "error: " + err.Error(), false
	}
	t.saw(fp, c)
	n := strings.Count(c, "\n") + 1
	if hadFile != nil {
		return fmt.Sprintf("wrote %s (%d lines, new file)", shown, n), true
	}
	if string(prev) == c {
		return fmt.Sprintf("wrote %s (%d lines, no change)", shown, n), true
	}
	add, del := lineDiff(string(prev), c)
	return fmt.Sprintf("wrote %s (%d lines; vs before: %d lines added, %d removed)", shown, n, add, del), true
}

// lineDiff counts the lines that b adds to a and the lines it removes, by
// line content (order is not compared).
func lineDiff(a, b string) (add, del int) {
	cnt := map[string]int{}
	for _, l := range strings.Split(a, "\n") {
		cnt[l]++
	}
	for _, l := range strings.Split(b, "\n") {
		if cnt[l] > 0 {
			cnt[l]--
		} else {
			add++
		}
	}
	for _, v := range cnt {
		del += v
	}
	return add, del
}

func (t *Tools) edit(fp, shown, old, nw string, all bool, near int) (string, bool) {
	if e := t.stale(fp, shown, false); e != "" {
		return e, false
	}
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
		return "error: old text not found in " + shown + ". " + nearest(src, old), false
	case cnt > 1 && !all && near <= 0:
		return fmt.Sprintf("error: old text appears %d times in %s (lines %s). Pass line=N to change the match at line N (one call per match), set all=true to change every match, or add a line around old that makes it unique. The matches with the lines around them:\n%s",
			cnt, shown, lineNums(src, old), matchContext(src, old)), false
	}
	at := strings.Index(src, old)
	if cnt > 1 && !all {
		// line picks the match that starts closest to it.
		bestD := -1
		for off := 0; ; {
			i := strings.Index(src[off:], old)
			if i < 0 {
				break
			}
			l := strings.Count(src[:off+i], "\n") + 1
			if d := max(l-near, near-l); bestD < 0 || d < bestD {
				at, bestD = off+i, d
			}
			off += i + len(old)
		}
	}
	line := strings.Count(src[:at], "\n") + 1
	if all {
		src = strings.ReplaceAll(src, old, nw)
	} else {
		src = src[:at] + nw + src[at+len(old):]
	}
	t.Undo.keep(fp)
	if err := os.WriteFile(fp, []byte(src), 0o644); err != nil {
		return "error: " + err.Error(), false
	}
	t.saw(fp, src)
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

// nearest finds the part of src that is most like old and shows it with line
// numbers, so the model can copy the right text without reading the file again.
func nearest(src, old string) string {
	want := strings.Split(strings.TrimRight(old, "\n"), "\n")
	lines := strings.Split(src, "\n")
	idx := map[string][]int{}
	for i, l := range lines {
		if k := strings.TrimSpace(l); k != "" {
			idx[k] = append(idx[k], i)
		}
	}
	votes := map[int]int{}
	for j, w := range want {
		for _, i := range idx[strings.TrimSpace(w)] {
			votes[i-j]++
		}
	}
	best, n := 0, 0
	for start, v := range votes {
		if v > n || (v == n && start < best) {
			best, n = start, v
		}
	}
	if n == 0 {
		// No line matches exactly: vote with lines that are nearly the same.
		for j, w := range want {
			w = strings.TrimSpace(w)
			if len(w) < 8 || len(lines) > 5000 {
				continue
			}
			bi, bs := -1, 0.0
			for i, l := range lines {
				if sc := similar(w, strings.TrimSpace(l)); sc > bs {
					bi, bs = i, sc
				}
			}
			if bs >= 0.7 {
				votes[bi-j]++
			}
		}
		for start, v := range votes {
			if v > n || (v == n && start < best) {
				best, n = start, v
			}
		}
		if n == 0 {
			return "No line of old is in the file, not even nearly. The file has changed or old is from another file. Read the file again and copy the text exactly."
		}
		n = 0 // the header below then says the lines only look alike
	}
	from := max(0, best-2)
	to := min(len(lines), best+len(want)+2, from+60)
	var b strings.Builder
	if n == 0 {
		fmt.Fprintf(&b, "No line of old matches exactly, but lines %d-%d look like it:\n", from+1, to)
	} else {
		fmt.Fprintf(&b, "%d of the %d lines of old match. The closest part of the file is lines %d-%d:\n", n, len(want), from+1, to)
	}
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
	}
	b.WriteString("Copy old from these lines, without the line numbers.")
	return b.String()
}

// findByName lists up to 5 project files with the given base name.
func (t *Tools) findByName(base string) []string {
	var out []string
	cmd := exec.Command("git", "ls-files", "-co", "--exclude-standard")
	cmd.Dir = t.Cwd
	if b, err := cmd.Output(); err == nil {
		for _, f := range strings.Split(string(b), "\n") {
			if filepath.Base(f) == base {
				out = append(out, f)
			}
		}
	} else {
		filepath.WalkDir(t.Cwd, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "build" || d.Name() == "target") {
				return filepath.SkipDir
			}
			if d.Name() == base && !d.IsDir() {
				rel, _ := filepath.Rel(t.Cwd, p)
				out = append(out, rel)
			}
			return nil
		})
	}
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// findDecl finds where a class, interface, object, enum, record, struct or
// type with this name is declared (for files that hold several types).
func (t *Tools) findDecl(name string) []string {
	if name == "" || !regexp.MustCompile(`^[A-Za-z_]\w*$`).MatchString(name) {
		return nil
	}
	pat := `\b(class|interface|object|enum|record|struct|trait|type)\s+` + name + `\b`
	cmd := exec.Command("git", "grep", "-nIE", pat)
	cmd.Dir = t.Cwd
	b, err := cmd.Output()
	if err != nil {
		cmd = exec.Command("grep", "-rnIE", "--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=build", pat, ".")
		cmd.Dir = t.Cwd
		b, _ = cmd.Output()
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if parts := strings.SplitN(l, ":", 3); len(parts) == 3 && len(out) < 5 {
			out = append(out, strings.TrimPrefix(parts[0], "./")+":"+parts[1])
		}
	}
	return out
}

// filesWith lists the files under the working directory that contain old.
func (t *Tools) filesWith(ctx context.Context, old string) []string {
	var cands []string
	cmd := exec.CommandContext(ctx, "git", "ls-files", "-co", "--exclude-standard")
	cmd.Dir = t.Cwd
	if out, err := cmd.Output(); err == nil {
		cands = strings.Split(strings.TrimSpace(string(out)), "\n")
	} else {
		filepath.WalkDir(t.Cwd, func(p string, d os.DirEntry, err error) error {
			if err != nil || len(cands) > 20000 {
				return filepath.SkipDir
			}
			if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "build" || d.Name() == "target") {
				return filepath.SkipDir
			}
			if !d.IsDir() {
				rel, _ := filepath.Rel(t.Cwd, p)
				cands = append(cands, rel)
			}
			return nil
		})
	}
	var out []string
	for _, c := range cands {
		if c == "" {
			continue
		}
		st, err := os.Stat(filepath.Join(t.Cwd, c))
		if err != nil || st.IsDir() || st.Size() > 2<<20 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(t.Cwd, c))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), old) {
			out = append(out, c)
		} else if _, ok := looseFind(string(b), old); ok {
			out = append(out, c)
		}
	}
	return out
}

// matchContext shows up to 5 matches of sub with one line before and after.
func matchContext(src, sub string) string {
	lines := strings.Split(src, "\n")
	n := strings.Count(sub, "\n") + 1
	var b strings.Builder
	off := 0
	for k := 0; k < 5; k++ {
		i := strings.Index(src[off:], sub)
		if i < 0 {
			break
		}
		start := strings.Count(src[:off+i], "\n")
		for l := max(0, start-1); l < min(len(lines), start+n+1); l++ {
			fmt.Fprintf(&b, "%d\t%s\n", l+1, lines[l])
		}
		b.WriteString("--\n")
		off += i + len(sub)
	}
	return strings.TrimSuffix(b.String(), "--\n")
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
	start := time.Now().Add(-time.Second)
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
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	raw, dropped := out.String(), 0
	if code == 0 && ctx.Err() == nil {
		raw, dropped = dropBuildNoise(command, raw)
	}
	res := clip(raw)
	if dropped > 0 {
		res = strings.TrimRight(res, "\n") + fmt.Sprintf("\n[qh: %d Gradle/Maven progress and task lines left out]", dropped)
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
	// grep and rg exit 1 when nothing matches; that is an answer, not an error.
	if code == 1 && strings.TrimSpace(out.String()) == "" && searchCmd.MatchString(command) {
		return "no matches [exit 1]", true
	}
	if code != 0 && bsdSed.MatchString(out.String()) && strings.Contains(command, "sed -i") {
		res = strings.TrimRight(res, "\n") + "\n" + bsdSedHint
	}
	if code != 0 && badPattern.MatchString(out.String()) {
		res = strings.TrimRight(res, "\n") + "\n" + patternHint
	}
	if errs := compilerErrors(out.String(), t.Cwd); len(errs) > 0 && (code != 0 || testCmd.MatchString(command)) {
		res = strings.TrimRight(res, "\n") + "\n\n[qh: compiler errors]\n" + strings.Join(errs, "\n")
		if imps := importHints(errs); len(imps) > 0 {
			res += "\nThese unresolved names usually need these imports:\n" + strings.Join(imps, "\n")
		}
	}
	// A test command piped into tail exits 0 even when tests fail, so test
	// commands get the report check whatever their exit code.
	if code != 0 || testCmd.MatchString(command) {
		sum, f := junitFailures(t.Cwd, start)
		if f != "" {
			res = strings.TrimRight(res, "\n") + "\n\n[qh: failing tests from the JUnit XML reports]\n" + f
		}
		if sum != "" {
			res = strings.TrimRight(res, "\n") + "\n[qh: tests: " + sum + "]"
		}
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

var compErr = regexp.MustCompile(`^e: |^\S+\.\w+:\d+(:\d+)?: (fatal )?error:|^\[ERROR\] \S+\.(java|kt|scala):\[\d+`)

// compilerErrors returns every compiler error line in out (Kotlin "e: ",
// javac/gcc/go "file:line: error:", Maven "[ERROR] File.java:[l,c]"), with
// file:// and the working directory cut off the paths. At most 40.
func compilerErrors(out, cwd string) []string {
	var errs []string
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if !compErr.MatchString(l) {
			continue
		}
		l = strings.TrimPrefix(l, "e: ")
		l = strings.TrimPrefix(l, "file://")
		l = strings.ReplaceAll(l, cwd+"/", "")
		if !seen[l] {
			seen[l] = true
			errs = append(errs, oneLine(l, 300))
		}
	}
	if len(errs) > 40 {
		errs = append(errs[:40], fmt.Sprintf("... and %d more", len(errs)-40))
	}
	return errs
}

var unresolved = regexp.MustCompile(`Unresolved reference:? '?(\w+)'?`)

// testImports maps names from common Kotlin test DSLs to their import.
// The model loses many steps hunting for these in jars.
var testImports = map[string]string{
	"every": "io.mockk.every", "coEvery": "io.mockk.coEvery", "verify": "io.mockk.verify",
	"coVerify": "io.mockk.coVerify", "mockk": "io.mockk.mockk", "spyk": "io.mockk.spyk",
	"slot": "io.mockk.slot", "just": "io.mockk.just", "Runs": "io.mockk.Runs",
	"confirmVerified": "io.mockk.confirmVerified", "clearMocks": "io.mockk.clearMocks",
	"verifyOrder": "io.mockk.verifyOrder", "verifySequence": "io.mockk.verifySequence",
	"MockKExtension": "io.mockk.junit5.MockKExtension", "MockK": "io.mockk.impl.annotations.MockK",
	"InjectMockKs": "io.mockk.impl.annotations.InjectMockKs", "SpykBean": "com.ninjasquad.springmockk.SpykBean",
	"MockkBean":                 "com.ninjasquad.springmockk.MockkBean",
	"assertThat":                "org.assertj.core.api.Assertions.assertThat",
	"assertThatThrownBy":        "org.assertj.core.api.Assertions.assertThatThrownBy",
	"assertThatCode":            "org.assertj.core.api.Assertions.assertThatCode",
	"assertThatExceptionOfType": "org.assertj.core.api.Assertions.assertThatExceptionOfType",
	"assertThrows":              "org.junit.jupiter.api.assertThrows", "assertEquals": "org.junit.jupiter.api.Assertions.assertEquals",
	"assertTrue": "org.junit.jupiter.api.Assertions.assertTrue", "assertFalse": "org.junit.jupiter.api.Assertions.assertFalse",
	"assertNull": "org.junit.jupiter.api.Assertions.assertNull", "assertNotNull": "org.junit.jupiter.api.Assertions.assertNotNull",
	"jsonPath": "org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath",
	"status":   "org.springframework.test.web.servlet.result.MockMvcResultMatchers.status",
}

// importHints lists the import lines for unresolved names that qh knows.
func importHints(errs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range errs {
		for _, m := range unresolved.FindAllStringSubmatch(e, -1) {
			if imp, ok := testImports[m[1]]; ok && !seen[imp] {
				seen[imp] = true
				out = append(out, "import "+imp)
			}
		}
	}
	return out
}

var bsdSed = regexp.MustCompile(`sed: \d+: .*(invalid command code|extra characters|unknown option|undefined label|expects \\ followed by text|unterminated)|sed: -I or -i may not be used`)

const bsdSedHint = "[qh: this is BSD sed (macOS). Its -i takes a suffix argument: use sed -i '' 's/a/b/' FILE, or the edit tool. Do not use -i.bak, it leaves a backup file in the tree.]"

// badPattern matches the errors grep and rg give for a broken regular
// expression, which is usually a literal search with ( [ { or * in it.
var badPattern = regexp.MustCompile(`(?i)parentheses not balanced|brackets \(\[ \]\) not balanced|unmatched [(\[{]|invalid (regular expression|preceding regular expression|content of)|regex parse error|trailing backslash|invalid range end`)

const patternHint = "[qh: the pattern is a regular expression and ( [ { * . have meanings there. For a literal search use grep -F (or rg -F), or escape them.]"

var searchCmd = regexp.MustCompile(`(^|[|;&]\s*|\s)(grep|egrep|fgrep|rg|ag|ack)\s`)

var testCmd = regexp.MustCompile(`(?i)\b(test|tests|check|verify|gradlew?|mvnw?|integrationTest)\b`)

type junitCase struct {
	Name    string    `xml:"name,attr"`
	Class   string    `xml:"classname,attr"`
	Skipped *struct{} `xml:"skipped"`
	Failure *struct {
		Message string `xml:"message,attr"`
		Text    string `xml:",chardata"`
	} `xml:"failure"`
	Error *struct {
		Message string `xml:"message,attr"`
		Text    string `xml:",chardata"`
	} `xml:"error"`
}

// junitFailures reads the JUnit XML reports (Gradle test-results, Maven
// surefire and failsafe) written since start and lists the failing tests with
// their message and the top of the stack. The model then does not need to
// dig in build/ for them.
func junitFailures(root string, start time.Time) (string, string) {
	var files []string
	seen := 0
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > 200000 {
			return filepath.SkipAll
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".gradle", ".idea", "classes", "kotlin", "tmp", "libs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".xml") || !(strings.Contains(p, "test-results") || strings.Contains(p, "surefire-reports") || strings.Contains(p, "failsafe-reports")) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(start) {
			files = append(files, p)
		}
		return nil
	})
	var b strings.Builder
	n, total, skipped := 0, 0, 0
	perClass := map[string]int{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		dec := xml.NewDecoder(bytes.NewReader(data))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			se, ok := tok.(xml.StartElement)
			if !ok || se.Name.Local != "testcase" {
				continue
			}
			var c junitCase
			if dec.DecodeElement(&c, &se) != nil {
				continue
			}
			total++
			perClass[c.Class[strings.LastIndex(c.Class, ".")+1:]]++
			if c.Skipped != nil {
				skipped++
				continue
			}
			msg, text := "", ""
			if c.Failure != nil {
				msg, text = c.Failure.Message, c.Failure.Text
			} else if c.Error != nil {
				msg, text = c.Error.Message, c.Error.Text
			} else {
				continue
			}
			n++
			if n > 10 {
				continue
			}
			fmt.Fprintf(&b, "FAILED %s.%s: %s\n", c.Class, c.Name, oneLine(msg, 300))
			lines := strings.Split(strings.TrimSpace(text), "\n")
			shown := 0
			for _, l := range lines[min(1, len(lines)):] {
				if l = strings.TrimSpace(l); strings.HasPrefix(l, "at ") && shown < 4 {
					b.WriteString("    " + l + "\n")
					shown++
				}
			}
			if m := statusMismatch.FindStringSubmatch(msg + "\n" + text); m != nil {
				if body := responseBody(text + "\n" + string(data)); body != "" {
					b.WriteString("    response body: " + oneLine(body, 300) + "\n")
					if strings.Contains(body, m[1]) {
						fmt.Fprintf(&b, "    [qh: the body says %s but the status was %s, so the handler ran and the code that writes the response does not set the status]\n", m[1], m[2])
					}
				}
			}
		}
	}
	if n > 10 {
		fmt.Fprintf(&b, "... and %d more failing tests\n", n-10)
	}
	sum := ""
	if total > 0 {
		sum = fmt.Sprintf("%d passed, %d failed, %d skipped", total-n-skipped, n, skipped)
		names := make([]string, 0, len(perClass))
		for c := range perClass {
			names = append(names, c)
		}
		sort.Strings(names)
		if len(names) <= 12 {
			var parts []string
			for _, c := range names {
				parts = append(parts, fmt.Sprintf("%s %d", c, perClass[c]))
			}
			sum += " (" + strings.Join(parts, ", ") + ")"
		} else {
			sum += fmt.Sprintf(" in %d classes", len(names))
		}
	}
	return sum, strings.TrimRight(b.String(), "\n")
}

var statusMismatch = regexp.MustCompile(`(?i)status\s*(?:code)?\s*expected:?\s*<?\[?(\d{3})[^\n]*?but was:?\s*<?\[?(\d{3})`)

var bodyLine = regexp.MustCompile(`(?m)^\s*(?:Body\s*=\s*|<\s*)?(\{.*\})\s*$`)

// responseBody finds the first JSON body in a test failure or its output,
// such as MockMvc's "Body = {...}" or WebTestClient's "< {...}".
func responseBody(s string) string {
	s = strings.NewReplacer("&quot;", `"`, "&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
	if m := bodyLine.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// similar is the Dice coefficient of the character pairs of a and b (0 to 1).
func similar(a, b string) float64 {
	if a == b {
		return 1
	}
	if len(a) < 2 || len(b) < 2 {
		return 0
	}
	pairs := map[string]int{}
	for i := 0; i+2 <= len(a); i++ {
		pairs[a[i:i+2]]++
	}
	hit := 0
	for i := 0; i+2 <= len(b); i++ {
		if pairs[b[i:i+2]] > 0 {
			pairs[b[i:i+2]]--
			hit++
		}
	}
	return 2 * float64(hit) / float64(len(a)-1+len(b)-1)
}
