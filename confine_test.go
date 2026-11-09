package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfine(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	deps := filepath.Join(base, "deps")
	other := filepath.Join(base, "other")
	for _, d := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "sub"), deps, other} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644)
	os.WriteFile(filepath.Join(deps, "lib.txt"), []byte("lib\n"), 0o644)
	os.WriteFile(filepath.Join(other, "secret.txt"), []byte("secret\n"), 0o644)
	os.Symlink(other, filepath.Join(repo, "link"))

	tl := &Tools{Cwd: filepath.Join(repo, "sub"), Confine: true, ReadRoots: []string{deps}}
	run := func(name, args string) (string, bool) { return tl.Run(context.Background(), name, args) }

	for _, c := range []struct {
		name, args string
		ok         bool
	}{
		{"read", `{"path": "../a.txt"}`, true},
		{"read", `{"path": "` + filepath.Join(deps, "lib.txt") + `"}`, true},
		{"read", `{"path": "` + filepath.Join(other, "secret.txt") + `"}`, false},
		{"read", `{"path": "../../other/secret.txt"}`, false},
		{"read", `{"path": "../link/secret.txt"}`, false},
		{"ls", `{"path": "` + other + `"}`, false},
		{"grep", `{"pattern": "secret", "path": "` + other + `"}`, false},
		{"write", `{"path": "` + filepath.Join(deps, "x.txt") + `", "content": "x"}`, false},
		{"write", `{"path": "../b.txt", "content": "b\n"}`, true},
	} {
		out, ok := run(c.name, c.args)
		blocked := strings.Contains(out, "is outside the project")
		if blocked == c.ok {
			t.Errorf("%s %s: blocked=%v, want ok=%v: %s", c.name, c.args, blocked, c.ok, out)
		}
		_ = ok
	}

	tl.Confine = false
	if out, _ := run("read", `{"path": "`+filepath.Join(other, "secret.txt")+`"}`); !strings.Contains(out, "secret") {
		t.Errorf("unconfined read failed: %s", out)
	}
}
