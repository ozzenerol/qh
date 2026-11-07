package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type slashCmd struct{ name, desc string }

var slashCmds = []slashCmd{
	{"/help", "keys and commands"},
	{"/new", "clear this session and start over"},
	{"/resume", "switch to another session"},
	{"/sessions", "list the saved sessions"},
	{"/compact", "summarize the conversation to free context"},
	{"/think on", "Qwen thinks before it answers"},
	{"/think off", "answer at once, faster and less careful"},
	{"/clear", "clear the screen"},
	{"/exit", "quit"},
}

func helpLines() []string {
	row := func(k, d string) string {
		return "  " + cBold + k + cReset + strings.Repeat(" ", max(1, 16-utf8.RuneCountInString(k))) + cGray + d
	}
	out := []string{"", cAccent + cBold + "Keys" + cReset}
	for _, kv := range [][2]string{
		{"enter", "send"},
		{"ctrl+j", "new line (pasted text keeps its lines)"},
		{"esc", "stop the answer"},
		{"↑ ↓", "history"},
		{"/", "commands"},
		{"ctrl+t", "show or hide the thinking"},
		{"ctrl+o", "show or hide the last compaction summary"},
		{"ctrl+l", "clear the screen"},
		{"ctrl+c ×2", "quit"},
	} {
		out = append(out, row(kv[0], kv[1]))
	}
	out = append(out, "", cAccent+cBold+"Commands"+cReset)
	for _, c := range slashCmds {
		out = append(out, row(c.name, c.desc))
	}
	out = append(out, "", cAccent+cBold+"From a shell"+cReset,
		row("qh -c", "continue the latest session of this folder"),
		row("qh -r", "pick a session"),
		row("qh run -from opus -s main \"...\"", ""),
		"  "+cGray+"writes into this chat; the message shows with the label opus")
	return out
}

// sugs returns the commands that match what the user typed after "/".
func (t *tui) sugs() []slashCmd {
	in := string(t.buf)
	if !strings.HasPrefix(in, "/") || strings.Contains(in, "\n") {
		t.sugFor = ""
		return nil
	}
	if in != t.sugFor {
		t.sugFor, t.sugSel = in, 0
	}
	var out []slashCmd
	for _, c := range slashCmds {
		if strings.HasPrefix(c.name, in) && c.name != in || c.name == in && len(in) > 1 {
			out = append(out, c)
		}
	}
	if t.sugSel >= len(out) {
		t.sugSel = max(0, len(out)-1)
	}
	return out
}

var spinFrames = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"}

type tui struct {
	out     *os.File
	width   int
	session string
	cwd     string
	me      string // our "from" label

	// input
	buf      []rune
	cur      int
	hist     []string
	histPos  int
	draft    []rune
	ctrlC    time.Time
	pasting  bool
	pending  []byte
	escTimer *time.Timer

	// the run that the session shows now
	running   bool
	runFrom   string
	runStart  time.Time
	thinkN    int
	tool      string // tool that runs now, already formatted
	textPart  string
	thinkPart string
	inMsg     bool
	inThink   bool
	md        mdState
	showThink bool
	frame     int

	model       string
	ctx         int
	window      int
	compacting  bool
	lastSummary string
	showSummary bool
	lastTS      string
	notice      string

	picker     *picker
	gen        int
	stopAttach chan struct{}

	sugSel int
	sugFor string

	liveRows  int // rows of the live area at the last draw
	cursorRow int // row of the cursor inside the live area
	attached  bool
}

func (t *tui) w(s string) { t.out.WriteString(s) }

// erase removes the live area (spinner, input box, status line).
func (t *tui) erase() {
	if t.liveRows == 0 {
		return
	}
	if t.cursorRow > 0 {
		t.w(fmt.Sprintf("\x1b[%dA", t.cursorRow))
	}
	t.w("\r\x1b[J")
	t.liveRows, t.cursorRow = 0, 0
}

// print writes lines above the live area.
func (t *tui) print(lines ...string) {
	t.erase()
	for _, l := range lines {
		t.w(l + cReset + "\n")
	}
}

func displayName(s string) string {
	switch s {
	case "ls":
		return "List"
	case "":
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (t *tui) label(from string) string {
	switch {
	case from == t.me || from == "":
		return ""
	case strings.HasPrefix(from, "chat:"), from == "chat":
		return "" // the user, in this or an earlier chat window
	case from == "qh run":
		return "run"
	}
	return from
}

func (t *tui) inner() int { return max(20, t.width-2) }

// flushText prints the complete lines of streamed answer text.
func (t *tui) flushText(all bool) {
	for {
		i := strings.IndexByte(t.textPart, '\n')
		if i < 0 {
			if !all || t.textPart == "" {
				return
			}
			i = len(t.textPart)
		}
		line := t.textPart[:i]
		if i < len(t.textPart) {
			t.textPart = t.textPart[i+1:]
		} else {
			t.textPart = ""
		}
		if !t.inMsg && strings.TrimSpace(line) == "" {
			continue
		}
		t.textLine(line)
	}
}

func (t *tui) textLine(line string) {
	prefix := "  "
	if !t.inMsg {
		t.print("")
		prefix = cUser + "● " + cReset
		t.inMsg = true
		t.md = mdState{}
	}
	rendered := t.md.line(line)
	for i, l := range wrap(rendered, t.width-3) {
		if i > 0 {
			prefix = "  "
		}
		t.print(prefix + l)
	}
}

func (t *tui) flushThink(all bool) {
	for {
		i := strings.IndexByte(t.thinkPart, '\n')
		if i < 0 {
			if !all || t.thinkPart == "" {
				return
			}
			i = len(t.thinkPart)
		}
		line := t.thinkPart[:i]
		if i < len(t.thinkPart) {
			t.thinkPart = t.thinkPart[i+1:]
		} else {
			t.thinkPart = ""
		}
		if !t.inThink && strings.TrimSpace(line) == "" {
			continue
		}
		prefix := "  "
		if !t.inThink {
			t.print("")
			prefix = cGray + "✻ " + cReset
			t.inThink = true
		}
		for i, l := range wrap(line, t.width-3) {
			if i > 0 {
				prefix = "  "
			}
			t.print(prefix + cGray + cItalic + l)
		}
	}
}

func (t *tui) endMessage() {
	t.flushThink(true)
	t.flushText(true)
	t.inMsg, t.inThink = false, false
}

func (t *tui) userLines(from, text string) []string {
	lab := t.label(from)
	var out []string
	first := true
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for _, wl := range wrap(l, t.width-4) {
			p := cGray + "  "
			if first {
				p = cGray + "> "
				if lab != "" {
					p = cAccent + lab + cReset + cGray + " > "
				}
				first = false
			}
			out = append(out, p+cUser+wl)
		}
	}
	return out
}

func (t *tui) event(e Event) {
	switch e.T {
	case "user":
		t.endMessage()
		t.print("")
		t.print(t.userLines(e.From, e.Text)...)
		if !e.Replay {
			t.running, t.runFrom, t.runStart, t.thinkN, t.tool = true, e.From, time.Now(), 0, ""
		}
	case "queued":
		lab := t.label(e.From)
		if lab == "" {
			lab = "you"
		}
		t.print(cGray + "  ◦ queued (" + lab + "): " + oneLine(e.Text, t.width-20))
	case "dropped":
		t.print(cGray + "  ◦ dropped: " + oneLine(e.Text, t.width-16))
	case "thinking":
		t.thinkN += len(e.Text)
		if t.showThink {
			t.thinkPart += e.Text
			t.flushThink(false)
		}
	case "text":
		if t.inThink {
			t.flushThink(true)
			t.inThink = false
		}
		t.textPart += e.Text
		if e.Replay {
			t.textPart += "\n"
			t.flushText(true)
			t.inMsg = false
			return
		}
		t.flushText(false)
	case "tool":
		t.endMessage()
		t.tool = cBold + displayName(e.Tool) + cReset + cGray + "(" + oneLine(e.Text, t.width-20) + ")" + cReset
		if e.Replay {
			return
		}
	case "result":
		dot := cGreen + "●"
		if !e.OK {
			dot = cRed + "●"
		}
		head := t.tool
		if head == "" {
			head = cBold + displayName(e.Tool) + cReset
		}
		t.print("", dot+cReset+" "+head, cGray+"  ⎿  "+oneLine(e.Text, t.width-8))
		t.tool = ""
		t.thinkN = 0
	case "info":
		t.endMessage()
		t.print(cGray + "  ⎿  " + e.Text)
	case "error":
		t.endMessage()
		t.print(cRed + "  ⎿  " + e.Text)
	case "done":
		t.endMessage()
		t.running, t.tool = false, ""
		t.compacting = false
		if st := e.Stats; st != nil {
			t.model, t.ctx = st.Model, st.Prompt
			if st.Window > 0 {
				t.window = st.Window
			}
			tps := 0.0
			if st.Secs > 0 {
				tps = float64(st.Output) / st.Secs
			}
			t.lastTS = fmt.Sprintf("%.0fs · %.0f tok/s", st.Secs, tps)
		}
	case "compacting":
		t.endMessage()
		t.compacting = true
		if !t.running {
			t.running, t.runStart, t.tool = true, time.Now(), ""
		}
	case "compacted":
		t.compacting = false
		t.lastSummary = e.Text
		if e.Replay {
			t.print("", cGray+"✻ Earlier messages were compacted · ctrl+o shows the summary")
			return
		}
		t.print("", cAccent+"✻ "+cReset+cBold+"Conversation compacted"+cReset+cGray+
			fmt.Sprintf(" (~%s → ~%s tokens · ctrl+o shows the summary)", human(e.Before), human(e.After)))
		t.ctx = e.After
	case "attached":
		t.attached = true
		if e.OK {
			t.running, t.runStart = true, time.Now()
		}
	}
}

func human(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

// draw writes the live area: the spinner, the input box and the status line.
func (t *tui) draw() {
	t.erase()
	if t.picker != nil {
		rows := t.pickerRows()
		for i, r := range rows {
			t.w(r + cReset)
			if i < len(rows)-1 {
				t.w("\n")
			}
		}
		t.liveRows, t.cursorRow = len(rows), len(rows)-1
		return
	}
	var rows []string
	if t.showSummary && t.lastSummary != "" {
		rows = append(rows, t.summaryPanel()...)
	}
	if t.textPart != "" {
		pre := "  "
		if !t.inMsg {
			pre = cUser + "● " + cReset
		}
		ls := wrap(t.md.line(t.textPart), t.width-3)
		for i, l := range ls {
			if i > 0 {
				pre = "  "
			}
			rows = append(rows, pre+l)
		}
	}
	if t.running {
		secs := int(time.Since(t.runStart).Seconds())
		verb := "Thinking"
		if t.compacting {
			verb = "Compacting the conversation"
		} else if t.tool != "" {
			verb = "Running " + t.tool + cAccent
		} else if t.textPart != "" || t.inMsg {
			verb = "Writing"
		}
		detail := fmt.Sprintf("%ds", secs)
		if t.thinkN > 0 && t.tool == "" {
			detail += " · " + human(t.thinkN) + " chars of thinking"
		}
		if lab := t.label(t.runFrom); lab != "" {
			detail += " · for " + lab
		}
		detail += " · esc to interrupt"
		rows = append(rows, "", clipCols(cAccent+spinFrames[t.frame%len(spinFrames)]+" "+verb+"…"+cReset+cGray+" ("+detail+")", t.width-1))
	}
	rows = append(rows, "")

	// input box
	iw := t.width - 7 // "│ > " + text + " │" fills width-1 columns
	if iw < 10 {
		iw = 10
	}
	var vis [][]rune
	line := []rune{}
	curRow, curCol, col := 0, 0, 0
	for i, r := range t.buf {
		if i == t.cur {
			curRow, curCol = len(vis), col
		}
		if r == '\n' {
			vis = append(vis, line)
			line, col = []rune{}, 0
			continue
		}
		if col == iw {
			vis = append(vis, line)
			line, col = []rune{}, 0
			if i == t.cur {
				curRow, curCol = len(vis), 0
			}
		}
		line = append(line, r)
		col++
	}
	if t.cur == len(t.buf) {
		curRow, curCol = len(vis), col
		if col == iw {
			vis = append(vis, line)
			line = []rune{}
			curRow, curCol = len(vis), 0
		}
	}
	vis = append(vis, line)
	top := len(rows)
	rows = append(rows, cFrame+"╭"+strings.Repeat("─", t.width-3)+"╮")
	for i, v := range vis {
		p := "  "
		if i == 0 {
			p = cBold + "> " + cReset
		}
		txt := string(v)
		pad := iw - utf8.RuneCountInString(txt)
		rows = append(rows, cFrame+"│ "+cReset+p+txt+strings.Repeat(" ", max(0, pad))+cFrame+" │")
	}
	rows = append(rows, cFrame+"╰"+strings.Repeat("─", t.width-3)+"╯")

	left := "  " + t.session + " · " + clipLeft(shortPath(t.cwd), max(10, t.width/2-10))
	if t.notice != "" {
		left = "  " + t.notice
	}
	right := ""
	if t.model != "" {
		right = strings.TrimSuffix(t.model, "-8bit")
		right = strings.TrimSuffix(right, "-5bit")
		right += " · ctx " + human(t.ctx)
		if t.window > 0 {
			right += fmt.Sprintf("/%s (%d%%)", human(t.window), t.ctx*100/t.window)
		}
		if t.lastTS != "" {
			right += " · " + t.lastTS
		}
	}
	if !t.attached {
		right = "not connected"
	}
	gap := t.width - 1 - visible(left) - visible(right) - 2
	status := left
	if gap > 0 {
		status += strings.Repeat(" ", gap) + right
	}
	if sg := t.sugs(); len(sg) > 0 {
		for i, c := range sg {
			name, desc := cGray+"  "+c.name, cGray+c.desc
			if i == t.sugSel {
				name, desc = cAccent+"❯ "+c.name, cAccent+c.desc
			}
			pad := strings.Repeat(" ", max(1, 14-utf8.RuneCountInString(c.name)))
			rows = append(rows, clipCols(name+pad+desc, t.width-1))
		}
	} else {
		rows = append(rows, cGray+clipCols(status, t.width-1))
	}

	for i, r := range rows {
		t.w(r + cReset)
		if i < len(rows)-1 {
			t.w("\n")
		}
	}
	t.liveRows = len(rows)
	cr := top + 1 + curRow
	up := len(rows) - 1 - cr
	if up > 0 {
		t.w(fmt.Sprintf("\x1b[%dA", up))
	}
	t.w(fmt.Sprintf("\r\x1b[%dC", 4+curCol))
	t.cursorRow = cr
}

// clipLeft keeps the end of s, which is the useful part of a path.
func clipLeft(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return "…" + string(r[len(r)-w+1:])
}

// summaryPanel is the last compaction summary, cut to fit the screen.
func (t *tui) summaryPanel() []string {
	md := mdState{}
	var body []string
	for _, l := range strings.Split(t.lastSummary, "\n") {
		for _, wl := range wrap(md.line(l), t.width-5) {
			body = append(body, wl)
		}
	}
	room := max(3, termHeight(int(t.out.Fd()))-14)
	more := ""
	if len(body) > room {
		more = fmt.Sprintf("… %d more lines", len(body)-room)
		body = body[:room]
	}
	rows := []string{"", cAccent + "✻ " + cReset + cBold + "Summary" + cReset + cGray + " · ctrl+o hides it"}
	for _, l := range body {
		rows = append(rows, cFrame+"  │ "+cReset+l)
	}
	if more != "" {
		rows = append(rows, cGray+"  "+more)
	}
	return rows
}

func shortPath(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		return "~" + p[len(h):]
	}
	return p
}

func (t *tui) header() {
	w := min(t.width-1, 60)
	in := w - 4
	line := func(s string) string {
		s = clipCols(s, in)
		pad := in - visible(s)
		return cFrame + "│ " + cReset + s + strings.Repeat(" ", max(0, pad)) + cFrame + " │"
	}
	t.print(
		cFrame+"╭"+strings.Repeat("─", w-2)+"╮",
		line(cAccent+"✻"+cReset+" "+cBold+"qh"+cReset+" "+version),
		line(""),
		line(cGray+"session "+t.session),
		line(cGray+"cwd "+clipLeft(shortPath(t.cwd), in-4)),
		line(cGray+"/ for commands · esc stops an answer · ctrl+t thinking"),
		cFrame+"╰"+strings.Repeat("─", w-2)+"╯",
	)
}

// key handling ---------------------------------------------------------------

type uiAction int

const (
	actNone uiAction = iota
	actSubmit
	actCancel
	actQuit
)

func (t *tui) insert(rs ...rune) {
	nb := make([]rune, 0, len(t.buf)+len(rs))
	nb = append(nb, t.buf[:t.cur]...)
	nb = append(nb, rs...)
	nb = append(nb, t.buf[t.cur:]...)
	t.buf = nb
	t.cur += len(rs)
}

func (t *tui) lineBounds() (int, int) {
	s, e := t.cur, t.cur
	for s > 0 && t.buf[s-1] != '\n' {
		s--
	}
	for e < len(t.buf) && t.buf[e] != '\n' {
		e++
	}
	return s, e
}

func (t *tui) histMove(d int) {
	if len(t.hist) == 0 {
		return
	}
	if t.histPos == len(t.hist) {
		t.draft = append([]rune(nil), t.buf...)
	}
	t.histPos = min(len(t.hist), max(0, t.histPos+d))
	if t.histPos == len(t.hist) {
		t.buf = append([]rune(nil), t.draft...)
	} else {
		t.buf = []rune(t.hist[t.histPos])
	}
	t.cur = len(t.buf)
}

// feed takes bytes from the terminal and returns what the user asked for.
func (t *tui) feed(b []byte) uiAction {
	t.pending = append(t.pending, b...)
	act := actNone
	for len(t.pending) > 0 && act == actNone {
		p := t.pending
		if t.pasting {
			if i := bytes.Index(p, []byte("\x1b[201~")); i >= 0 {
				t.insertText(p[:i])
				t.pending = p[i+6:]
				t.pasting = false
				continue
			}
			// keep a possible start of the end marker for the next read
			keep := 0
			for k := 1; k < 6 && k <= len(p); k++ {
				if bytes.HasPrefix([]byte("\x1b[201~"), p[len(p)-k:]) {
					keep = k
				}
			}
			t.insertText(p[:len(p)-keep])
			t.pending = p[len(p)-keep:]
			return actNone
		}
		if p[0] == 0x1b {
			if len(p) == 1 {
				return actNone // wait: a lone Esc or the start of a sequence
			}
			n, a := t.escape(p)
			if n == 0 {
				return actNone // incomplete sequence
			}
			t.pending = p[n:]
			act = a
			continue
		}
		r, sz := utf8.DecodeRune(p)
		if r == utf8.RuneError && sz == 1 && !utf8.FullRune(p) {
			return actNone
		}
		t.pending = p[sz:]
		act = t.key(r)
	}
	return act
}

func (t *tui) insertText(b []byte) {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	t.insert([]rune(s)...)
}

// escape parses an escape sequence at the start of p. It returns 0 when the
// sequence is not complete yet.
func (t *tui) escape(p []byte) (int, uiAction) {
	if p[1] != '[' && p[1] != 'O' {
		return 2, actNone // Alt+key: ignore
	}
	j := 2
	for j < len(p) && (p[j] < 0x40 || p[j] > 0x7e) {
		j++
	}
	if j >= len(p) {
		return 0, actNone
	}
	seq := string(p[2 : j+1])
	if sg := t.sugs(); len(sg) > 0 && (seq == "A" || seq == "B") {
		if seq == "A" {
			t.sugSel = (t.sugSel + len(sg) - 1) % len(sg)
		} else {
			t.sugSel = (t.sugSel + 1) % len(sg)
		}
		return j + 1, actNone
	}
	switch seq {
	case "A":
		s, _ := t.lineBounds()
		if s == 0 {
			t.histMove(-1)
		} else {
			col := t.cur - s
			ps := s - 1
			for ps > 0 && t.buf[ps-1] != '\n' {
				ps--
			}
			t.cur = min(ps+col, s-1)
		}
	case "B":
		_, e := t.lineBounds()
		if e == len(t.buf) {
			t.histMove(1)
		} else {
			s, _ := t.lineBounds()
			col := t.cur - s
			ne := e + 1
			for ne < len(t.buf) && t.buf[ne] != '\n' {
				ne++
			}
			t.cur = min(e+1+col, ne)
		}
	case "C":
		t.cur = min(len(t.buf), t.cur+1)
	case "D":
		t.cur = max(0, t.cur-1)
	case "H", "1~":
		t.cur, _ = t.lineBounds()
	case "F", "4~":
		_, t.cur = t.lineBounds()
	case "3~":
		if t.cur < len(t.buf) {
			t.buf = append(t.buf[:t.cur], t.buf[t.cur+1:]...)
		}
	case "200~":
		t.pasting = true
	}
	return j + 1, actNone
}

func (t *tui) key(r rune) uiAction {
	if r != 3 {
		t.ctrlC = time.Time{}
		if t.notice == "Press Ctrl+C again to quit" {
			t.notice = ""
		}
	}
	if sg := t.sugs(); len(sg) > 0 && (r == '\r' || r == '\t') {
		t.buf = []rune(sg[t.sugSel].name)
		t.cur = len(t.buf)
		if r == '\r' {
			return actSubmit
		}
		return actNone
	}
	switch r {
	case '\r':
		if t.cur > 0 && t.cur == len(t.buf) && t.buf[t.cur-1] == '\\' {
			t.buf = t.buf[:t.cur-1]
			t.cur--
			t.insert('\n')
			return actNone
		}
		return actSubmit
	case '\n':
		t.insert('\n')
	case 0x7f, 0x08:
		if t.cur > 0 {
			t.buf = append(t.buf[:t.cur-1], t.buf[t.cur:]...)
			t.cur--
		}
	case 3:
		if t.running {
			return actCancel
		}
		if len(t.buf) > 0 {
			t.buf, t.cur = nil, 0
			return actNone
		}
		if !t.ctrlC.IsZero() && time.Since(t.ctrlC) < 2*time.Second {
			return actQuit
		}
		t.ctrlC = time.Now()
		t.notice = "Press Ctrl+C again to quit"
	case 4:
		if len(t.buf) == 0 {
			return actQuit
		}
		if t.cur < len(t.buf) {
			t.buf = append(t.buf[:t.cur], t.buf[t.cur+1:]...)
		}
	case 1:
		t.cur, _ = t.lineBounds()
	case 5:
		_, t.cur = t.lineBounds()
	case 0x15:
		s, _ := t.lineBounds()
		t.buf = append(t.buf[:s], t.buf[t.cur:]...)
		t.cur = s
	case 0x0b:
		_, e := t.lineBounds()
		t.buf = append(t.buf[:t.cur], t.buf[e:]...)
	case 0x17:
		s := t.cur
		for s > 0 && t.buf[s-1] == ' ' {
			s--
		}
		for s > 0 && t.buf[s-1] != ' ' && t.buf[s-1] != '\n' {
			s--
		}
		t.buf = append(t.buf[:s], t.buf[t.cur:]...)
		t.cur = s
	case 0x0c:
		t.erase()
		t.w("\x1b[2J\x1b[H")
	case 0x0f:
		if t.lastSummary == "" {
			t.notice = "no compaction summary in this session"
			break
		}
		t.showSummary = !t.showSummary
	case 0x14:
		t.showThink = !t.showThink
		t.notice = "thinking " + map[bool]string{true: "shown", false: "hidden"}[t.showThink]
	case '\t':
		t.insert(' ', ' ')
	default:
		if r >= 0x20 {
			t.insert(r)
		}
	}
	return actNone
}

// session picker ---------------------------------------------------------------

type picker struct {
	rows []SessionInfo
	sel  int
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func (t *tui) pickerRows() []string {
	p := t.picker
	rows := []string{"", cBold + "Resume a session" + cReset}
	if len(p.rows) == 0 {
		return append(rows, cGray+"  no saved sessions", "", cGray+"  esc to go back")
	}
	const show = 10
	start := max(0, min(p.sel-show/2, len(p.rows)-show))
	for i := start; i < min(len(p.rows), start+show); i++ {
		r := p.rows[i]
		mark, style := "  ", ""
		if i == p.sel {
			mark, style = cAccent+"❯ ", cBold
		}
		msg := r.Last
		if msg == "" {
			msg = "(empty)"
		}
		meta := fmt.Sprintf("%s · %d msgs · %s", ago(r.Updated), r.Msgs, clipLeft(shortPath(r.Cwd), 30))
		rows = append(rows, clipCols(mark+style+r.Name+cReset+"  "+msg, t.width-1),
			clipCols("    "+cGray+meta, t.width-1))
	}
	return append(rows, "", cGray+"  ↑↓ select · enter resume · esc cancel")
}

// pickKey handles a key in the picker. It returns the chosen session, or
// "" with done=true when the user leaves the picker.
func (t *tui) pickKey(b []byte) (name string, done bool) {
	p := t.picker
	for len(b) > 0 {
		k := string(b[:1])
		n := 1
		if b[0] == 0x1b && len(b) >= 3 && (b[1] == '[' || b[1] == 'O') {
			k, n = "\x1b["+string(b[2]), 3
		}
		b = b[n:]
		switch k {
		case "\x1b[A", "k":
			p.sel = max(0, p.sel-1)
		case "\x1b[B", "j":
			p.sel = min(len(p.rows)-1, p.sel+1)
		case "\r":
			if len(p.rows) > 0 {
				return p.rows[p.sel].Name, true
			}
			return "", true
		case "\x1b", "q", "\x03":
			return "", true
		}
	}
	return "", false
}

func listSessionsReq() []SessionInfo {
	c, dec, err := send(ClientReq{Op: "list"})
	if err != nil {
		return nil
	}
	defer c.Close()
	var e Event
	if dec.Decode(&e) != nil {
		return nil
	}
	var rows []SessionInfo
	json.Unmarshal([]byte(e.Text), &rows)
	return rows
}

func nonEmpty(rows []SessionInfo) []SessionInfo {
	var out []SessionInfo
	for _, r := range rows {
		if r.Msgs > 0 {
			out = append(out, r)
		}
	}
	return out
}

// newSessionName names a fresh chat session after the time it started.
func newSessionName() string { return time.Now().Format("0102-150405") }

// latestSession is the newest session of cwd, or the newest one at all.
func latestSession(cwd string) string {
	rows := listSessionsReq()
	for _, r := range rows {
		if r.Cwd == cwd && r.Msgs > 0 {
			return r.Name
		}
	}
	for _, r := range rows {
		if r.Msgs > 0 {
			return r.Name
		}
	}
	return newSessionName()
}

// the loop --------------------------------------------------------------------

type genEvent struct {
	gen int
	e   Event
}

// switchTo clears the screen and shows another session.
func (t *tui) switchTo(name string, events chan genEvent) {
	if t.stopAttach != nil {
		close(t.stopAttach)
	}
	t.gen++
	t.stopAttach = make(chan struct{})
	t.session = name
	t.running, t.tool, t.textPart, t.thinkPart, t.inMsg, t.inThink = false, "", "", "", false, false
	t.ctx, t.lastTS, t.attached = 0, "", false
	t.erase()
	t.w("\x1b[2J\x1b[H")
	t.header()
	go attachLoop(name, t.cwd, t.me, t.gen, events, t.stopAttach)
}

func chatTUI(session string, pick bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	fd := int(os.Stdin.Fd())
	restore, err := makeRaw(fd)
	if err != nil {
		return chatPlain(session)
	}
	t := &tui{out: os.Stdout, width: termWidth(int(os.Stdout.Fd())), session: session, cwd: cwd,
		me: fmt.Sprintf("chat:%d", os.Getpid())}
	t.w("\x1b[?2004h") // bracketed paste
	defer func() {
		t.erase()
		t.w("\x1b[?2004l")
		restore()
		fmt.Println()
	}()

	keys := make(chan []byte, 64)
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(b)
			if err != nil {
				close(keys)
				return
			}
			keys <- append([]byte(nil), b[:n]...)
		}
	}()
	events := make(chan genEvent, 1024)
	quitOnPickerEsc := false
	if pick {
		t.picker = &picker{rows: nonEmpty(listSessionsReq())}
		quitOnPickerEsc = true
	} else {
		t.switchTo(session, events)
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	tick := time.NewTicker(120 * time.Millisecond)
	defer tick.Stop()
	escFire := make(chan struct{}, 1)
	cancelRun := func() {
		go simpleQuiet(ClientReq{Op: "cancel", Session: t.session})
		t.notice = "stopping…"
	}

	t.draw()
	for {
		select {
		case b, ok := <-keys:
			if !ok {
				return nil
			}
			if t.picker != nil {
				name, done := t.pickKey(b)
				if done {
					t.picker = nil
					switch {
					case name != "":
						t.switchTo(name, events)
					case quitOnPickerEsc:
						return nil
					}
					quitOnPickerEsc = false
				}
				break
			}
			if t.escTimer != nil {
				t.escTimer.Stop()
				t.escTimer = nil
			}
			switch t.feed(b) {
			case actQuit:
				return nil
			case actCancel:
				cancelRun()
			case actSubmit:
				if t.submit() {
					return nil
				}
			}
			if len(t.pending) == 1 && t.pending[0] == 0x1b {
				t.escTimer = time.AfterFunc(40*time.Millisecond, func() { escFire <- struct{}{} })
			}
		case <-escFire:
			if len(t.pending) == 1 && t.pending[0] == 0x1b {
				t.pending = nil
				switch {
				case len(t.sugs()) > 0:
					t.buf, t.cur = nil, 0
				case t.running:
					cancelRun()
				}
			}
		case ge := <-events:
			if ge.gen != t.gen {
				break // from the session we left
			}
			e := ge.e
			if e.T == "_lost" {
				t.attached = false
				t.running = false
			} else {
				t.event(e)
			}
			if e.T == "done" && strings.HasPrefix(t.notice, "stopping") {
				t.notice = ""
			}
		case <-winch:
			t.erase()
			t.width = termWidth(int(os.Stdout.Fd()))
		case <-tick.C:
			if !t.running {
				continue
			}
			t.frame++
		}
		t.draw()
	}
}

func (t *tui) submit() bool {
	text := strings.TrimSpace(string(t.buf))
	if text == "" {
		return false
	}
	t.buf, t.cur = nil, 0
	if len(t.hist) == 0 || t.hist[len(t.hist)-1] != text {
		t.hist = append(t.hist, text)
	}
	t.histPos = len(t.hist)
	switch {
	case text == "/exit" || text == "/quit":
		return true
	case text == "/help":
		t.print(helpLines()...)
		return false
	case text == "/clear":
		t.erase()
		t.w("\x1b[2J\x1b[H")
		return false
	case text == "/new":
		if t.running {
			t.notice = "a run is active; stop it first (esc)"
			return false
		}
		if err := simpleQuiet(ClientReq{Op: "reset", Session: t.session, Cwd: t.cwd}); err != nil {
			t.print(cRed + "  ⎿  " + err.Error())
			return false
		}
		t.erase()
		t.w("\x1b[2J\x1b[H")
		t.header()
		t.ctx, t.lastTS = 0, ""
		return false
	case text == "/compact" || strings.HasPrefix(text, "/compact "):
		focus := strings.TrimSpace(strings.TrimPrefix(text, "/compact"))
		go simpleQuiet(ClientReq{Op: "compact", Session: t.session, Cwd: t.cwd, Prompt: focus, From: t.me, Detach: true})
		return false
	case text == "/resume":
		if t.running {
			t.notice = "a run is active; stop it first (esc)"
			return false
		}
		t.picker = &picker{rows: nonEmpty(listSessionsReq())}
		return false
	case text == "/sessions":
		c, dec, err := send(ClientReq{Op: "sessions"})
		if err == nil {
			var e Event
			dec.Decode(&e)
			c.Close()
			t.print(strings.Split(cGray+e.Text, "\n")...)
		}
		return false
	case strings.HasPrefix(text, "/think"):
		on := strings.TrimSpace(strings.TrimPrefix(text, "/think")) != "off"
		if err := simpleQuiet(ClientReq{Op: "set", Session: t.session, Cwd: t.cwd, Think: &on}); err != nil {
			t.print(cRed + "  ⎿  " + err.Error())
		} else {
			t.notice = "thinking " + onOff(on) + " for this session"
		}
		return false
	case strings.HasPrefix(text, "/"):
		t.notice = "unknown command; /help lists them"
		return false
	}
	req := ClientReq{Op: "run", Session: t.session, Cwd: t.cwd, Prompt: text, From: t.me, Detach: true}
	go simpleQuiet(req) // the attach stream shows the run and its errors
	return false
}

// simpleQuiet sends a request and waits for done, without printing.
func simpleQuiet(req ClientReq) error {
	c, dec, err := send(req)
	if err != nil {
		return err
	}
	defer c.Close()
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return nil
		}
		switch e.T {
		case "error":
			return fmt.Errorf("%s", e.Text)
		case "done":
			return nil
		}
	}
}

// attachLoop keeps an attach connection open and passes its events on. When
// the daemon goes away it says so and connects again. stop ends it.
func attachLoop(session, cwd, from string, gen int, out chan<- genEvent, stop <-chan struct{}) {
	first := true
	for {
		c, dec, err := send(ClientReq{Op: "attach", Session: session, Cwd: cwd, From: from})
		if err == nil {
			go func() {
				<-stop
				c.Close()
			}()
			skip := !first // after a reconnect, do not print the history again
			for {
				var e Event
				if err := dec.Decode(&e); err != nil {
					break
				}
				if skip && e.Replay {
					continue
				}
				out <- genEvent{gen, e}
			}
			c.Close()
			select {
			case <-stop:
				return
			default:
			}
			out <- genEvent{gen, Event{T: "_lost"}}
		}
		first = false
		select {
		case <-stop:
			return
		case <-time.After(time.Second):
		}
	}
}
