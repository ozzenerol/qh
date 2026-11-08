package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type cfMember struct {
	flags uint16
	name  string
}

// classFile writes a minimal class file with the given fields and methods.
func classFile(name string, fields, methods []cfMember) []byte {
	var pool [][]byte
	idx := map[string]uint16{}
	utf := func(s string) uint16 {
		if i, ok := idx[s]; ok {
			return i
		}
		e := []byte{1, byte(len(s) >> 8), byte(len(s))}
		pool = append(pool, append(e, s...))
		idx[s] = uint16(len(pool))
		return idx[s]
	}
	cls := func(s string) uint16 {
		n := utf(s)
		pool = append(pool, []byte{7, byte(n >> 8), byte(n)})
		return uint16(len(pool))
	}
	this, super := cls(name), cls("java/lang/Object")
	pool = append(pool, []byte{5, 0, 0, 0, 0, 0, 0, 0, 1}, nil) // a long takes two slots
	desc := utf("()V")
	var b bytes.Buffer
	w := func(v any) { binary.Write(&b, binary.BigEndian, v) }
	mem := func(ms []cfMember) {
		w(uint16(len(ms)))
		for _, m := range ms {
			w(m.flags)
			w(utf(m.name))
			w(desc)
			w(uint16(1))
			w(utf("Code"))
			w(uint32(2))
			w(uint16(0))
		}
	}
	var body bytes.Buffer
	b, body = body, b
	w(uint16(0x21))
	w(this)
	w(super)
	w(uint16(0))
	mem(fields)
	mem(methods)
	w(uint16(0))
	b, body = body, b
	w(uint32(0xCAFEBABE))
	w(uint32(61))
	w(uint16(len(pool) + 1))
	for _, e := range pool {
		b.Write(e)
	}
	b.Write(body.Bytes())
	return b.Bytes()
}

func writeJar(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write(c)
	}
	zw.Close()
	f.Close()
}

func testJar(t *testing.T, path string) {
	pub := uint16(0x0009)
	writeJar(t, path, map[string][]byte{
		"org/x/route/RouteDslKt.class": classFile("org/x/route/RouteDslKt", []cfMember{{pub, "MAX"}, {0x000A, "hidden"}},
			[]cfMember{{pub, "routes"}, {pub | 0x1000, "routes$default"}, {0x000A, "helper"}, {pub, "getDefaultTimeout"}, {0x0001, "instance"}}),
		"org/x/route/RedisRateLimiter.class":        classFile("org/x/route/RedisRateLimiter", nil, nil),
		"org/x/route/RedisRateLimiter$Config.class": classFile("org/x/route/RedisRateLimiter$Config", nil, nil),
		"org/x/route/RedisRateLimiter$1.class":      classFile("org/x/route/RedisRateLimiter$1", nil, nil),
		"org/y/Other.class":                         classFile("org/y/Other", nil, nil),
		"META-INF/versions/11/org/y/Other.class":    classFile("org/y/Other", nil, nil),
		"module-info.class":                         {0xCA},
	})
}

func TestStaticMembers(t *testing.T) {
	b := classFile("a/BKt", []cfMember{{0x0009, "CONST"}}, []cfMember{{0x0009, "f"}, {0x1009, "f$default"}, {0x0008, "pkg"}, {0x0009, "isReady"}, {0x0009, "getName"}})
	var got []string
	for _, m := range staticMembers(b) {
		got = append(got, m.kind+" "+m.name)
	}
	if strings.Join(got, ",") != "val CONST,fun f,fun isReady,val isReady,fun getName,val name" {
		t.Fatalf("%q", got)
	}
	if staticMembers([]byte{0xCA, 0xFE, 0xBA, 0xBE, 0, 0}) != nil || staticMembers(b[:len(b)/2]) != nil {
		t.Fatal("broken class file read as fine")
	}
}

func TestIndexJar(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x-1.0.jar")
	testJar(t, p)
	var got []string
	for _, e := range indexJar(p) {
		got = append(got, e.kind+" "+e.fq)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"fun org.x.route.routes", "val org.x.route.MAX", "val org.x.route.defaultTimeout", "class org.x.route.RedisRateLimiter", "class org.x.route.RedisRateLimiter.Config", "class org.x.route.RouteDslKt"} {
		if !strings.Contains(all, want+"\n") && !strings.HasSuffix(all, want) {
			t.Errorf("no %q in\n%s", want, all)
		}
	}
	for _, bad := range []string{"routes$default", "helper", "hidden", "instance", "$1", "module-info"} {
		if strings.Contains(all, bad) {
			t.Errorf("%q in\n%s", bad, all)
		}
	}
	if strings.Count(all, "org.y.Other") != 1 {
		t.Errorf("multi-release copy listed:\n%s", all)
	}
}

func TestLookupGradleAndCache(t *testing.T) {
	t.Setenv("QH_HOME", t.TempDir())
	dir := t.TempDir()
	jar := filepath.Join(t.TempDir(), "gw-1.0.jar")
	testJar(t, jar)
	os.WriteFile(filepath.Join(dir, "settings.gradle.kts"), []byte(`rootProject.name = "x"`), 0o644)
	os.WriteFile(filepath.Join(dir, "gradlew"), []byte("#!/bin/sh\necho run >> calls.txt\necho 'QHJAR "+jar+"'\n"), 0o755)
	os.MkdirAll(filepath.Join(dir, "app"), 0o755)
	tl := &Tools{Cwd: filepath.Join(dir, "app")}
	ctx := context.Background()
	out, ok := tl.Run(ctx, "lookup", `{"name":"routes"}`)
	if !ok || out != "import org.x.route.routes  (fun, gw-1.0.jar)\n" {
		t.Fatalf("routes: %q", out)
	}
	for name, want := range map[string]string{
		"RedisRateLimiter":                    "import org.x.route.RedisRateLimiter  (class, gw-1.0.jar)\n",
		"redisratelimiter":                    "import org.x.route.RedisRateLimiter  (class, gw-1.0.jar)\n",
		"org.x.route.RedisRateLimiter.Config": "import org.x.route.RedisRateLimiter.Config  (class, gw-1.0.jar)\n",
		"RouteLocatorBuilder.routes":          "no import ends with RouteLocatorBuilder.routes; these are the ones named routes:\nimport org.x.route.routes  (fun, gw-1.0.jar)\n",
		"nothing":                             "nothing: not found in the project's libraries (resolved by Gradle (1 jars))\n",
	} {
		if out, _ := tl.Run(ctx, "lookup", `{"name":"`+name+`"}`); out != want {
			t.Errorf("%s: %q", name, out)
		}
	}
	calls := func() int {
		b, _ := os.ReadFile(filepath.Join(dir, "calls.txt"))
		return strings.Count(string(b), "run")
	}
	if calls() != 1 {
		t.Fatalf("gradlew ran %d times", calls())
	}
	// A new process reads the index file; a changed build file builds it again.
	lookupMu.Lock()
	lookupCache = map[string]*lookupIndex{}
	lookupMu.Unlock()
	tl.Run(ctx, "lookup", `{"name":"routes"}`)
	if calls() != 1 {
		t.Fatal("index file not reused")
	}
	os.WriteFile(filepath.Join(dir, "app", "build.gradle.kts"), []byte(`dependencies {}`), 0o644)
	tl.Run(ctx, "lookup", `{"name":"routes"}`)
	if calls() != 2 {
		t.Fatalf("not rebuilt after a build file change: %d", calls())
	}
}

func TestLookupFallbackToCache(t *testing.T) {
	t.Setenv("QH_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	cache := filepath.Join(home, ".gradle", "caches", "modules-2", "files-2.1", "org.x")
	testJar(t, filepath.Join(cache, "gw", "1.10.0", "h1", "gw-1.10.0.jar"))
	writeJar(t, filepath.Join(cache, "gw", "1.9.0", "h2", "gw-1.9.0.jar"), map[string][]byte{"org/x/Old.class": classFile("org/x/Old", nil, nil)})
	writeJar(t, filepath.Join(cache, "gw", "1.10.0", "h1", "gw-1.10.0-sources.jar"), map[string][]byte{"org/x/Src.class": nil})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "build.gradle.kts"), []byte(`dependencies { implementation("org.x:gw-starter:1.0") }`), 0o644)
	os.WriteFile(filepath.Join(dir, "gradlew"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	tl := &Tools{Cwd: dir}
	out, _ := tl.Run(context.Background(), "lookup", `{"name":"routes"}`)
	if out != "import org.x.route.routes  (fun, gw-1.10.0.jar)\n" {
		t.Fatalf("%q", out)
	}
	out, _ = tl.Run(context.Background(), "lookup", `{"name":"Old"}`)
	if !strings.Contains(out, "not found") || !strings.Contains(out, "newest cached jars of the groups") {
		t.Fatalf("%q", out)
	}
}

func TestLookupNoBuild(t *testing.T) {
	out, ok := (&Tools{Cwd: t.TempDir()}).Run(context.Background(), "lookup", `{"name":"x"}`)
	if ok || !strings.Contains(out, "no Gradle or Maven build") {
		t.Fatalf("%q", out)
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess("1.9.0", "1.10.0") || versionLess("2.0", "1.10") || !versionLess("1.0-RC1", "1.0.1") {
		t.Fatal("versionLess")
	}
}
