package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	grepMax = 100 // matching lines that grep returns
	globMax = 200 // paths that glob returns
)

const grepHint = "[qh: the pattern is a regular expression and ( [ { * . have meanings there. For a literal search set literal to true, or escape them.]"

// skipDirs are never searched: dependencies and build output. Hidden
// folders (.git, .cargo, .m2, .venv) are skipped too, unless the search
// starts inside one.
var skipDirs = map[string]bool{"node_modules": true, "build": true, "target": true, "vendor": true, "__pycache__": true, "venv": true}

// globMatch matches a slash path against a pattern where ** stands for any
// number of directories. A pattern without a slash matches the base name.
func globMatch(pat, rel string) bool {
	if !strings.Contains(pat, "/") {
		ok, _ := filepath.Match(pat, filepath.Base(rel))
		return ok
	}
	return matchSegs(strings.Split(pat, "/"), strings.Split(rel, "/"))
}

func matchSegs(p, s []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			for i := 0; i <= len(s); i++ {
				if matchSegs(p[1:], s[i:]) {
					return true
				}
			}
			return false
		}
		if len(s) == 0 {
			return false
		}
		if ok, _ := filepath.Match(p[0], s[0]); !ok {
			return false
		}
		p, s = p[1:], s[1:]
	}
	return len(s) == 0
}

// walk calls fn for each regular file under root (a file or a directory),
// with its path relative to the working directory.
func (t *Tools) walk(ctx context.Context, root string, fn func(path, rel string)) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, e := filepath.Rel(t.Cwd, p)
		if e != nil || strings.HasPrefix(rel, "..") {
			rel = p
		}
		fn(p, filepath.ToSlash(rel))
		return nil
	})
}

// glob lists the files whose path matches pat, like src/**/*Test.kt.
func (t *Tools) glob(ctx context.Context, root, pat string) (string, bool) {
	pat = strings.TrimPrefix(filepath.ToSlash(pat), "./")
	var out []string
	n := 0
	err := t.walk(ctx, root, func(_, rel string) {
		if globMatch(pat, rel) {
			if n++; len(out) < globMax {
				out = append(out, rel)
			}
		}
	})
	if err != nil {
		return "error: " + err.Error(), false
	}
	if n == 0 {
		return "no matches", true
	}
	res := strings.Join(out, "\n")
	if n > len(out) {
		res += fmt.Sprintf("\n[%d more]", n-len(out))
	}
	return res, true
}

// grep searches the files under root for a regular expression (Go syntax)
// or, with literal, for the exact text. glob filters the files.
func (t *Tools) grep(ctx context.Context, pat, root, glob string, literal bool) (string, bool) {
	match := func(l []byte) bool { return bytes.Contains(l, []byte(pat)) }
	if !literal {
		re, err := regexp.Compile(pat)
		if err != nil {
			return "error: " + err.Error() + "\n" + grepHint, false
		}
		match = re.Match
	}
	glob = strings.TrimPrefix(filepath.ToSlash(glob), "./")
	var b strings.Builder
	n := 0
	var hits []grepHit
	err := t.walk(ctx, root, func(p, rel string) {
		if glob != "" && !globMatch(glob, rel) {
			return
		}
		if fi, err := os.Stat(p); err != nil || fi.Size() > 4<<20 {
			return
		}
		data, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			return // binary
		}
		lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
		for i, l := range lines {
			l = bytes.TrimSuffix(l, []byte("\r"))
			if !match(l) {
				continue
			}
			if t.GrepCtx && n < grepCtxMax {
				hits = append(hits, grepHit{rel, lines, i})
			}
			if n++; n <= grepMax {
				txt := string(l)
				if len(txt) > 300 {
					txt = txt[:300] + "..."
				}
				fmt.Fprintf(&b, "%s:%d:%s\n", rel, i+1, txt)
			}
		}
	})
	if err != nil {
		return "error: " + err.Error(), false
	}
	if n == 0 {
		return "no matches", true
	}
	if t.GrepCtx && n <= grepCtxMax {
		return grepWithContext(hits), true
	}
	if n > grepMax {
		fmt.Fprintf(&b, "[%d more]\n", n-grepMax)
	}
	return b.String(), true
}
