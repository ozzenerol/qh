package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var readLine = regexp.MustCompile(`^(\d+)\t(.*)$`)

// TestReplayFailedCalls replays the failed edit and read calls of stored
// sessions (QH_REPLAY=dir of session .json files) on files rebuilt from the
// session: full reads, then the successful writes and edits in order. Bash
// failures are sorted with failKind. It prints counts; it fails on nothing.
func TestReplayFailedCalls(t *testing.T) {
	src := os.Getenv("QH_REPLAY")
	if src == "" {
		t.Skip("set QH_REPLAY to a folder of session files")
	}
	files, _ := filepath.Glob(filepath.Join(src, "*.json"))
	sort.Strings(files)
	count := map[string]int{}
	ctx := context.Background()
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var s Session
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		root := t.TempDir()
		local := func(p string) string {
			if !filepath.IsAbs(p) {
				p = filepath.Join(s.Cwd, p)
			}
			return filepath.Join(root, filepath.Clean(p))
		}
		known := map[string]bool{}
		calls := map[string]ToolCall{}
		for _, m := range s.Msgs {
			for _, tc := range m.ToolCalls {
				calls[tc.ID] = tc
			}
			if m.Role != "tool" {
				continue
			}
			tc, ok := calls[m.ToolCallID]
			if !ok {
				continue
			}
			var a map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &a)
			p, _ := a["path"].(string)
			if p == "" {
				p, _ = a["file_path"].(string)
			}
			out := m.Content
			if strings.HasPrefix(out, "[you made") {
				_, out, _ = strings.Cut(out, "\n")
			}
			failed := strings.HasPrefix(out, "error:")
			name := tc.Function.Name
			switch {
			case name == "bash":
				if strings.HasPrefix(out, "error:") || regexp.MustCompile(`\[exit [1-9]\d*\]\s*$`).MatchString(out) && !strings.HasPrefix(out, "no matches [exit 1]") {
					count["bash "+failKind("bash", out)]++
				}
			case name == "read" && !failed && p != "":
				if c, ok := fromRead(out); ok {
					lp := local(p)
					os.MkdirAll(filepath.Dir(lp), 0o755)
					os.WriteFile(lp, []byte(c), 0o644)
					known[lp] = true
				}
			case name == "write" && !failed && p != "":
				c, _ := a["content"].(string)
				lp := local(p)
				os.MkdirAll(filepath.Dir(lp), 0o755)
				os.WriteFile(lp, []byte(c), 0o644)
				known[lp] = true
			case name == "edit" && p != "":
				lp := local(p)
				if !known[lp] {
					if failed {
						count["edit failed, file unknown"]++
					}
					continue
				}
				before, _ := os.ReadFile(lp)
				res, ok := (&Tools{Cwd: filepath.Join(root, s.Cwd)}).Run(ctx, "edit", rebase(tc.Function.Arguments, root, s.Cwd))
				switch {
				case failed && ok:
					count["edit failed, now ok"]++
					fmt.Printf("now ok: %s\n", firstLine(res))
					os.WriteFile(lp, before, 0o644) // the session went on from the failure
				case failed:
					count["edit failed, still fails"]++
				case !ok:
					known[lp] = false // the rebuild drifted
				}
			case name == "read" && failed && strings.Contains(out, "is a directory"):
				count["read of a folder, now listed"]++
			case failed:
				count[name+" failed, not replayed"]++
			}
		}
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%5d  %s\n", count[k], k)
	}
}

// fromRead rebuilds a whole file from a read result, or says it cannot.
func fromRead(out string) (string, bool) {
	if strings.HasPrefix(out, "note:") {
		_, out, _ = strings.Cut(out, "\n")
	}
	var b strings.Builder
	n := 0
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		m := readLine.FindStringSubmatch(l)
		if m == nil || strings.HasSuffix(l, "...[line cut]") {
			return "", false
		}
		if n++; fmt.Sprint(n) != m[1] {
			return "", false
		}
		b.WriteString(m[2] + "\n")
	}
	return b.String(), n > 0
}

// rebase points the path of the call into the replay folder.
func rebase(args, root, cwd string) string {
	var a map[string]any
	json.Unmarshal([]byte(args), &a)
	for _, k := range []string{"path", "file_path"} {
		if p, ok := a[k].(string); ok {
			if !filepath.IsAbs(p) {
				p = filepath.Join(cwd, p)
			}
			a[k] = filepath.Join(root, p)
		}
	}
	b, _ := json.Marshal(a)
	return string(b)
}

// TestReplayReports runs the claim check on the final report of the last
// run of each stored session (QH_REPLAY), with the facts taken from that
// run's successful writes and edits and its test count lines. It prints the
// notes; it fails on nothing.
func TestReplayReports(t *testing.T) {
	src := os.Getenv("QH_REPLAY")
	if src == "" {
		t.Skip("set QH_REPLAY to a folder of session files")
	}
	files, _ := filepath.Glob(filepath.Join(src, "*.json"))
	sort.Strings(files)
	sumRe := regexp.MustCompile(`\[qh: tests: ([^\]]+)\]`)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var s Session
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		start := 0
		for i, m := range s.Msgs {
			if m.Role == "user" && m.From != "qh" && m.From != "compact" {
				start = i
			}
		}
		run := s.Msgs[start:]
		report := ""
		for i := len(run) - 1; i >= 0; i-- {
			if run[i].Role == "assistant" && len(run[i].ToolCalls) == 0 && len(run[i].Content) > 100 {
				report = run[i].Content
				break
			}
		}
		if report == "" {
			continue
		}
		calls := map[string]ToolCall{}
		changed := map[string]bool{}
		tl := &Tools{}
		for _, m := range run {
			for _, tc := range m.ToolCalls {
				calls[tc.ID] = tc
			}
			if m.Role != "tool" {
				continue
			}
			tc := calls[m.ToolCallID]
			if (tc.Function.Name == "write" || tc.Function.Name == "edit") && !strings.HasPrefix(m.Content, "error:") {
				var a struct{ Path string }
				json.Unmarshal([]byte(tc.Function.Arguments), &a)
				if a.Path != "" {
					changed[a.Path] = true
				}
			}
			for _, x := range sumRe.FindAllStringSubmatch(m.Content, -1) {
				tl.noteTests(x[1])
			}
		}
		var ff []fileFact
		for p := range changed {
			rel := strings.TrimPrefix(p, s.Cwd+"/")
			ff = append(ff, fileFact{rel: rel, kind: "modified", add: 1})
		}
		notes := facts(report, ff, tl.Tests, nil, true)
		fmt.Printf("== %s: %d changed, claimed %v\n", filepath.Base(f), len(changed), claimedFiles(report))
		for _, n := range notes {
			if strings.HasPrefix(n, "qh note") {
				fmt.Println("   " + n)
			}
		}
	}
}
