package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// extEval runs the eval tasks through another qh binary (QH_EVAL_BIN), so a
// build without `qh eval` can be measured the same way. The binary gets its
// own daemon in a temporary QH_HOME, and its model requests go through a
// proxy here that adds up the token counts of each request.
type extEval struct {
	bin  string
	home string
	sock string
	srv  *exec.Cmd
	px   *usageProxy
}

// usageProxy forwards the model requests and reads the usage chunk of each
// streamed answer.
type usageProxy struct {
	origin string // scheme://host of the model server
	ln     net.Listener

	mu                     sync.Mutex
	reqs, last, fresh, out int
}

func (p *usageProxy) reset() {
	p.mu.Lock()
	p.reqs, p.last, p.fresh, p.out = 0, 0, 0, 0
	p.mu.Unlock()
}

func (p *usageProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.origin+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	rd := bufio.NewReader(resp.Body)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			w.Write(line)
			if fl != nil {
				fl.Flush()
			}
			p.usage(line)
		}
		if err != nil {
			return
		}
	}
}

func (p *usageProxy) usage(line []byte) {
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return
	}
	var c struct {
		Usage *Usage `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data: ")), &c) != nil || c.Usage == nil {
		return
	}
	p.mu.Lock()
	p.reqs++
	p.last = c.Usage.PromptTokens
	p.fresh += c.Usage.PromptTokens - c.Usage.Details.CachedTokens
	p.out += c.Usage.CompletionTokens
	p.mu.Unlock()
}

func startExtEval(bin, modelURL string) (*extEval, error) {
	u, err := url.Parse(modelURL)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	px := &usageProxy{origin: u.Scheme + "://" + u.Host, ln: ln}
	go http.Serve(ln, px)
	home, err := os.MkdirTemp("", "qh-ext-")
	if err != nil {
		ln.Close()
		return nil, err
	}
	if d, err := qhDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(d, "config.json")); err == nil {
			os.WriteFile(filepath.Join(home, "config.json"), b, 0o600)
		}
	}
	e := &extEval{bin: bin, home: home, sock: filepath.Join(home, "qh.sock"), px: px}
	e.srv = exec.Command(bin, "serve")
	e.srv.Env = append(os.Environ(), "QH_HOME="+home, "QH_SOCK="+e.sock, "QH_URL=http://"+ln.Addr().String()+u.Path)
	logf, _ := os.Create(filepath.Join(home, "serve.log"))
	e.srv.Stdout, e.srv.Stderr = logf, logf
	if err := e.srv.Start(); err != nil {
		e.close()
		return nil, err
	}
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", e.sock); err == nil {
			c.Close()
			return e, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.close()
	return nil, fmt.Errorf("%s serve did not open %s", bin, e.sock)
}

func (e *extEval) close() {
	if e.srv != nil && e.srv.Process != nil {
		if c, err := net.Dial("unix", e.sock); err == nil {
			b, _ := json.Marshal(ClientReq{Op: "stop"})
			c.Write(append(b, '\n'))
			io.Copy(io.Discard, c)
			c.Close()
		}
		done := make(chan struct{})
		go func() { e.srv.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			e.srv.Process.Kill()
			<-done
		}
	}
	e.px.ln.Close()
	os.RemoveAll(e.home)
}

// evalExt is set when QH_EVAL_BIN names the binary to measure.
var evalExt *extEval

// extStats is the part of Stats that every qh version sends in its done event.
type extStats struct {
	Steps    int  `json:"steps"`
	ToolErrs int  `json:"tool_errors"`
	CmdFails int  `json:"failed_commands"`
	Limit    bool `json:"step_limit"`
}

// run sends one task to the daemon and waits for its done event.
func (e *extEval) run(ctx context.Context, name, repo, prompt string, log io.Writer) (extStats, error) {
	var st extStats
	e.px.reset()
	c, err := net.Dial("unix", e.sock)
	if err != nil {
		return st, err
	}
	defer c.Close()
	go func() { <-ctx.Done(); c.Close() }() // closing the socket cancels the run
	b, _ := json.Marshal(ClientReq{Op: "run", Session: name, Cwd: repo, Prompt: prompt, New: true, From: "eval"})
	if _, err := c.Write(append(b, '\n')); err != nil {
		return st, err
	}
	dec := json.NewDecoder(c)
	for {
		var ev struct {
			T     string          `json:"t"`
			Tool  string          `json:"tool"`
			Text  string          `json:"text"`
			Stats json.RawMessage `json:"stats"`
		}
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return st, ctx.Err()
			}
			return st, fmt.Errorf("the daemon closed the connection: %v", err)
		}
		switch ev.T {
		case "tool":
			fmt.Fprintf(log, "  %s %s\n", ev.Tool, oneLine(ev.Text, 100))
		case "error":
			fmt.Fprintf(log, "  error: %s\n", ev.Text)
		case "done":
			if len(ev.Stats) > 0 {
				json.Unmarshal(ev.Stats, &st)
			}
			return st, nil
		}
	}
}

// fill copies the measured numbers into r.
func (e *extEval) fill(r *EvalResult, st extStats, secs float64) {
	e.px.mu.Lock()
	defer e.px.mu.Unlock()
	r.Steps, r.ToolErrs, r.CmdFails, r.Limit = st.Steps, st.ToolErrs, st.CmdFails, st.Limit
	r.Prompt, r.Fresh, r.Output, r.Secs, r.Requests = e.px.last, e.px.fresh, e.px.out, secs, e.px.reqs
}
