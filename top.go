package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// daemon is one qh daemon found by qh top: a folder like ~/.qh or ~/.qh-kc
// with a socket in it.
type daemon struct {
	dir  string
	host string // host:port of the model server, from config.json
	up   bool
	rows []SessionInfo
}

func findDaemons() []*daemon {
	h, _ := os.UserHomeDir()
	dirs, _ := filepath.Glob(filepath.Join(h, ".qh*"))
	if d := os.Getenv("QH_HOME"); d != "" {
		dirs = append([]string{d}, dirs...)
	}
	var out []*daemon
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		if _, err := os.Stat(filepath.Join(d, "qh.sock")); err != nil {
			continue
		}
		dm := &daemon{dir: d, host: "127.0.0.1:8080"}
		var c Config
		if b, err := os.ReadFile(filepath.Join(d, "config.json")); err == nil && json.Unmarshal(b, &c) == nil && c.URL != "" {
			if u, err := url.Parse(c.URL); err == nil {
				dm.host = u.Host
			}
		}
		out = append(out, dm)
	}
	return out
}

// poll asks the daemon for its sessions. It never starts a daemon.
func (d *daemon) poll() {
	d.up, d.rows = false, nil
	c, err := net.DialTimeout("unix", filepath.Join(d.dir, "qh.sock"), 300*time.Millisecond)
	if err != nil {
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	b, _ := json.Marshal(ClientReq{Op: "list"})
	c.Write(append(b, '\n'))
	var e Event
	if json.NewDecoder(c).Decode(&e) != nil {
		return
	}
	d.up = true
	json.Unmarshal([]byte(e.Text), &d.rows)
}

// panel draws one daemon in w columns.
func (d *daemon) panel(w, frame int) []string {
	name := shortPath(d.dir)
	dot := cRed + "●" + cGray + " down"
	if d.up {
		dot = cGreen + "●"
	}
	busy := "idle"
	var live, recent []SessionInfo
	for _, r := range d.rows {
		switch {
		case r.Running:
			live = append(live, r)
			if !r.Waiting {
				busy = "busy"
			}
		case time.Since(r.Updated) < time.Hour && r.Msgs > 0:
			recent = append(recent, r)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Name < live[j].Name })
	out := []string{
		clipCols(" "+cBold+name+cReset+cGray+" → "+d.host+" "+dot, w),
		clipCols(" "+cGray+"model "+busy+fmt.Sprintf(" · %d running · %d in the last hour", len(live), len(recent)), w),
		"",
	}
	if !d.up {
		return out
	}
	for _, r := range live {
		mark := cAccent + spinFrames[frame%len(spinFrames)]
		state := fmt.Sprintf("step %d", r.Step)
		if r.Waiting {
			mark, state = cYou+"…", "waits for the model"
		}
		from := byLabel(r.From)
		out = append(out,
			clipCols(" "+mark+cReset+" "+cBold+r.Name+cReset+" "+cOpus+from+cReset+cGray+" "+state, w),
			clipCols("   "+cGray+"ctx "+human(r.Ctx)+" · "+cReset+r.Last, w))
	}
	if len(live) > 0 && len(recent) > 0 {
		out = append(out, "")
	}
	for i, r := range recent {
		if i == 6 {
			out = append(out, clipCols("   "+cGray+fmt.Sprintf("… %d more", len(recent)-6), w))
			break
		}
		out = append(out, clipCols(" "+cGreen+"✓"+cReset+" "+r.Name+" "+cOpus+byLabel(r.By)+cGray+" "+ago(r.Updated)+" · "+plural(r.Turns, "turn")+" · ctx "+human(r.Ctx), w))
	}
	if len(live) == 0 && len(recent) == 0 {
		out = append(out, " "+cGray+"nothing in the last hour")
	}
	return out
}

func topCmd() error {
	ds := findDaemons()
	if len(ds) == 0 {
		return fmt.Errorf("no daemon found (no ~/.qh*/qh.sock)")
	}
	fd := int(os.Stdin.Fd())
	restore, err := makeRaw(fd)
	if err != nil {
		return err
	}
	out := os.Stdout
	out.WriteString("\x1b[?1049h\x1b[?25l")
	defer func() {
		out.WriteString("\x1b[?25h\x1b[?1049l")
		restore()
	}()
	keys := make(chan byte, 8)
	go func() {
		b := make([]byte, 64)
		for {
			n, err := os.Stdin.Read(b)
			if err != nil {
				close(keys)
				return
			}
			for _, c := range b[:n] {
				keys <- c
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	frame, last := 0, time.Time{}
	for {
		if time.Since(last) > time.Second {
			for _, d := range ds {
				d.poll()
			}
			last = time.Now()
		}
		out.WriteString(topFrame(ds, termWidth(int(out.Fd())), frame))
		select {
		case c, ok := <-keys:
			if !ok || c == 'q' || c == 0x1b || c == 3 {
				return nil
			}
		case <-winch:
		case <-tick.C:
			frame++
		}
	}
}

// topFrame is one screen of qh top: the daemons side by side when two fit,
// one under the other when not.
func topFrame(ds []*daemon, width, frame int) string {
	var lines []string
	head := " " + cQwen + cBold + "qh top" + cReset + cGray + fmt.Sprintf("  %s", plural(len(ds), "daemon"))
	lines = append(lines, clipCols(head+strings.Repeat(" ", max(1, width-1-visible(head)-8))+"q quits", width-1), "")
	if len(ds) == 2 && width >= 80 {
		cw := (width - 2) / 2
		a, b := ds[0].panel(cw, frame), ds[1].panel(cw, frame)
		for i := 0; i < max(len(a), len(b)); i++ {
			l, r := "", ""
			if i < len(a) {
				l = a[i]
			}
			if i < len(b) {
				r = b[i]
			}
			lines = append(lines, padRight(l, cw)+cReset+cFrame+"│"+cReset+r)
		}
	} else {
		for i, d := range ds {
			if i > 0 {
				lines = append(lines, cFrame+strings.Repeat("─", width-1))
			}
			lines = append(lines, d.panel(width-1, frame)...)
		}
	}
	var b strings.Builder
	b.WriteString("\x1b[?2026h\x1b[H")
	for _, l := range lines {
		b.WriteString(l + cReset + "\x1b[K\r\n")
	}
	b.WriteString("\x1b[J\x1b[?2026l")
	return b.String()
}
