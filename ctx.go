package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
	"sort"
	"strings"
)

// ctxByTool counts the tokens (chars/4) that the results of each tool hold
// in the messages, so a run can show what fills its context.
func ctxByTool(msgs []Message) map[string]int {
	m := map[string]int{}
	for _, x := range msgs {
		if x.Role == "tool" {
			m[cmpOr(x.Name, "?")] += len(x.Content) / 4
		}
	}
	return m
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ctxTop is "read 41%, bash 33%, grep 9%": the three tools whose results
// take the most of the context of total tokens.
func ctxTop(m map[string]int, total int) string {
	if total <= 0 {
		return ""
	}
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		if v > 0 {
			l = append(l, kv{k, v})
		}
	}
	sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v || l[i].v == l[j].v && l[i].k < l[j].k })
	var out []string
	for i, x := range l {
		if i == 3 {
			break
		}
		out = append(out, fmt.Sprintf("%s %d%%", x.k, x.v*100/total))
	}
	return strings.Join(out, ", ")
}

// readMark is one read whose full result the model got.
type readMark struct {
	callID string // the tool message that holds it
	run    int    // Session.runs at the time
	step   int
	lines  int  // lines in the file
	whole  bool // the result held the whole file
	hash   uint64
	size   int64
	asked  bool // the last answer for this key was "unchanged"
}

func fileHash(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

type readArgs struct {
	Path   string `json:"path"`
	Offset any    `json:"offset"`
	Limit  any    `json:"limit"`
}

func intArg(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		var n int
		fmt.Sscanf(strings.TrimSpace(x), "%d", &n)
		return n
	}
	return 0
}

// readKey is the file and range of a read call.
func readKey(t *Tools, raw string) (fp, key string, ranged bool) {
	var a readArgs
	if json.Unmarshal([]byte(raw), &a) != nil || a.Path == "" {
		return "", "", false
	}
	fp = t.path(a.Path)
	off, lim := intArg(a.Offset), intArg(a.Limit)
	if off < 1 {
		off = 1
	}
	return fp, fmt.Sprintf("%s\x00%d\x00%d", fp, off, lim), off > 1 || lim > 0
}

func inContext(msgs []Message, callID string) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" && msgs[i].ToolCallID == callID {
			return true
		}
	}
	return false
}

// reread answers a read of a file the model already has in its context, in
// the same range or as a whole, when the file has not changed since. It
// returns "" when the read should go through. A second ask in a row gets the
// content.
func (s *Session) reread(t *Tools, name, raw string, step int) string {
	if name != "read" {
		return ""
	}
	fp, key, ranged := readKey(t, raw)
	if key == "" || s.reads == nil {
		return ""
	}
	m := s.reads[key]
	whole := false
	if m == nil && ranged {
		m, whole = s.reads[fp+"\x00whole"], true
	}
	if m == nil || !inContext(s.Msgs, m.callID) {
		return ""
	}
	fi, err := os.Stat(fp)
	if err != nil || fi.Size() != m.size {
		return ""
	}
	b, err := os.ReadFile(fp)
	if err != nil || fileHash(b) != m.hash {
		return ""
	}
	if m.asked {
		m.asked = false
		return ""
	}
	m.asked = true
	when := fmt.Sprintf("at step %d", m.step)
	if m.run != s.runs {
		when = "in an earlier turn"
	}
	if whole {
		return fmt.Sprintf("unchanged since your read of the whole file %s (%d lines); those lines are above. Read again to get them anyway.", when, m.lines)
	}
	return fmt.Sprintf("unchanged since your read %s (%d lines); read with offset/limit for part of it, or read again to get it anyway.", when, m.lines)
}

var wholeRe = regexp.MustCompile(`\[lines \d+-\d+ of \d+;`)

// sawRead notes a read result that went to the model.
func (s *Session) sawRead(t *Tools, raw, callID, out string, step int) {
	fp, key, ranged := readKey(t, raw)
	if key == "" || strings.HasPrefix(out, "note: ") {
		return // a read that went to another file than asked
	}
	b, err := os.ReadFile(fp)
	if err != nil {
		return
	}
	if s.reads == nil {
		s.reads = map[string]*readMark{}
	}
	n := strings.Count(string(b), "\n")
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	m := &readMark{callID: callID, run: s.runs, step: step, lines: n, hash: fileHash(b), size: int64(len(b))}
	m.whole = !ranged && !wholeRe.MatchString(out)
	s.reads[key] = m
	if m.whole {
		s.reads[fp+"\x00whole"] = m
	}
}

// outlineRe finds declarations: func, fun, def, class and the like, and
// Java or C# methods.
var outlineRe = regexp.MustCompile(`^\s*(?:(?:export|public|private|protected|internal|static|final|abstract|open|override|suspend|inline|async|data|sealed|enum|pub(?:\([a-z]+\))?|unsafe|default)\s+)*(?:func|fun|def|class|interface|object|record|struct|trait|impl|fn|type|enum|module|function)\b|^\s*(?:public|private|protected)\s+[\w<>\[\],.? ]+\s+\w+\s*\(`)

// outline lists the declaration lines from line from on, at most max.
func outline(lines []string, from, max int) []string {
	var out []string
	n := 0
	for i := from - 1; i < len(lines); i++ {
		if !outlineRe.MatchString(lines[i]) {
			continue
		}
		if n++; len(out) < max {
			l := strings.TrimSpace(lines[i])
			if len(l) > 120 {
				l = l[:120] + "..."
			}
			out = append(out, fmt.Sprintf("%d\t%s", i+1, l))
		}
	}
	if n > len(out) {
		out = append(out, fmt.Sprintf("[%d more declarations]", n-len(out)))
	}
	return out
}

// buildNoise matches lines a successful Gradle or Maven build prints that
// say nothing: task headers, downloads and progress.
var buildNoise = regexp.MustCompile(`^(> Task :\S+( (UP-TO-DATE|NO-SOURCE|SKIPPED|FROM-CACHE))?\s*$|> Configure project|<[-=]+> \d+% (EXECUTING|CONFIGURING|INITIALIZING)|Download(ing)? https?://|\[INFO\] Download(ing|ed) from |Progress \(\d+\): |Starting a Gradle Daemon|\[INFO\] -+\s*$|\[INFO\] ?\s*$)`)

var buildCmd = regexp.MustCompile(`\b(gradlew?|mvnw?)\b`)

// dropBuildNoise removes noise lines from the output of a Gradle or Maven
// command that passed. It returns the output and the number dropped.
func dropBuildNoise(command, out string) (string, int) {
	if !buildCmd.MatchString(command) {
		return out, 0
	}
	lines := strings.Split(out, "\n")
	keep := lines[:0]
	n := 0
	for _, l := range lines {
		if buildNoise.MatchString(strings.TrimRight(l, "\r")) {
			n++
			continue
		}
		keep = append(keep, l)
	}
	if n == 0 {
		return out, 0
	}
	return strings.Join(keep, "\n"), n
}
