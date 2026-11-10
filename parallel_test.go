package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// inflightModel holds each answer for a while and records the most requests
// it had open at once.
type inflightModel struct {
	mu       sync.Mutex
	open     int
	max      int
	holdTime time.Duration
}

func (m *inflightModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		fmt.Fprint(w, `{"data":[{"id":"/m/p"}]}`)
		return
	}
	m.mu.Lock()
	m.open++
	if m.open > m.max {
		m.max = m.open
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.open--
		m.mu.Unlock()
	}()
	select {
	case <-time.After(m.holdTime):
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
}

func parallelMax(t *testing.T, parallel int) (int, []Event) {
	m := &inflightModel{holdTime: 200 * time.Millisecond}
	ts := httptest.NewServer(m)
	defer ts.Close()
	home := t.TempDir()
	t.Setenv("QH_HOME", home)
	t.Setenv("QH_URL", ts.URL+"/v1")
	if parallel > 0 {
		os.WriteFile(filepath.Join(home, "config.json"), []byte(fmt.Sprintf(`{"parallel": %d}`, parallel)), 0o600)
	}
	done := make(chan error, 1)
	go func() { done <- serve() }()
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(sockPath()); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cwd := t.TempDir()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var infos []Event
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, dec, err := send(ClientReq{Op: "run", Session: fmt.Sprintf("p%d", i), Cwd: cwd, Prompt: "hi", From: "opus"})
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			for {
				var e Event
				if dec.Decode(&e) != nil || e.T == "done" {
					return
				}
				if e.T == "info" {
					mu.Lock()
					infos = append(infos, e)
					mu.Unlock()
				}
			}
		}(i)
	}
	wg.Wait()
	simple(ClientReq{Op: "stop"})
	<-done
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.max, infos
}

func TestParallelGate(t *testing.T) {
	if got, infos := parallelMax(t, 0); got != 1 || len(infos) == 0 {
		t.Fatalf("default: %d requests at once, infos %v", got, infos)
	}
	if got, infos := parallelMax(t, 3); got != 3 || len(infos) != 0 {
		t.Fatalf("parallel 3: %d requests at once, infos %v", got, infos)
	}
}
