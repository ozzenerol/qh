package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("q") != "go context" || r.URL.Query().Get("format") != "json" {
			t.Errorf("request %s", r.URL)
		}
		w.Write([]byte(`{"results":[{"title":"context package - Go","url":"https://pkg.go.dev/context","content":"Package <b>context</b> defines the Context type."}]}`))
	}))
	defer srv.Close()
	out, ok := webSearch(context.Background(), srv.URL+"/", "go context")
	want := "1. context package - Go\n   https://pkg.go.dev/context\n   Package context defines the Context type."
	if !ok || out != want {
		t.Fatalf("got %q", out)
	}
	if s := shortResult("web_search", "", out, true); s != "1 result" {
		t.Errorf("short %q", s)
	}
	if _, ok := webSearch(context.Background(), "", "x"); ok {
		t.Error("no search_url accepted")
	}
}

func TestFetchURL(t *testing.T) {
	page := `<html><head><title>x</title><style>p{}</style></head><body><nav>menu</nav>
<h2>Usage</h2><p>Call &lt;b&gt; <a href="/">WithTimeout</a>.</p><ul><li>one</li><li>two</li></ul>
<pre>ctx, cancel :=
    context.WithTimeout(ctx, d)</pre><script>alert(1)</script></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	}))
	defer srv.Close()
	out, ok := fetchURL(context.Background(), srv.URL, 0)
	want := "## Usage\nCall <b> WithTimeout.\n\n- one\n- two\n\n```\nctx, cancel :=\n    context.WithTimeout(ctx, d)\n```"
	if !ok || out != want {
		t.Fatalf("got %q", out)
	}
	out, _ = fetchURL(context.Background(), srv.URL, 5)
	if !strings.HasPrefix(out, "age\n") || !strings.Contains(out, "[chars 5-") {
		t.Errorf("offset: %q", out)
	}
	if _, ok := fetchURL(context.Background(), "file:///etc/passwd", 0); ok {
		t.Error("file URL accepted")
	}
}

func TestWebOff(t *testing.T) {
	tl := &Tools{Cwd: t.TempDir()}
	if out, ok := tl.Run(context.Background(), "web_search", `{"query":"x"}`); ok || !strings.Contains(out, "off") {
		t.Errorf("got %v %q", ok, out)
	}
}
