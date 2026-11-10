package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	readTail  = regexp.MustCompile(`\[lines (\d+)-(\d+) of (\d+);`)
	wroteNums = regexp.MustCompile(`(\d+) lines added, (\d+) removed`)
	wroteNew  = regexp.MustCompile(`\((\d+) lines, new file\)`)
	exitCode  = regexp.MustCompile(`\[exit (-?\d+)\]\s*$`)
)

// shortResult is the result of a tool call in a few words, for the one-line
// view of the call in the chat: "49 lines", "3 hits", "+2 −2", "ok", "exit 1".
func shortResult(tool, args, out string, ok bool) string {
	if !ok {
		if m := exitCode.FindStringSubmatch(strings.TrimSpace(out)); m != nil {
			return "exit " + m[1]
		}
		return "failed"
	}
	count := func() int {
		n := 0
		for _, l := range strings.Split(out, "\n") {
			if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "[") {
				n++
			}
		}
		return n
	}
	switch tool {
	case "read":
		if m := readTail.FindStringSubmatch(out); m != nil {
			return fmt.Sprintf("%s-%s of %s", m[1], m[2], m[3])
		}
		n := 0
		for _, l := range strings.Split(out, "\n") {
			if i := strings.IndexByte(l, '\t'); i > 0 && strings.Trim(l[:i], "0123456789") == "" {
				n++
			}
		}
		return plural(n, "line")
	case "grep":
		if strings.TrimSpace(out) == "" || strings.HasPrefix(out, "no match") {
			return "no match"
		}
		return plural(count(), "hit")
	case "web_search":
		if strings.HasPrefix(out, "no results") {
			return "no results"
		}
		return plural(strings.Count(out, "\n   http"), "result")
	case "fetch_url":
		return fmt.Sprintf("%d chars", len([]rune(out)))
	case "glob", "ls":
		return plural(count(), "entry")
	case "edit":
		var a map[string]any
		json.Unmarshal([]byte(args), &a)
		str := func(ks ...string) string {
			for _, k := range ks {
				if v, ok := a[k].(string); ok {
					return v
				}
			}
			return ""
		}
		return fmt.Sprintf("+%d −%d", lineCount(str("new", "new_string", "replace")), lineCount(str("old", "old_string", "search")))
	case "write":
		if m := wroteNums.FindStringSubmatch(out); m != nil {
			return "+" + m[1] + " −" + m[2]
		}
		if m := wroteNew.FindStringSubmatch(out); m != nil {
			return "new, " + m[1] + " lines"
		}
		return "ok"
	}
	return "ok"
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

func plural(n int, w string) string {
	if n == 1 {
		return "1 " + w
	}
	if strings.HasSuffix(w, "y") {
		return fmt.Sprintf("%d %sies", n, w[:len(w)-1])
	}
	return fmt.Sprintf("%d %ss", n, w)
}
