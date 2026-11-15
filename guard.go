package main

// Checks of the written code against the brief and the notes. They run with
// the final checks; what they find goes back to the model in its fix turn.
//
//   - "never:" lines in the notes or the brief: text that must not appear in
//     the lines the run added (an old package name, a removed API).
//   - API lines in the brief's code blocks (declarations) that no source file
//     in the repo contains.
//   - files changed outside the brief's Change: list.
//   - fully qualified class names in code, enums that copy another enum,
//     private method parameters and locals that are never used,
//     @SuppressWarnings, and the comments the run added, for a cleanup pass.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func (c Config) guardOn() bool { return c.Guard == nil || *c.Guard }

var neverRe = regexp.MustCompile("(?im)^\\s*(?:[-*]|\\d+[.)])?\\s*never:\\s*(.+?)\\s*$")

// neverRules reads the "never:" lines of the texts. A value in backticks is
// taken as written; otherwise the whole rest of the line is the text.
func neverRules(texts ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range texts {
		for _, m := range neverRe.FindAllStringSubmatch(t, -1) {
			v := m[1]
			if i := strings.Index(v, "`"); i >= 0 {
				if j := strings.Index(v[i+1:], "`"); j > 0 {
					v = v[i+1 : i+1+j]
				}
			}
			v = strings.TrimSpace(v)
			if len(v) >= 3 && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// addedLines gives, per changed file, the lines it has now that it did not
// have before the run (counted as a multiset, so moved lines do not count).
type addedLine struct {
	n    int
	text string
}

func addedLines(fs []fileFact) map[string][]addedLine {
	out := map[string][]addedLine{}
	for _, f := range fs {
		if f.kind == "deleted" || f.add < 0 {
			continue
		}
		had := map[string]int{}
		for _, l := range strings.Split(f.before, "\n") {
			had[strings.TrimSpace(l)]++
		}
		for i, l := range strings.Split(f.now, "\n") {
			k := strings.TrimSpace(l)
			if had[k] > 0 {
				had[k]--
				continue
			}
			if k != "" {
				out[f.rel] = append(out[f.rel], addedLine{i + 1, l})
			}
		}
	}
	return out
}

func neverHits(rules []string, added map[string][]addedLine) []string {
	var out []string
	for _, rel := range sortedKeys(added) {
		for _, l := range added[rel] {
			for _, r := range rules {
				if strings.Contains(l.text, r) {
					out = append(out, fmt.Sprintf("%s:%d uses %q, which the notes or the task forbid: %s", rel, l.n, r, strings.TrimSpace(l.text)))
				}
			}
		}
	}
	return out
}

var (
	fenceInfo = regexp.MustCompile("^(\\w*)")
	apiDeclRe = regexp.MustCompile(`^(?:@\w+(?:\([^)]*\))?\s+)*(?:(?:public|protected|private|static|final|abstract|sealed|non-sealed|default|override|suspend|data|open|internal)\s+)*(?:record\s+\w+\s*[(<]|func\s+(?:\([^)]*\)\s*)?\w+\s*[(\[]|(?:class|interface|enum)\s+\w+|[\w<>\[\],.?]+(?:<[^=]*>)?\s+\w+\s*\()`)
	apiSkip   = regexp.MustCompile(`^(?:return|new|if|for|while|switch|case|yield|throw|else|var|val|assert|select|from|where|order|group|limit|insert|update|delete)\b`)
	notCode   = map[string]bool{"sql": true, "yaml": true, "yml": true, "xml": true, "json": true, "bash": true, "sh": true, "text": true, "properties": true, "toml": true, "diff": true}
	spaceRe   = regexp.MustCompile(`\s+`)
)

// apiLines lists the declaration lines in the code blocks of a task: the
// signatures, records and types the code must contain as written.
func apiLines(prompt string) []string {
	parts := strings.Split(prompt, "```")
	var out []string
	seen := map[string]bool{}
	for i := 1; i < len(parts); i += 2 {
		block := parts[i]
		first, rest, _ := strings.Cut(block, "\n")
		if notCode[strings.ToLower(fenceInfo.FindString(strings.TrimSpace(first)))] {
			continue
		}
		inText := false
		for _, l := range strings.Split(rest, "\n") {
			t := strings.TrimSpace(l)
			if strings.Count(t, `"""`)%2 == 1 {
				inText = !inText
				continue
			}
			if inText || t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
				continue
			}
			if i := strings.Index(t, " //"); i > 0 {
				t = strings.TrimSpace(t[:i])
			}
			if apiSkip.MatchString(t) || strings.Contains(t, " = ") || strings.Contains(t, "->") || !apiDeclRe.MatchString(t) {
				continue
			}
			if !strings.HasSuffix(t, "{}") && !strings.HasSuffix(t, "{") && !strings.HasSuffix(t, ";") && !strings.HasSuffix(t, ")") {
				continue
			}
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// apiKey is a declaration with the spaces and the trailing body or
// semicolon removed, so line breaks in the written code do not matter.
func apiKey(s string) string {
	s = spaceRe.ReplaceAllString(s, "")
	for _, suf := range []string{"{}", "{", ";"} {
		s = strings.TrimSuffix(s, suf)
	}
	return s
}

var srcExt = map[string]bool{".java": true, ".kt": true, ".go": true, ".ts": true, ".tsx": true, ".js": true, ".rs": true, ".py": true, ".cs": true, ".swift": true}

// repoSources reads the source files under root, skipping build output and
// dependencies. It stops after max files.
func repoSources(root string, max int) map[string]string {
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "target", "build", "node_modules", "vendor", "dist", ".gradle", ".idea", "out", "bin":
				if p != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !srcExt[filepath.Ext(p)] || len(out) >= max {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && len(b) < 1<<20 {
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

func missingAPI(prompt string, srcs map[string]string) []string {
	want := apiLines(prompt)
	if len(want) == 0 {
		return nil
	}
	var all strings.Builder
	for _, s := range srcs {
		all.WriteString(spaceRe.ReplaceAllString(s, ""))
		all.WriteString("\n")
	}
	have := all.String()
	var out []string
	for _, w := range want {
		if !strings.Contains(have, apiKey(w)) {
			out = append(out, w)
		}
	}
	return out
}

// stripStrings blanks out string literals and text blocks of Java or Kotlin
// source, keeping the line count, so checks on code do not match text.
func stripStrings(src string) string {
	var b strings.Builder
	inText, inStr := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inText {
			if strings.HasPrefix(src[i:], `"""`) {
				inText = false
				i += 2
				b.WriteString(`"""`)
			} else if c == '\n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
			continue
		}
		if inStr {
			if c == '\\' && i+1 < len(src) {
				i++
				b.WriteString("  ")
				continue
			}
			if c == '"' || c == '\n' {
				inStr = false
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
			continue
		}
		if strings.HasPrefix(src[i:], `"""`) {
			inText = true
			i += 2
			b.WriteString(`"""`)
			continue
		}
		if c == '"' {
			inStr = true
		}
		b.WriteByte(c)
	}
	return b.String()
}

var fqnRe = regexp.MustCompile(`\b(?:org|com|java|javax|jakarta|tools|io|net|kotlin|kotlinx)\.(?:[a-z_]\w*\.)+[A-Z]\w*`)

func isJVM(rel string) bool {
	e := filepath.Ext(rel)
	return e == ".java" || e == ".kt"
}

// fqnInCode finds fully qualified class names in the added lines of Java and
// Kotlin files, outside imports, package lines, comments and strings.
func fqnInCode(added map[string][]addedLine, now map[string]string) []string {
	var out []string
	for _, rel := range sortedKeys(added) {
		if !isJVM(rel) {
			continue
		}
		lines := strings.Split(stripStrings(now[rel]), "\n")
		for _, l := range added[rel] {
			if l.n-1 >= len(lines) {
				continue
			}
			t := strings.TrimSpace(lines[l.n-1])
			if strings.HasPrefix(t, "import ") || strings.HasPrefix(t, "package ") || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
				continue
			}
			if i := strings.Index(t, "//"); i >= 0 {
				t = t[:i]
			}
			if m := fqnRe.FindString(t); m != "" {
				out = append(out, fmt.Sprintf("%s:%d writes %s in full; import it and use the simple name", rel, l.n, m))
			}
		}
	}
	return out
}

var enumRe = regexp.MustCompile(`(?s)\benum\s+(?:class\s+)?(\w+)[^{]*\{([^}]*)\}`)
var enumConst = regexp.MustCompile(`^\s*([A-Z][A-Z0-9_]*)\b`)

func enumConstants(body string) []string {
	if i := strings.Index(body, ";"); i >= 0 {
		body = body[:i]
	}
	var out []string
	for _, p := range strings.Split(body, ",") {
		if m := enumConst.FindStringSubmatch(p); m != nil {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// copiedEnums finds enums the run added whose constants are the same as an
// enum somewhere else in the repo.
func copiedEnums(added map[string][]addedLine, srcs map[string]string) []string {
	type enum struct{ file, name, key string }
	var all []enum
	for f, s := range srcs {
		for _, m := range enumRe.FindAllStringSubmatch(stripStrings(s), -1) {
			c := enumConstants(m[2])
			if len(c) >= 2 {
				all = append(all, enum{f, m[1], strings.Join(c, ",")})
			}
		}
	}
	var out []string
	for _, rel := range sortedKeys(added) {
		addedText := map[string]bool{}
		for _, l := range added[rel] {
			addedText[strings.TrimSpace(l.text)] = true
		}
		for _, e := range all {
			if e.file != rel {
				continue
			}
			isNew := false
			for t := range addedText {
				if regexp.MustCompile(`\benum\s+(?:class\s+)?` + e.name + `\b`).MatchString(t) {
					isNew = true
					break
				}
			}
			if !isNew {
				continue
			}
			for _, o := range all {
				if o.key == e.key && (o.file != e.file || o.name != e.name) {
					out = append(out, fmt.Sprintf("enum %s in %s has the same constants as %s in %s; use %s instead of a copy", e.name, rel, o.name, o.file, o.name))
					break
				}
			}
		}
	}
	return out
}

var (
	privMethod = regexp.MustCompile(`(?m)^\s*private\s+(?:static\s+)?(?:final\s+)?(?:<[^>]+>\s+)?[\w<>\[\],.? ]+?\s+(\w+)\s*\(([^)]*)\)\s*(?:throws[\w\s,.]+)?\{`)
	paramName  = regexp.MustCompile(`(\w+)\s*$`)
	recordHead = regexp.MustCompile(`\brecord\s+$`)
	localDecl  = regexp.MustCompile(`^\s*(?:final\s+)?(?:var|[A-Z][\w<>\[\],.? ]*?|int|long|double|boolean|char|byte|short|float)\s+(\w+)\s*=[^=]`)
)

// bodyAt returns the text from the brace at open to its matching brace.
func bodyAt(src string, open int) string {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[open : i+1]
			}
		}
	}
	return src[open:]
}

func lineOf(src string, off int) int { return strings.Count(src[:off], "\n") + 1 }

// splitTop splits a parameter list at the commas outside <>, () and [].
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '<', '(', '[':
			depth++
		case '>', ')', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// unusedNames finds, in Java files the run changed, parameters of private
// methods that the body never uses, and locals that are assigned and never
// read, on lines the run added.
func unusedNames(added map[string][]addedLine, now map[string]string) []string {
	var out []string
	for _, rel := range sortedKeys(added) {
		if filepath.Ext(rel) != ".java" {
			continue
		}
		src := stripStrings(now[rel])
		isAdded := map[int]bool{}
		for _, l := range added[rel] {
			isAdded[l.n] = true
		}
		for _, m := range privMethod.FindAllStringSubmatchIndex(src, -1) {
			body := bodyAt(src, m[1]-1)
			name := src[m[2]:m[3]]
			ln := lineOf(src, m[2])
			if !isAdded[ln] || recordHead.MatchString(src[m[0]:m[2]]) {
				continue // record components are read through accessors
			}
			for _, p := range splitTop(src[m[4]:m[5]]) {
				pm := paramName.FindStringSubmatch(strings.TrimSpace(p))
				if pm == nil {
					continue
				}
				if !regexp.MustCompile(`\b` + pm[1] + `\b`).MatchString(body) {
					out = append(out, fmt.Sprintf("%s:%d: parameter %s of %s is never used", rel, ln, pm[1], name))
				}
			}
		}
		lines := strings.Split(src, "\n")
		for i, l := range lines {
			if !isAdded[i+1] {
				continue
			}
			m := localDecl.FindStringSubmatch(l)
			if m == nil || strings.Contains(l, "private ") || strings.Contains(l, "public ") || strings.Contains(l, "protected ") {
				continue
			}
			if ind := len(l) - len(strings.TrimLeft(l, " \t")); ind < 8 && !strings.HasPrefix(l, "\t\t") {
				continue // a field, not a local
			}
			after := l[strings.Index(l, "=")+1:]
			rest := strings.Join(lines[i+1:min(len(lines), i+200)], "\n")
			if end := blockEnd(rest, strings.Count(after, "{")-strings.Count(after, "}")); end >= 0 {
				rest = rest[:end]
			}
			if !regexp.MustCompile(`\b`+m[1]+`\b`).MatchString(rest) && !regexp.MustCompile(`\b`+m[1]+`\b`).MatchString(after) {
				out = append(out, fmt.Sprintf("%s:%d: local %s is assigned and never used", rel, i+1, m[1]))
			}
		}
	}
	return out
}

// blockEnd is the offset where the block that holds the start of s closes;
// open is the number of braces already open at the start (a lambda on the
// declaration line).
func blockEnd(s string, open int) int {
	depth := max(open, 0)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

var commentRe = regexp.MustCompile(`^\s*(//|#)\s?(.*)$`)

// addedComments lists the line comments the run added in source files.
func addedComments(added map[string][]addedLine) []string {
	var out []string
	for _, rel := range sortedKeys(added) {
		ext := filepath.Ext(rel)
		if !srcExt[ext] {
			continue
		}
		for _, l := range added[rel] {
			m := commentRe.FindStringSubmatch(l.text)
			if m == nil || m[1] == "#" && ext != ".py" {
				continue
			}
			out = append(out, fmt.Sprintf("%s:%d %s", rel, l.n, strings.TrimSpace(l.text)))
		}
	}
	return out
}

func suppressions(added map[string][]addedLine) []string {
	var out []string
	for _, rel := range sortedKeys(added) {
		for _, l := range added[rel] {
			if strings.Contains(l.text, "@SuppressWarnings") || strings.Contains(l.text, "@Suppress(") {
				out = append(out, fmt.Sprintf("%s:%d %s", rel, l.n, strings.TrimSpace(l.text)))
			}
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// guardMinComments is how many added comments start the comment review.
const guardMinComments = 3

// guard runs the checks and returns the message for the fix turn ("" when
// nothing is found) and one note per finding for the stats.
func guard(prompt, cwd string, notes []string, fs []fileFact) (string, []string) {
	added := addedLines(fs)
	now := map[string]string{}
	for _, f := range fs {
		now[f.rel] = f.now
	}
	root := cwd
	if r := projectRoot(cwd); r != "" {
		root = r
	}
	srcs := repoSources(root, 3000)

	var parts, out []string
	add := func(head string, items []string, tail string) {
		if len(items) == 0 {
			return
		}
		for _, it := range items {
			out = append(out, "qh guard: "+it)
		}
		if len(items) > 20 {
			items = append(items[:20], fmt.Sprintf("(and %d more)", len(items)-20))
		}
		parts = append(parts, head+"\n"+strings.Join(items, "\n")+"\n"+tail)
	}

	add("These added lines use text that the notes or the task say never to use:", neverHits(neverRules(append(notes, prompt)...), added),
		"Use what the notes and the task give instead. If you are sure a fact in the notes is wrong, do not work around it: put the evidence (the exact compiler error or javap output) under ## Questions.")
	add("The task gives these declarations, and no source file in the repo has them as written:", missingAPI(prompt, srcs),
		"Use the task's names, types and parameters exactly. Do not add your own copy of a type the task names. If one cannot be used as written, say why under ## Questions.")
	add("You changed files outside the task's Change list:", outsideScope(prompt, fs),
		"Undo each one (git checkout -- FILE for a tracked file, rm for a new one) unless the task cannot pass without it; keep one only with a line under \"Changed outside the brief:\" in the report saying why.")
	add("Clean up these lines:", append(append(append(fqnInCode(added, now), copiedEnums(added, srcs)...), unusedNames(added, now)...), suppressions(added)...),
		"Fix each one with edit: import the class, reuse the existing type, delete the unused parameter (and its arguments at the call sites), drop an unused local (keep the call on its own line when it has an effect, such as saving a row), delete a @SuppressWarnings that hides nothing.")
	if cs := addedComments(added); len(cs) >= guardMinComments {
		if len(cs) > 30 {
			cs = append(cs[:30], fmt.Sprintf("(and %d more)", len(cs)-30))
		}
		parts = append(parts, "You added these comments:\n"+strings.Join(cs, "\n")+
			"\nDelete every comment that says what the next line does, numbers steps, or is a section banner. Keep only those that say why (a reason, a rule, a trap) and fix any that are wrong. Do not add new comments.")
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "qh review of your changes:\n\n" + strings.Join(parts, "\n\n") + "\n\nFix these with edit, build again, then give the final report.", out
}
