package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSamplingSent(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	var c Config
	if err := applySet(&c, "temperature=0.6,top_k=20"); err != nil {
		t.Fatal(err)
	}
	l := &LLM{BaseURL: srv.URL, Model: "m", HTTP: srv.Client(), Sample: c.Sampling}
	if _, _, err := l.Chat(context.Background(), Request{MaxTok: 10}, func(Delta) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if got["temperature"] != 0.6 || got["top_k"] != 20.0 {
		t.Fatalf("temperature %v top_k %v", got["temperature"], got["top_k"])
	}
	if _, ok := got["top_p"]; ok {
		t.Fatal("top_p sent although not set")
	}
	l.Sample = Sampling{}
	got = nil
	l.Chat(context.Background(), Request{MaxTok: 10}, func(Delta) bool { return true })
	if _, ok := got["temperature"]; ok {
		t.Fatal("temperature sent although not set")
	}
}
