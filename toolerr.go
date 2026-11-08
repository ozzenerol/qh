package main

import (
	"fmt"
	"regexp"
	"strings"
)

// indentFind finds old in src when only the indentation differs by the same
// prefix on every line. It returns the text in src, new indented the same
// way, and a note for the model. Exactly one place must match.
func indentFind(src, old, nw string) (string, string, string, bool) {
	want := strings.Split(strings.TrimRight(old, "\n"), "\n")
	for i := range want {
		want[i] = strings.TrimRight(want[i], " \t\r")
	}
	if len(want) == 0 || strings.TrimSpace(strings.Join(want, "")) == "" {
		return "", "", "", false
	}
	lines := strings.SplitAfter(src, "\n")
	var match, extra string
	add, at, found := false, 0, 0
	for i := 0; i+len(want) <= len(lines) && found < 2; i++ {
		ex, ad, ok := "", false, true
		set := false
		for j, w := range want {
			l := strings.TrimRight(lines[i+j], " \t\r\n")
			if strings.TrimSpace(l) != strings.TrimSpace(w) {
				ok = false
				break
			}
			if strings.TrimSpace(w) == "" {
				continue
			}
			fl, wl := lead(l), lead(w)
			var e string
			var a bool
			switch {
			case strings.HasSuffix(fl, wl):
				e, a = fl[:len(fl)-len(wl)], true
			case strings.HasSuffix(wl, fl):
				e, a = wl[:len(wl)-len(fl)], false
			default:
				ok = false
			}
			if !ok {
				break
			}
			if !set {
				ex, ad, set = e, a, true
			} else if e != ex || (e != "" && a != ad) {
				ok = false
				break
			}
		}
		if ok && ex != "" {
			found++
			match, extra, add, at = strings.TrimSuffix(strings.Join(lines[i:i+len(want)], ""), "\n"), ex, ad, i+1
		}
	}
	if found != 1 {
		return "", "", "", false
	}
	nl := strings.Split(nw, "\n")
	for i, l := range nl {
		switch {
		case strings.TrimSpace(l) == "":
		case add:
			nl[i] = extra + l
		case strings.HasPrefix(l, extra):
			nl[i] = l[len(extra):]
		default:
			return "", "", "", false // new has a line qh cannot outdent exactly
		}
	}
	way := "more"
	if !add {
		way = "less"
	}
	note := fmt.Sprintf("note: old matched lines %d-%d with %s indentation (%q per line); qh changed the indentation of new the same way.\n", at, at+len(want)-1, way, extra)
	return match, strings.Join(nl, "\n"), note, true
}

func lead(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// wholeLine returns the offset of the only match of old that covers whole
// lines (indentation before it allowed), when old also appears inside
// longer lines (an import of User and of UserRepository).
func wholeLine(src, old string) (int, bool) {
	at, n := -1, 0
	for off := 0; ; {
		i := strings.Index(src[off:], old)
		if i < 0 {
			break
		}
		i += off
		end := i + len(old)
		ls := strings.LastIndexByte(src[:i], '\n') + 1
		start := strings.TrimLeft(src[ls:i], " \t") == "" || strings.HasPrefix(old, "\n")
		stop := end == len(src) || src[end] == '\n' || src[end] == '\r' || strings.HasSuffix(old, "\n")
		if start && stop {
			at, n = i, n+1
		}
		off = i + 1
	}
	return at, n == 1
}

var misuseOut = regexp.MustCompile(`(?im)command not found|^.*\bcd: .*(no such file|not a directory)|unexpected EOF|here-document|syntax error near|unexpected end of file|unrecognized option|invalid option|illegal option|unknown (flag|option|shorthand)`)

// failKind sorts a failed tool call: "misuse" when the call itself was wrong
// (bad arguments, a missing file, an edit that does not match, a shell or
// flag mistake), "failed" when a command ran and exited non-zero (a failing
// build or test is an answer, not a tool error).
func failKind(name, out string) string {
	if strings.HasPrefix(out, "[you made") {
		_, out, _ = strings.Cut(out, "\n")
	}
	if name != "bash" || strings.HasPrefix(out, "error:") {
		return "misuse"
	}
	if strings.Contains(out, "[killed after ") {
		return "misuse"
	}
	if misuseOut.MatchString(lastLines(out, 6)) {
		return "misuse"
	}
	return "failed"
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}
