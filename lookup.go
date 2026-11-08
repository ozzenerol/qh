package main

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	lookupMax     = 10
	lookupTimeout = 180 * time.Second
)

func lookupSchema() map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name":        "lookup",
		"description": "Find a class or a top-level Kotlin function or property in the project's libraries. Returns the import lines.",
		"parameters": map[string]any{"type": "object", "required": []string{"name"}, "properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "a simple name like routes or RedisRateLimiter, or a dotted one"}}},
	}}
}

// The init script prints the resolved compile and test classpaths of every
// project, without the configuration cache.
const gradleInit = `allprojects {
    tasks.register("qhJars") {
        def cfgs = project.configurations.matching { it.canBeResolved && it.name in ["compileClasspath", "testCompileClasspath"] }
        doLast {
            cfgs.each { c ->
                c.incoming.artifactView { lenient = true }.files.each { println "QHJAR " + it }
            }
        }
    }
}
`

// lookupEntry is one importable name.
type lookupEntry struct {
	simple, fq, kind, jar string
}

type lookupIndex struct {
	entries []lookupEntry
	bySimp  map[string][]int
	note    string // how the jars were found
}

var (
	lookupMu    sync.Mutex
	lookupCache = map[string]*lookupIndex{} // index file -> loaded index, for the daemon's life
)

// buildRoot is the folder with settings.gradle(.kts), the top pom.xml or
// gradlew above cwd, or "".
func buildRoot(cwd string) (string, string) {
	kind, root := "", ""
	for d := cwd; ; d = filepath.Dir(d) {
		for _, f := range []string{"settings.gradle.kts", "settings.gradle", "gradlew"} {
			if exists(filepath.Join(d, f)) {
				kind, root = "gradle", d
			}
		}
		if kind == "" && exists(filepath.Join(d, "pom.xml")) {
			kind, root = "maven", d // keep going up: the top pom wins
		} else if kind == "maven" && exists(filepath.Join(d, "pom.xml")) {
			root = d
		}
		if exists(filepath.Join(d, ".git")) || d == filepath.Dir(d) {
			break
		}
	}
	return kind, root
}

var buildFileRe = regexp.MustCompile(`(^|/)(settings\.gradle(\.kts)?|build\.gradle(\.kts)?|gradle\.properties|libs\.versions\.toml|pom\.xml)$`)

// buildKey hashes the build files under root, so the index is rebuilt when
// the dependencies may have changed.
func buildKey(root string) string {
	var files []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "build", "target", ".gradle", "node_modules", ".idea", "out":
				return filepath.SkipDir
			}
			return nil
		}
		if buildFileRe.MatchString(p) {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		c, _ := os.ReadFile(f)
		b.WriteString(f + "\x00" + string(c) + "\x00")
	}
	return strconv.FormatUint(contentHash([]byte(b.String())), 36)
}

func runQuiet(ctx context.Context, dir string, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
	out, err := c.CombinedOutput()
	return string(out), err
}

// projectJars resolves the project's classpath with its own build tool,
// offline first. It falls back to the cache jars of the groups that the
// build files name.
func projectJars(ctx context.Context, kind, root, work string) ([]string, string) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var jars []string
	switch kind {
	case "gradle":
		init := filepath.Join(work, "qhjars.gradle")
		os.WriteFile(init, []byte(gradleInit), 0o644)
		tool := "gradle"
		if exists(filepath.Join(root, "gradlew")) {
			tool = "./gradlew"
		}
		for _, off := range []bool{true, false} {
			args := []string{"-q", "--no-configuration-cache", "--init-script", init, "qhJars"}
			if off {
				args = append([]string{"--offline"}, args...)
			}
			out, err := runQuiet(ctx, root, tool, args...)
			for _, l := range strings.Split(out, "\n") {
				if p, ok := strings.CutPrefix(strings.TrimSpace(l), "QHJAR "); ok && strings.HasSuffix(p, ".jar") {
					jars = append(jars, p)
				}
			}
			if err == nil && len(jars) > 0 {
				return dedupe(jars), "resolved by Gradle"
			}
		}
	case "maven":
		outFile := filepath.Join(work, "classpath.txt")
		tool := "mvn"
		if exists(filepath.Join(root, "mvnw")) {
			tool = "./mvnw"
		}
		for _, off := range []bool{true, false} {
			args := []string{"-q", "dependency:build-classpath", "-Dmdep.outputFile=" + outFile, "-Dmdep.includeScope=test"}
			if off {
				args = append([]string{"-o"}, args...)
			}
			if _, err := runQuiet(ctx, root, tool, args...); err == nil {
				b, _ := os.ReadFile(outFile)
				for _, p := range strings.Split(strings.TrimSpace(string(b)), string(os.PathListSeparator)) {
					if strings.HasSuffix(p, ".jar") {
						jars = append(jars, p)
					}
				}
				if len(jars) > 0 {
					return dedupe(jars), "resolved by Maven"
				}
			}
		}
	}
	return groupJars(kind, root), "the build did not resolve; newest cached jars of the groups in the build files"
}

func dedupe(l []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range l {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

var groupRe = regexp.MustCompile(`["'(]([a-z][\w-]*(?:\.[\w-]+)+):[\w.-]+|<groupId>([\w.-]+)</groupId>|group\s*=\s*"([\w.-]+)"`)

// groupJars takes, for each group named in the build files, the newest
// version of every artifact in the Gradle or Maven cache.
func groupJars(kind, root string) []string {
	groups := map[string]bool{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == "build" || d.Name() == "target" || d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !d.IsDir() && buildFileRe.MatchString(p) {
			b, _ := os.ReadFile(p)
			for _, m := range groupRe.FindAllStringSubmatch(string(b), -1) {
				groups[m[1]+m[2]+m[3]] = true
			}
		}
		return nil
	})
	h, _ := os.UserHomeDir()
	var jars []string
	for g := range groups {
		var gdir string
		if kind == "maven" {
			gdir = filepath.Join(h, ".m2", "repository", filepath.FromSlash(strings.ReplaceAll(g, ".", "/")))
		} else {
			gdir = filepath.Join(h, ".gradle", "caches", "modules-2", "files-2.1", g)
		}
		arts, _ := os.ReadDir(gdir)
		for _, a := range arts {
			vers, _ := os.ReadDir(filepath.Join(gdir, a.Name()))
			var names []string
			for _, v := range vers {
				names = append(names, v.Name())
			}
			sort.Slice(names, func(i, j int) bool { return versionLess(names[i], names[j]) })
			for i := len(names) - 1; i >= 0; i-- {
				var found []string
				filepath.WalkDir(filepath.Join(gdir, a.Name(), names[i]), func(p string, d os.DirEntry, err error) error {
					if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jar") && !strings.HasSuffix(p, "-sources.jar") && !strings.HasSuffix(p, "-javadoc.jar") {
						found = append(found, p)
					}
					return nil
				})
				if len(found) > 0 {
					jars = append(jars, found[0])
					break
				}
			}
		}
	}
	sort.Strings(jars)
	return jars
}

var numRe = regexp.MustCompile(`\d+|\D+`)

func versionLess(a, b string) bool {
	pa, pb := numRe.FindAllString(a, -1), numRe.FindAllString(b, -1)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea == nil && eb == nil {
			return na < nb
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}

var facadeRe = regexp.MustCompile(`Kt(__\w+)?$`)

// indexJar lists the classes of a jar, and the top-level Kotlin functions
// and properties in its facade classes.
func indexJar(path string) []lookupEntry {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil
	}
	defer zr.Close()
	jar := filepath.Base(path)
	var out []lookupEntry
	for _, f := range zr.File {
		n := f.Name
		if !strings.HasSuffix(n, ".class") {
			continue
		}
		if strings.HasPrefix(n, "META-INF/") {
			continue // multi-release copies repeat the base classes
		}
		cls := strings.TrimSuffix(n, ".class")
		if strings.HasSuffix(cls, "module-info") || strings.HasSuffix(cls, "package-info") {
			continue
		}
		pkg, simple := "", cls
		if i := strings.LastIndexByte(cls, '/'); i >= 0 {
			pkg, simple = strings.ReplaceAll(cls[:i], "/", "."), cls[i+1:]
		}
		if anonRe.MatchString(simple) {
			continue
		}
		fq := strings.ReplaceAll(strings.ReplaceAll(cls, "/", "."), "$", ".")
		parts := strings.Split(simple, "$")
		out = append(out, lookupEntry{simple: parts[len(parts)-1], fq: fq, kind: "class", jar: jar})
		if !strings.Contains(simple, "$") && facadeRe.MatchString(simple) {
			rc, err := f.Open()
			if err != nil {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(rc, 8<<20))
			rc.Close()
			if err != nil {
				continue
			}
			for _, m := range staticMembers(b) {
				e := lookupEntry{simple: m.name, kind: m.kind, jar: jar}
				if pkg != "" {
					e.fq = pkg + "." + m.name
				} else {
					e.fq = m.name
				}
				out = append(out, e)
			}
		}
	}
	return out
}

var anonRe = regexp.MustCompile(`\$\d|\$\$|^\d`)

type member struct{ name, kind string }

// staticMembers reads a class file and returns its public static, not
// synthetic methods (fun) and fields (val); a getX() method also gives val x.
func staticMembers(b []byte) []member {
	r := &cfReader{b: b}
	if r.u4() != 0xCAFEBABE {
		return nil
	}
	r.skip(4)
	n := int(r.u2())
	utf := make([]string, n)
	for i := 1; i < n && r.ok(); i++ {
		switch tag := r.u1(); tag {
		case 1:
			l := int(r.u2())
			utf[i] = string(r.bytes(l))
		case 3, 4, 9, 10, 11, 12, 17, 18:
			r.skip(4)
		case 5, 6:
			r.skip(8)
			i++
		case 7, 8, 16, 19, 20:
			r.skip(2)
		case 15:
			r.skip(3)
		default:
			return nil
		}
	}
	r.skip(6)
	r.skip(2 * int(r.u2()))
	var out []member
	seen := map[member]bool{}
	add := func(m member) {
		if m.name != "" && !seen[m] && !strings.ContainsAny(m.name, "$<-") {
			seen[m] = true
			out = append(out, m)
		}
	}
	for pass := 0; pass < 2 && r.ok(); pass++ {
		cnt := int(r.u2())
		for i := 0; i < cnt && r.ok(); i++ {
			acc, name := r.u2(), int(r.u2())
			r.skip(2)
			attrs := int(r.u2())
			for j := 0; j < attrs && r.ok(); j++ {
				r.skip(2)
				r.skip(int(r.u4()))
			}
			if acc&0x0009 != 0x0009 || acc&0x1000 != 0 || name >= len(utf) {
				continue
			}
			nm := utf[name]
			if pass == 0 {
				add(member{nm, "val"})
				continue
			}
			add(member{nm, "fun"})
			for _, pre := range []string{"get", "is"} {
				if rest, ok := strings.CutPrefix(nm, pre); ok && rest != "" && rest[0] >= 'A' && rest[0] <= 'Z' {
					p := strings.ToLower(rest[:1]) + rest[1:]
					if pre == "is" {
						p = nm
					}
					add(member{p, "val"})
				}
			}
		}
	}
	if !r.ok() {
		return nil
	}
	return out
}

type cfReader struct {
	b   []byte
	at  int
	bad bool
}

func (r *cfReader) ok() bool { return !r.bad }
func (r *cfReader) need(n int) bool {
	if r.bad || r.at+n > len(r.b) || n < 0 {
		r.bad = true
		return false
	}
	return true
}
func (r *cfReader) u1() byte {
	if !r.need(1) {
		return 0
	}
	r.at++
	return r.b[r.at-1]
}
func (r *cfReader) u2() uint16 {
	if !r.need(2) {
		return 0
	}
	r.at += 2
	return binary.BigEndian.Uint16(r.b[r.at-2:])
}
func (r *cfReader) u4() uint32 {
	if !r.need(4) {
		return 0
	}
	r.at += 4
	return binary.BigEndian.Uint32(r.b[r.at-4:])
}
func (r *cfReader) bytes(n int) []byte {
	if !r.need(n) {
		return nil
	}
	r.at += n
	return r.b[r.at-n : r.at]
}
func (r *cfReader) skip(n int) { r.bytes(n) }

func lookupDir() (string, error) {
	d, err := qhDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "lookup"), nil
}

// loadIndex returns the index of the project at cwd, building it when the
// build files changed since the last one.
func loadIndex(ctx context.Context, cwd string) (*lookupIndex, error) {
	kind, root := buildRoot(cwd)
	if kind == "" {
		return nil, errors.New("no Gradle or Maven build found above " + cwd)
	}
	base, err := lookupDir()
	if err != nil {
		return nil, err
	}
	work := filepath.Join(base, strconv.FormatUint(contentHash([]byte(root)), 36))
	os.MkdirAll(work, 0o755)
	key := buildKey(root)
	idxFile := filepath.Join(work, "index-"+key+".tsv")
	lookupMu.Lock()
	defer lookupMu.Unlock()
	if ix := lookupCache[idxFile]; ix != nil {
		return ix, nil
	}
	if ix, err := readIndex(idxFile); err == nil {
		lookupCache[idxFile] = ix
		return ix, nil
	}
	jars, note := projectJars(ctx, kind, root, work)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#\t%s (%d jars)\n", note, len(jars))
	for _, j := range jars {
		for _, e := range indexJar(j) {
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", e.simple, e.fq, e.kind, e.jar)
		}
	}
	old, _ := filepath.Glob(filepath.Join(work, "index-*.tsv"))
	for _, o := range old {
		os.Remove(o)
	}
	if err := os.WriteFile(idxFile+".tmp", []byte(b.String()), 0o644); err == nil {
		os.Rename(idxFile+".tmp", idxFile)
	}
	ix, err := readIndex(idxFile)
	if err != nil {
		return nil, err
	}
	lookupCache[idxFile] = ix
	return ix, nil
}

func readIndex(p string) (*lookupIndex, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ix := &lookupIndex{bySimp: map[string][]int{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if n, ok := strings.CutPrefix(l, "#\t"); ok {
			ix.note = n
			continue
		}
		p := strings.Split(l, "\t")
		if len(p) != 4 {
			continue
		}
		ix.bySimp[p[0]] = append(ix.bySimp[p[0]], len(ix.entries))
		ix.entries = append(ix.entries, lookupEntry{p[0], p[1], p[2], p[3]})
	}
	return ix, sc.Err()
}

// find answers a lookup: exact simple name, or a dotted name matched on the
// end of the full name; case-insensitive only when nothing matches exactly.
func (ix *lookupIndex) find(name string) []lookupEntry {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "import "))
	simple := name
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		simple = name[i+1:]
	}
	var hits []lookupEntry
	for _, i := range ix.bySimp[simple] {
		e := ix.entries[i]
		if simple == name || e.fq == name || strings.HasSuffix(e.fq, "."+name) {
			hits = append(hits, e)
		}
	}
	if len(hits) == 0 && simple == name {
		low := strings.ToLower(name)
		for _, e := range ix.entries {
			if strings.ToLower(e.simple) == low {
				hits = append(hits, e)
			}
		}
	}
	return hits
}

func (t *Tools) lookup(ctx context.Context, name string) (string, bool) {
	if strings.TrimSpace(name) == "" {
		return `error: lookup needs the "name" argument`, false
	}
	ix, err := loadIndex(ctx, t.Cwd)
	if err != nil {
		return "error: lookup: " + err.Error(), false
	}
	hits := ix.find(name)
	head := ""
	if i := strings.LastIndexByte(name, '.'); len(hits) == 0 && i >= 0 {
		// Receiver.fun of an extension, or a wrong package: try the simple name.
		if hits = ix.find(name[i+1:]); len(hits) > 0 {
			head = fmt.Sprintf("no import ends with %s; these are the ones named %s:\n", name, name[i+1:])
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("%s: not found in the project's libraries (%s)\n", name, ix.note), true
	}
	// One line per import; the kinds and jars of the same import are joined.
	type agg struct {
		kinds, jars []string
	}
	m := map[string]*agg{}
	var order []string
	for _, h := range hits {
		a := m[h.fq]
		if a == nil {
			a = &agg{}
			m[h.fq] = a
			order = append(order, h.fq)
		}
		if !contains(a.kinds, h.kind) {
			a.kinds = append(a.kinds, h.kind)
		}
		if !contains(a.jars, h.jar) {
			a.jars = append(a.jars, h.jar)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return len(order[i]) < len(order[j]) })
	var b strings.Builder
	b.WriteString(head)
	for i, fq := range order {
		if i == lookupMax {
			fmt.Fprintf(&b, "[%d more]\n", len(order)-i)
			break
		}
		a := m[fq]
		fmt.Fprintf(&b, "import %s  (%s, %s)\n", fq, strings.Join(a.kinds, "/"), strings.Join(a.jars, ", "))
	}
	return b.String(), true
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func lookupCmd(args []string) error {
	fs := flag.NewFlagSet("lookup", flag.ExitOnError)
	dir := fs.String("C", ".", "project folder")
	fs.Parse(args)
	d, err := absDir(*dir)
	if err != nil {
		return err
	}
	for _, n := range fs.Args() {
		t0 := time.Now()
		out, ok := (&Tools{Cwd: d}).lookup(context.Background(), n)
		fmt.Print(out)
		fmt.Fprintf(os.Stderr, "[%s: %.2fs]\n", n, time.Since(t0).Seconds())
		if !ok {
			return errors.New("lookup failed")
		}
	}
	return nil
}
