package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	undoKeep    = 5       // runs kept per session
	undoMaxFile = 2 << 20 // larger files are not kept
)

// Checkpoint keeps the content of each file a run changes, as it was before
// the run's first change to it. Nothing is stored until a file is touched.
type Checkpoint struct {
	root string // sessions/NAME.undo
	dir  string // root/ID, made at the first touched file
	idx  map[string]int
	git  string // git root of Cwd, "" when Cwd is not in a repo

	bgSnaps []*bashSnap // taken when a background command started; applied at the end

	ID       string    `json:"id"`
	Prompt   string    `json:"prompt"`
	Cwd      string    `json:"cwd"`
	Started  time.Time `json:"started"`
	Finished bool      `json:"finished"`
	Files    []cpFile  `json:"files"`
}

type cpFile struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	Skipped bool   `json:"skipped,omitempty"` // over undoMaxFile; not restored
	Blob    string `json:"blob,omitempty"`
	After   string `json:"after"` // sha256 of the content when the run ended, "" when the file was gone
}

// undoRoot is where the checkpoints of a session live.
func undoRoot(session string) string {
	d, err := qhDir()
	if err != nil || session == "" {
		return ""
	}
	return filepath.Join(d, "sessions", session+".undo")
}

func newCheckpoint(root, cwd, prompt string) *Checkpoint {
	if root == "" {
		return nil
	}
	cp := &Checkpoint{root: root, idx: map[string]int{}, Cwd: cwd, Started: time.Now(),
		ID: time.Now().Format("20060102-150405.000"), Prompt: oneLine(prompt, 120)}
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		cp.git = strings.TrimSpace(string(out))
	}
	return cp
}

// keep stores fp as it is now, unless the run touched it already.
func (cp *Checkpoint) keep(fp string) {
	if cp == nil {
		return
	}
	if _, ok := cp.idx[fp]; ok {
		return
	}
	b, err := os.ReadFile(fp)
	cp.keepContent(fp, b, err == nil)
}

func (cp *Checkpoint) keepContent(fp string, b []byte, existed bool) {
	if _, ok := cp.idx[fp]; ok {
		return
	}
	if cp.dir == "" {
		cp.dir = filepath.Join(cp.root, cp.ID)
		if os.MkdirAll(cp.dir, 0o700) != nil {
			return
		}
	}
	f := cpFile{Path: fp, Existed: existed}
	if existed {
		if len(b) > undoMaxFile {
			f.Skipped = true
		} else {
			f.Blob = fmt.Sprint(len(cp.Files))
			if os.WriteFile(filepath.Join(cp.dir, f.Blob), b, 0o600) != nil {
				return
			}
		}
	}
	cp.idx[fp] = len(cp.Files)
	cp.Files = append(cp.Files, f)
	cp.save()
}

func (cp *Checkpoint) save() {
	b, _ := json.MarshalIndent(cp, "", " ")
	p := filepath.Join(cp.dir, "run.json")
	if os.WriteFile(p+".tmp", b, 0o600) == nil {
		os.Rename(p+".tmp", p)
	}
}

// finish notes the state of every touched file and drops old runs.
func (cp *Checkpoint) finish() {
	if cp == nil {
		return
	}
	for _, s := range cp.bgSnaps {
		cp.bashAfter(s) // the background commands have stopped by now
	}
	if cp.dir == "" {
		return
	}
	for i := range cp.Files {
		cp.Files[i].After = fileHash(cp.Files[i].Path)
	}
	cp.Finished = true
	cp.save()
	runs := listRuns(cp.root)
	for i := 0; i < len(runs)-undoKeep; i++ {
		os.RemoveAll(filepath.Join(cp.root, runs[i].ID))
	}
}

func fileHash(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// dirty lists the paths git status reports in the repo, absolute.
func (cp *Checkpoint) dirty() map[string]string {
	if cp == nil || cp.git == "" {
		return nil
	}
	out, err := exec.Command("git", "-C", cp.git, "status", "--porcelain", "-z", "--untracked-files=all").Output()
	if err != nil {
		return nil
	}
	m := map[string]string{}
	parts := strings.Split(string(out), "\x00")
	for i := 0; i < len(parts); i++ {
		e := parts[i]
		if len(e) < 4 {
			continue
		}
		m[filepath.Join(cp.git, e[3:])] = e[:2]
		if e[0] == 'R' || e[0] == 'C' {
			i++ // the source path follows
		}
	}
	return m
}

// bashBefore notes what a bash call may change: the dirty files git knows
// of, with the content of those this run has not kept yet.
type bashSnap struct {
	dirty   map[string]string
	content map[string][]byte
}

func (cp *Checkpoint) bashBefore() *bashSnap {
	d := cp.dirty()
	if d == nil {
		return nil
	}
	s := &bashSnap{dirty: d, content: map[string][]byte{}}
	for p := range d {
		if _, ok := cp.idx[p]; ok {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Size() <= undoMaxFile {
			if b, err := os.ReadFile(p); err == nil {
				s.content[p] = b
			}
		}
	}
	return s
}

// bashAfter keeps the files the bash call changed: a file that was clean
// before comes from HEAD (or did not exist when untracked), a dirty one from
// the copy taken before the call.
func (cp *Checkpoint) bashAfter(s *bashSnap) {
	if s == nil {
		return
	}
	after := cp.dirty()
	for p, st := range after {
		if _, ok := cp.idx[p]; ok {
			continue
		}
		if old, was := s.dirty[p]; was {
			b, ok := s.content[p]
			if !ok {
				continue
			}
			now, err := os.ReadFile(p)
			if err == nil && bytes.Equal(now, b) && old == st {
				continue
			}
			cp.keepContent(p, b, true)
			continue
		}
		if st == "??" || st[0] == 'A' {
			cp.keepContent(p, nil, false)
			continue
		}
		rel, _ := filepath.Rel(cp.git, p)
		if b, err := exec.Command("git", "-C", cp.git, "show", "HEAD:"+filepath.ToSlash(rel)).Output(); err == nil {
			cp.keepContent(p, b, true)
		}
	}
	// A dirty file the call deleted or reverted to HEAD.
	for p, b := range s.content {
		if _, ok := after[p]; !ok {
			if _, kept := cp.idx[p]; !kept {
				cp.keepContent(p, b, true)
			}
		}
	}
}

func listRuns(root string) []*Checkpoint {
	ents, _ := os.ReadDir(root)
	var runs []*Checkpoint
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "run.json"))
		if err != nil {
			continue
		}
		cp := &Checkpoint{}
		if json.Unmarshal(b, cp) == nil && cp.ID == e.Name() {
			cp.root, cp.dir = root, filepath.Join(root, e.Name())
			runs = append(runs, cp)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })
	return runs
}

// undo restores the files of the last run in root and deletes its
// checkpoint, so the next undo goes one run further back.
func undo(root string, force bool) (string, error) {
	runs := listRuns(root)
	if len(runs) == 0 {
		return "", errors.New("no run to undo")
	}
	cp := runs[len(runs)-1]
	if !cp.Finished && !force {
		return "", fmt.Errorf("run %s has not finished (it runs, or the daemon stopped in it); -force restores it anyway", cp.ID)
	}
	var changed []string
	for _, f := range cp.Files {
		if cp.Finished && fileHash(f.Path) != f.After {
			changed = append(changed, "  "+f.Path)
		}
	}
	if len(changed) > 0 && !force {
		return "", fmt.Errorf("these files changed after run %s ended; -force restores them anyway:\n%s", cp.ID, strings.Join(changed, "\n"))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "undid run %s: %s\n", cp.ID, cp.Prompt)
	var errs []string
	for _, f := range cp.Files {
		var err error
		switch {
		case f.Skipped:
			fmt.Fprintf(&b, "  kept      %s (over 2 MB, not saved)\n", f.Path)
			continue
		case !f.Existed:
			if err = os.Remove(f.Path); err == nil || errors.Is(err, os.ErrNotExist) {
				err = nil
				fmt.Fprintf(&b, "  deleted   %s\n", f.Path)
			}
		default:
			var c []byte
			if c, err = os.ReadFile(filepath.Join(cp.dir, f.Blob)); err == nil {
				os.MkdirAll(filepath.Dir(f.Path), 0o755)
				if err = os.WriteFile(f.Path, c, 0o644); err == nil {
					fmt.Fprintf(&b, "  restored  %s\n", f.Path)
				}
			}
		}
		if err != nil {
			errs = append(errs, f.Path+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return b.String(), errors.New(strings.Join(errs, "\n"))
	}
	os.RemoveAll(cp.dir)
	return b.String(), nil
}

func undoList(root string) string {
	runs := listRuns(root)
	if len(runs) == 0 {
		return "no runs to undo\n"
	}
	var b strings.Builder
	for i := len(runs) - 1; i >= 0; i-- {
		r := runs[i]
		state := ""
		if !r.Finished {
			state = " (not finished)"
		}
		fmt.Fprintf(&b, "%s  %d files%s  %s\n", r.ID, len(r.Files), state, r.Prompt)
	}
	return b.String()
}

func undoCmd(args []string) error {
	fs := flag.NewFlagSet("undo", flag.ExitOnError)
	name := fs.String("s", "main", "session")
	list := fs.Bool("list", false, "list the runs that can be undone, newest first")
	force := fs.Bool("force", false, "restore even when files changed after the run")
	fs.Parse(args)
	if !validName.MatchString(*name) {
		return fmt.Errorf("bad session name %q", *name)
	}
	root := undoRoot(*name)
	if *list {
		fmt.Print(undoList(root))
		return nil
	}
	out, err := undo(root, *force)
	fmt.Print(out)
	return err
}
