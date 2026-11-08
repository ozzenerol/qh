package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const factsMaxFiles = 10

// testCount is one "[qh: tests: ...]" line qh wrote.
type testCount struct {
	passed, failed, skipped int
	classes                 []int // the per-class counts in the line
}

var (
	testSumRe   = regexp.MustCompile(`^(\d+) passed, (\d+) failed, (\d+) skipped`)
	testClassRe = regexp.MustCompile(`(\w+) (\d+)(?:,|\))`)
)

func (t *Tools) noteTests(sum string) {
	if m := testSumRe.FindStringSubmatch(sum); m != nil {
		p, _ := strconv.Atoi(m[1])
		f, _ := strconv.Atoi(m[2])
		s, _ := strconv.Atoi(m[3])
		c := testCount{passed: p, failed: f, skipped: s}
		if i := strings.Index(sum, "("); i >= 0 {
			for _, x := range testClassRe.FindAllStringSubmatch(sum[i:], -1) {
				n, _ := strconv.Atoi(x[2])
				c.classes = append(c.classes, n)
			}
		}
		t.Tests = append(t.Tests, c)
	}
}

// fileFact is one file the run changed, compared with its content before
// the run.
type fileFact struct {
	rel, kind  string // kind: created, modified, deleted
	add, del   int    // -1 when the file was too big to keep
	formatOnly bool   // only whitespace changed
	before     string
	now        string
}

// changes lists the files of the checkpoint that differ now from their
// content before the run.
func (cp *Checkpoint) changes() []fileFact {
	if cp == nil {
		return nil
	}
	var out []fileFact
	for _, f := range cp.Files {
		rel, err := filepath.Rel(cp.Cwd, f.Path)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = f.Path
		}
		nowB, err := os.ReadFile(f.Path)
		exists := err == nil
		ff := fileFact{rel: rel, now: string(nowB)}
		switch {
		case f.Skipped:
			if !exists {
				ff.kind = "deleted"
			} else {
				ff.kind = "modified"
			}
			ff.add, ff.del = -1, -1
			out = append(out, ff)
			continue
		case !f.Existed && !exists:
			continue
		case !f.Existed:
			ff.kind = "created"
		case !exists:
			ff.kind = "deleted"
		default:
			ff.kind = "modified"
		}
		if f.Existed {
			b, err := os.ReadFile(filepath.Join(cp.dir, f.Blob))
			if err != nil {
				continue
			}
			ff.before = string(b)
		}
		if ff.kind == "modified" && ff.before == ff.now {
			continue
		}
		for _, op := range lineOps(splitLines(ff.before), splitLines(ff.now)) {
			switch op.kind {
			case '+':
				ff.add++
			case '-':
				ff.del++
			}
		}
		ff.formatOnly = ff.kind == "modified" && noSpace(ff.before) == noSpace(ff.now)
		out = append(out, ff)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

func noSpace(s string) string {
	return strings.Join(strings.Fields(s), "")
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type lineOp struct {
	kind byte // ' ', '-', '+'
	text string
	a, b int // line numbers (1-based) in the old and the new file
}

// lineOps is a line diff: common head and tail cut, then an LCS on the rest.
// A middle too big for the table becomes one replace block.
func lineOps(a, b []string) []lineOp {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	var ops []lineOp
	for i := 0; i < p; i++ {
		ops = append(ops, lineOp{' ', a[i], i + 1, i + 1})
	}
	am, bm := a[p:len(a)-s], b[p:len(b)-s]
	n, m := len(am), len(bm)
	if n*m > 4_000_000 {
		for i, l := range am {
			ops = append(ops, lineOp{'-', l, p + i + 1, 0})
		}
		for j, l := range bm {
			ops = append(ops, lineOp{'+', l, 0, p + j + 1})
		}
	} else {
		lcs := make([][]int32, n+1)
		for i := range lcs {
			lcs[i] = make([]int32, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if am[i] == bm[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else {
					lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && am[i] == bm[j]:
				ops = append(ops, lineOp{' ', am[i], p + i + 1, p + j + 1})
				i, j = i+1, j+1
			case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
				ops = append(ops, lineOp{'-', am[i], p + i + 1, 0})
				i++
			default:
				ops = append(ops, lineOp{'+', bm[j], 0, p + j + 1})
				j++
			}
		}
	}
	for k := 0; k < s; k++ {
		ia, ib := len(a)-s+k, len(b)-s+k
		ops = append(ops, lineOp{' ', a[ia], ia + 1, ib + 1})
	}
	return ops
}

// unified prints a unified diff with 3 lines of context.
func unified(ff fileFact) string {
	ops := lineOps(splitLines(ff.before), splitLines(ff.now))
	from, to := "a/"+ff.rel, "b/"+ff.rel
	if ff.kind == "created" {
		from = "/dev/null"
	}
	if ff.kind == "deleted" {
		to = "/dev/null"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", from, to)
	const ctx = 3
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(0, i-ctx)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			k := end
			for k < len(ops) && ops[k].kind == ' ' {
				k++
			}
			if k < len(ops) && k-end <= 2*ctx {
				end = k
				continue
			}
			end = min(len(ops), end+ctx)
			break
		}
		h := ops[start:end]
		la, lb, na, nb := 0, 0, 0, 0
		for _, o := range h {
			if o.kind != '+' {
				if la == 0 {
					la = o.a
				}
				na++
			}
			if o.kind != '-' {
				if lb == 0 {
					lb = o.b
				}
				nb++
			}
		}
		if na == 0 {
			la = startLine(ops, start, true)
		}
		if nb == 0 {
			lb = startLine(ops, start, false)
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", la, na, lb, nb)
		for _, o := range h {
			b.WriteString(string(o.kind) + o.text + "\n")
		}
		i = end
	}
	return b.String()
}

// startLine is the line before an empty side of a hunk, as diff prints it.
func startLine(ops []lineOp, at int, old bool) int {
	for k := at - 1; k >= 0; k-- {
		if old && ops[k].a > 0 {
			return ops[k].a
		}
		if !old && ops[k].b > 0 {
			return ops[k].b
		}
	}
	return 0
}

// facts builds the block that ends the report: what the run changed, the
// last test counts qh saw and the background commands it stopped, then one
// note per claim of the report that does not match them.
func facts(report string, files []fileFact, tests []testCount, stopped []string, rw bool) []string {
	var out []string
	if rw {
		if len(files) > 0 {
			out = append(out, fmt.Sprintf("qh facts: the run changed %d files:", len(files)))
			for i, f := range files {
				if i == factsMaxFiles {
					out = append(out, fmt.Sprintf("  (%d more; qh diff shows all)", len(files)-i))
					break
				}
				l := "  " + f.kind + " " + f.rel
				switch {
				case f.add < 0:
					l += " (over 2 MB, not counted)"
				default:
					l += fmt.Sprintf(" (+%d -%d)", f.add, f.del)
				}
				if f.formatOnly {
					l += " whitespace only"
				}
				out = append(out, l)
			}
		}
	}
	if len(tests) > 0 {
		t := tests[len(tests)-1]
		out = append(out, fmt.Sprintf("qh facts: last test run qh saw: %d passed, %d failed, %d skipped", t.passed, t.failed, t.skipped))
	}
	if len(stopped) > 0 {
		out = append(out, "qh facts: stopped the background commands still running at the end: "+strings.Join(stopped, ", "))
	}
	if rw {
		out = append(out, fileClaims(report, files)...)
	}
	out = append(out, testClaims(report, tests)...)
	return out
}

var (
	claimLabel = regexp.MustCompile(`(?i)\b(files?\s+(changed|created|modified|edited|written|added|deleted|updated)|(changed|modified|created|new|edited)\s+files?)\b`)
	otherLabel = regexp.MustCompile(`(?i)^\s*(#+\s|\*\*[^*]+:?\*\*:?\s*$|\*\*[^*]+:\*\*|[A-Z][\w ]{2,30}:\s*$)`)
	fileTok    = regexp.MustCompile("[A-Za-z0-9_./-]*[A-Za-z0-9_-]\\.(kt|kts|java|go|py|ts|tsx|js|jsx|rs|c|h|cc|cpp|hpp|cs|rb|php|swift|scala|groovy|sql|xml|yml|yaml|json|toml|properties|gradle|md|sh|html|css|proto)\\b")
)

// claimedFiles returns the base names in the report's "files changed" part:
// the label line and the lines after it, up to a blank line before
// non-list text, or the next label.
func claimedFiles(report string) []string {
	var names []string
	seen := map[string]bool{}
	add := func(l string) {
		for _, m := range fileTok.FindAllString(l, -1) {
			if b := filepath.Base(m); !seen[b] {
				seen[b] = true
				names = append(names, b)
			}
		}
	}
	lines := strings.Split(report, "\n")
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		loc := claimLabel.FindStringIndex(l)
		if loc == nil || strings.Contains(strings.ToLower(l[:loc[0]]), "read") {
			continue
		}
		add(l[loc[1]:])
		for j := i + 1; j < len(lines); j++ {
			n := lines[j]
			t := strings.TrimSpace(n)
			if t == "" {
				if j+1 < len(lines) && listItem(lines[j+1]) {
					continue
				}
				break
			}
			if !listItem(n) && otherLabel.MatchString(n) {
				break
			}
			if claimLabel.MatchString(n) {
				break
			}
			add(n)
			i = j
		}
	}
	return names
}

func listItem(l string) bool {
	t := strings.TrimSpace(l)
	return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || len(t) > 2 && t[0] >= '0' && t[0] <= '9' && (strings.HasPrefix(t[1:], ". ") || strings.HasPrefix(t[1:], ") ") || len(t) > 3 && t[1] >= '0' && t[1] <= '9' && t[2] == '.')
}

func fileClaims(report string, files []fileFact) []string {
	changed := map[string]bool{}
	for _, f := range files {
		changed[filepath.Base(f.rel)] = true
	}
	var out []string
	for _, c := range claimedFiles(report) {
		if !changed[c] {
			out = append(out, "qh note: the report lists "+c+" as changed, but this run did not change it")
		}
	}
	for _, f := range files {
		if f.formatOnly || isTestPath(f.rel) || f.kind == "deleted" && f.del == 0 {
			continue
		}
		if !strings.Contains(report, filepath.Base(f.rel)) {
			out = append(out, "qh note: the run "+f.kind+" "+f.rel+", and the report does not mention it")
		}
	}
	return out
}

var (
	reportTests = regexp.MustCompile(`(?i)\b(\d+)\s+(?:(?:unit\s+|integration\s+)?tests?\s+)?(?:passed|pass|passing)\b`)
	saysPass    = regexp.MustCompile(`(?i)\b(?:all\s+(?:\d+\s+)?)?tests?\s+(?:now\s+)?pass(?:es|ed)?\b|\b0\s+failed\b|\bno\s+(?:test\s+)?failures\b`)
)

func testClaims(report string, tests []testCount) []string {
	if len(tests) == 0 {
		return nil
	}
	ok := map[int]bool{}
	for _, t := range tests {
		ok[t.passed] = true
		ok[t.passed+t.failed+t.skipped] = true
		for _, c := range t.classes {
			ok[c] = true
		}
	}
	var out []string
	seen := map[int]bool{}
	for _, m := range reportTests.FindAllStringSubmatch(report, -1) {
		n, _ := strconv.Atoi(m[1])
		if n == 0 || ok[n] || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, fmt.Sprintf("qh note: the report says %q, but no test run qh saw had %d tests", strings.TrimSpace(m[0]), n))
	}
	last := tests[len(tests)-1]
	if last.failed > 0 {
		if m := saysPass.FindString(report); m != "" {
			out = append(out, fmt.Sprintf("qh note: the report says %q, but the last test run qh saw had %d failed", m, last.failed))
		}
	}
	return out
}

// lastRunFile names the file in the undo folder that holds the id of the
// newest run, whether it changed files or not.
const lastRunFile = "last"

func diffCmd(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	name := fs.String("s", "main", "session")
	fs.Parse(args)
	if !validName.MatchString(*name) {
		return fmt.Errorf("bad session name %q", *name)
	}
	out, err := lastDiff(undoRoot(*name))
	fmt.Print(out)
	return err
}

// lastDiff is the unified diff of the newest run of the session against the
// files as they are now.
func lastDiff(root string) (string, error) {
	runs := listRuns(root)
	last, _ := os.ReadFile(filepath.Join(root, lastRunFile))
	id := strings.TrimSpace(string(last))
	if len(runs) == 0 || id != "" && runs[len(runs)-1].ID != id {
		if id != "" {
			return fmt.Sprintf("the last run (%s) changed no files\n", id), nil
		}
		return "", errors.New("no run with changes on this session")
	}
	cp := runs[len(runs)-1]
	var b strings.Builder
	fmt.Fprintf(&b, "run %s: %s\n", cp.ID, cp.Prompt)
	fs := cp.changes()
	if len(fs) == 0 {
		b.WriteString("the files are back to their state before the run\n")
	}
	for _, f := range fs {
		if f.add < 0 {
			fmt.Fprintf(&b, "%s %s: over 2 MB, no diff\n", f.kind, f.rel)
			continue
		}
		b.WriteString(unified(f))
	}
	return b.String(), nil
}
