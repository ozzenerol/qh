package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"
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
	{"/web on", "let Qwen search the web and fetch pages"},
	{"/web off", "no web tools (the default)"},
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
		{"ctrl+w alt+⌫", "delete the word before the cursor (ctrl+⌫ too)"},
		{"alt+d", "delete the word after the cursor (ctrl+del too)"},
		{"ctrl+u ctrl+k", "delete to the start / end of the line"},
		{"ctrl+y", "paste what the last delete removed"},
		{"alt+← alt+→", "move by word (ctrl+← → and alt+b alt+f too)"},
		{"ctrl+a ctrl+e", "start / end of the line"},
		{"esc", "stop the answer"},
		{"↑ ↓", "history"},
		{"/", "commands"},
		{"ctrl+t", "show or hide the thinking"},
		{"ctrl+r", "sessions: open, delete, clear finished runs"},
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

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

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
	killed   []rune // the text the last kill removed, for Ctrl+Y
	pasting  bool
	pending  []byte
	escTimer *time.Timer

	// the run that the session shows now
	running      bool
	runFrom      string
	runStart     time.Time
	thinkN       int
	tool         string // tool that runs now, already formatted
	textPart     string
	thinkPart    string
	inMsg        bool
	qwenShown    bool // the "qwen" tag of this turn is printed
	lastKind     string
	thoughtN     int // thinking chars since the last folded line
	thoughtStart time.Time
	toolName     string
	toolArg      string
	toolStart    time.Time
	budget       int // think_budget, for the meter
	inThink      bool
	md           mdState
	tbl          []string // rows of a Markdown table that is still coming
	showThink    bool
	frame        int

	model       string
	ctx         int
	window      int
	logo        string // kitty escape codes of the header logo, built once
	sent        bool   // a prompt went out from this chat
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

	fb      strings.Builder // the frame being built
	lastKey string          // the live area at the last draw

	liveRows  int // rows of the live area at the last draw
	cursorRow int // row of the cursor inside the live area
	attached  bool
}

// w adds to the frame; flush sends the frame to the terminal in one write.
func (t *tui) w(s string) { t.fb.WriteString(s) }

// flush writes the frame as one synchronized update (DEC mode 2026) with the
// cursor hidden, so the terminal never shows a half-drawn screen.
func (t *tui) flush() {
	if t.fb.Len() == 0 {
		return
	}
	t.out.WriteString("\x1b[?2026h\x1b[?25l" + t.fb.String() + "\x1b[?25h\x1b[?2026l")
	t.fb.Reset()
}

// erase moves the cursor to the top of the live area. It does not clear it:
// the next writes overwrite each line and clear what is left, so no blank
// frame appears.
func (t *tui) erase() {
	if t.liveRows > 0 {
		if t.cursorRow > 0 {
			t.w(fmt.Sprintf("\x1b[%dA", t.cursorRow))
		}
		t.w("\r")
	}
	t.liveRows, t.cursorRow, t.lastKey = 0, 0, ""
}

// clearLive removes the live area from the screen.
func (t *tui) clearLive() {
	t.erase()
	t.w("\x1b[J")
}

// print writes lines above the live area.
func (t *tui) print(lines ...string) {
	t.erase()
	for _, l := range lines {
		t.w(l + cReset + "\x1b[K\n")
	}
}

// emitLive draws rows as the live area and puts the cursor at row cr,
// column col. It writes nothing when the area did not change.
func (t *tui) emitLive(rows []string, cr, col int) {
	key := strings.Join(rows, "\n") + fmt.Sprintf("|%d|%d", cr, col)
	if key == t.lastKey {
		return
	}
	t.erase()
	for i, r := range rows {
		t.w(r + cReset + "\x1b[K")
		if i < len(rows)-1 {
			t.w("\n")
		}
	}
	t.w("\x1b[J")
	t.liveRows = len(rows)
	if up := len(rows) - 1 - cr; up > 0 {
		t.w(fmt.Sprintf("\x1b[%dA", up))
	}
	t.w("\r")
	if col > 0 {
		t.w(fmt.Sprintf("\x1b[%dC", col))
	}
	t.cursorRow, t.lastKey = cr, key
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

// The transcript has a gutter of gw columns: a speaker tag on the first line
// of a turn ("you", "qwen", "opus"), spaces after it.
const gw = 6

func tag(name, color string) string {
	return " " + color + cBold + name + cReset + strings.Repeat(" ", max(1, gw-1-utf8.RuneCountInString(name)))
}

// qwenPrefix is the gutter for the next line of the model's turn.
func (t *tui) qwenPrefix() string {
	if !t.qwenShown {
		t.qwenShown = true
		return tag("qwen", cQwen)
	}
	return strings.Repeat(" ", gw)
}

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
	if !t.inMsg {
		if t.qwenShown {
			t.print("")
		}
		t.inMsg = true
		t.md = mdState{}
	}
	t.lastKind = "text"
	if !t.md.fence && isTableRow(line) {
		t.tbl = append(t.tbl, line)
		return
	}
	t.flushTable()
	rendered := t.md.line(line)
	for _, l := range wrap(rendered, t.width-gw-1) {
		t.print(t.qwenPrefix() + l)
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
		t.inThink = true
		for _, l := range wrap(line, t.width-gw-3) {
			t.print(t.qwenPrefix() + cGray + "∴ " + cItalic + l)
		}
	}
}

// endThought prints the folded line of the thinking that just ended, when the
// thinking is hidden.
func (t *tui) endThought() {
	if t.thoughtN > 0 && !t.showThink {
		secs := time.Since(t.thoughtStart).Seconds()
		t.lastKind = "thought"
		t.print(t.qwenPrefix() + cGray + fmt.Sprintf("∴ thought for %.0fs · %s chars", secs, human(t.thoughtN)))
	}
	t.thoughtN = 0
}

// flushTable prints the table rows collected so far as one drawn table.
func (t *tui) flushTable() {
	if len(t.tbl) == 0 {
		return
	}
	rows := renderTable(t.tbl, t.width-gw-1)
	t.tbl = nil
	for _, l := range rows {
		t.print(t.qwenPrefix() + l)
	}
}

func (t *tui) endMessage() {
	t.flushThink(true)
	t.flushText(true)
	t.flushTable()
	t.inMsg, t.inThink = false, false
}

func speaker(lab string) (string, string) {
	if lab == "" {
		return "you", cYou
	}
	return lab, cOpus
}

func (t *tui) userLines(from, text string) []string {
	name, col := speaker(t.label(from))
	var out []string
	first := true
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for _, wl := range wrap(l, t.width-gw-1) {
			p := strings.Repeat(" ", gw)
			if first {
				p = tag(name, col)
				first = false
			}
			out = append(out, p+cUser+wl)
		}
	}
	return out
}

func secs(ms int) string {
	if ms < 100 {
		return "0.0s"
	}
	if ms < 60000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dm%02ds", ms/60000, ms/1000%60)
}

// callLine is one tool call as a single line: name, target, result, time.
func (t *tui) callLine(tool, arg, short string, ok bool, ms int) string {
	name, res := cQwen+padRight(tool, 6)+cReset, cGray
	if !ok {
		name, res = cRed+padRight(tool, 6)+cReset, cRed
	}
	if strings.HasPrefix(short, "+") && ok {
		if i := strings.Index(short, " −"); i > 0 {
			short = cGreen + short[:i] + " " + cRed + short[i+1:] + cReset
		}
	}
	tail := res + padRight(short, 12) + cReset
	if ms > 0 {
		tail += cGray + " " + secs(ms)
	}
	argW := max(10, t.width-1-gw-2-7-20)
	return t.qwenPrefix() + cFrame + "├ " + cReset + name + " " + padRight(clipCols(arg, argW), argW) + cReset + " " + tail
}

// padRight pads s with spaces to w visible columns.
func padRight(s string, w int) string {
	return s + strings.Repeat(" ", max(0, w-visible(s)))
}

func (t *tui) event(e Event) {
	switch e.T {
	case "user":
		t.endMessage()
		t.endThought()
		t.qwenShown, t.lastKind = false, ""
		t.print("")
		t.print(t.userLines(e.From, e.Text)...)
		t.print("")
		if !e.Replay {
			t.running, t.runFrom, t.runStart, t.thinkN, t.tool = true, e.From, time.Now(), 0, ""
		}
	case "queued":
		lab := t.label(e.From)
		if lab == "" {
			lab = "you"
		}
		t.print(cGray + strings.Repeat(" ", gw) + "◦ queued (" + lab + "): " + oneLine(e.Text, t.width-gw-20))
	case "dropped":
		t.print(cGray + strings.Repeat(" ", gw) + "◦ dropped: " + oneLine(e.Text, t.width-gw-16))
	case "thinking":
		if t.thoughtN == 0 {
			t.thoughtStart = time.Now()
		}
		t.thinkN += len(e.Text)
		t.thoughtN += len(e.Text)
		if t.showThink {
			t.thinkPart += e.Text
			t.flushThink(false)
		}
	case "text":
		if t.inThink {
			t.flushThink(true)
			t.inThink = false
		}
		t.endThought()
		t.textPart += e.Text
		if e.Replay {
			t.textPart += "\n"
			t.flushText(true)
			t.inMsg = false
			return
		}
		t.flushText(false)
	case "tool":
		if e.Sub {
			return // shown with its result
		}
		t.endMessage()
		t.endThought()
		if t.lastWasText() {
			t.print("")
		}
		t.toolName, t.toolArg, t.toolStart = e.Tool, oneLine(e.Text, 200), time.Now()
		t.tool = e.Tool
	case "result":
		if e.Sub {
			mark := cGray + "·"
			if !e.OK {
				mark = cRed + "·"
			}
			t.print(strings.Repeat(" ", gw) + cFrame + "│ " + mark + cReset + cGray + " " + e.Tool + " " + oneLine(e.Text, t.width-gw-16) + cReset)
			return
		}
		arg := t.toolArg
		if t.toolName != e.Tool {
			arg = ""
		}
		short := e.Short
		if short == "" {
			short = "ok"
			if !e.OK {
				short = "failed"
			}
		}
		t.print(t.callLine(e.Tool, arg, short, e.OK, e.Ms))
		if !e.OK && e.Text != "" {
			t.print(strings.Repeat(" ", gw) + cFrame + "│  " + cRed + oneLine(e.Text, t.width-gw-5))
		}
		t.tool, t.toolName, t.toolArg = "", "", ""
		t.thinkN = 0
		t.lastKind = "call"
	case "info":
		t.endMessage()
		t.print(cGray + strings.Repeat(" ", gw) + "· " + e.Text)
	case "error":
		t.endMessage()
		t.print(cRed + strings.Repeat(" ", gw) + "· " + e.Text)
	case "done":
		t.endMessage()
		t.endThought()
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
			t.print("", cGray+"  ∴ earlier messages were compacted · ctrl+o shows the summary")
			return
		}
		t.print("", cAccent+"  ∴ "+cReset+cBold+"Conversation compacted"+cReset+cGray+
			fmt.Sprintf(" (~%s → ~%s tokens · ctrl+o shows the summary)", human(e.Before), human(e.After)))
		t.ctx = e.After
	case "attached":
		t.attached = true
		if e.OK {
			t.running, t.runStart = true, time.Now()
		}
	}
}

// lastWasText tells if the line above is answer text, so a tool call after it
// gets a blank line first.
func (t *tui) lastWasText() bool {
	return t.lastKind == "text"
}

func human(n int) string {
	if n >= 10000 {
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

// draw writes the live area: the spinner, the input box and the status line.
func meter(n, of, cells int) string {
	f := 0
	if of > 0 {
		f = min(cells, n*cells/of)
	}
	c := cCode
	if of > 0 && n*10 >= of*8 {
		c = cAccent
	}
	return c + strings.Repeat("▮", f) + cFrame + strings.Repeat("▯", cells-f) + cReset
}

func (t *tui) draw() {
	if t.picker != nil {
		rows := t.pickerRows()
		t.emitLive(rows, len(rows)-1, 0)
		return
	}
	var rows []string
	if t.showSummary && t.lastSummary != "" {
		rows = append(rows, t.summaryPanel()...)
	}
	if len(t.tbl) > 0 {
		for _, l := range renderTable(t.tbl, t.width-gw-1) {
			rows = append(rows, strings.Repeat(" ", gw)+l)
		}
	}
	if t.textPart != "" {
		p := strings.Repeat(" ", gw)
		if !t.qwenShown {
			p = tag("qwen", cQwen)
		}
		for i, l := range wrap(t.md.line(t.textPart), t.width-gw-1) {
			if i > 0 {
				p = strings.Repeat(" ", gw)
			}
			rows = append(rows, p+l)
		}
	}
	if t.running {
		spin := cAccent + spinFrames[t.frame%len(spinFrames)] + cReset
		el := int(time.Since(t.runStart).Seconds())
		var line string
		switch {
		case t.compacting:
			line = spin + " " + cAccent + "compacting the conversation" + cGray + fmt.Sprintf(" %ds", el)
		case t.tool != "":
			ts := int(time.Since(t.toolStart).Seconds())
			line = spin + " " + cQwen + padRight(t.toolName, 6) + cReset + " " + oneLine(t.toolArg, max(10, t.width-gw-40)) + cGray + fmt.Sprintf("  %ds", ts)
		case t.textPart != "" || t.inMsg:
			line = spin + " " + cAccent + "writing" + cGray + fmt.Sprintf(" %ds", el)
		default:
			line = spin + " " + cAccent + "thinking" + cGray + fmt.Sprintf(" %ds", el)
			if t.thinkN > 0 {
				b := t.budget
				if b <= 0 {
					b = 30000
				}
				line += fmt.Sprintf(" · %s of %s ", human(t.thinkN), human(b)) + cAccent + "▕" +
					strings.Repeat("█", min(12, t.thinkN*12/b)) + cFrame + strings.Repeat("░", 12-min(12, t.thinkN*12/b)) + cAccent + "▏" + cGray
			}
		}
		if lab := t.label(t.runFrom); lab != "" {
			line += " · for " + lab
		}
		line += " · esc stops"
		rows = append(rows, "", clipCols(strings.Repeat(" ", gw)+line, t.width-1))
	}
	rows = append(rows, "")

	// input between two rules
	const pw = 3 // " › "
	iw := max(10, t.width-pw-2)
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
	rule := cFrame + strings.Repeat("─", t.width-1)
	top := len(rows)
	rows = append(rows, rule)
	for i, v := range vis {
		p := "   "
		if i == 0 {
			p = " " + cYou + cBold + "›" + cReset + " "
		}
		txt := string(v)
		if i == 0 && len(t.buf) == 0 && t.running {
			if lab := t.label(t.runFrom); lab != "" {
				txt = cGray + clipCols("watching "+lab+" · what you type now is queued for after this run", iw)
			}
		}
		if i == 0 && len(t.buf) == 0 && !t.running {
			txt = cFrame + clipCols("ask qwen, or type / for commands", iw)
		}
		rows = append(rows, p+txt)
	}
	rows = append(rows, rule)

	left := " " + t.session
	if !t.sent && t.ctx == 0 {
		left = " " + cUser + "^R" + cGray + " sessions  " + cUser + "^T" + cGray + " thinking  " + cUser + "^O" + cGray + " summary  " + cUser + "esc" + cGray + " stop"
	}
	if t.notice != "" {
		left = " " + t.notice
	}
	right := cRed + "● " + cGray + "not connected"
	if t.attached {
		right = cGreen + "● " + cGray
		if t.model != "" {
			m := strings.TrimSuffix(strings.TrimSuffix(t.model, "-8bit"), "-5bit")
			if i := strings.LastIndex(m, "/"); i >= 0 {
				m = m[i+1:]
			}
			right += m + " · "
		}
		right += "ctx " + human(t.ctx)
		if t.window > 0 {
			right += "/" + human(t.window) + " " + meter(t.ctx, t.window, 8)
			if t.ctx > 0 {
				right += cGray + fmt.Sprintf(" %d%%", t.ctx*100/t.window)
			}
		}
		if t.lastTS != "" {
			right += " · " + t.lastTS
		}
	}
	// When both halves do not fit on one line, the right half gets its own line.
	status := []string{left}
	if gap := t.width - 1 - visible(left) - visible(right) - 1; gap > 0 {
		status[0] += strings.Repeat(" ", gap) + right
	} else {
		status = append(status, " "+right)
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
		for _, l := range status {
			rows = append(rows, cGray+clipCols(l, t.width-1))
		}
	}

	t.emitLive(rows, top+1+curRow, pw+curCol)
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
	body := mdLines(t.lastSummary, t.width-5)
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
	cfg := loadConfig()
	proj, branch := t.cwd, ""
	if out, err := exec.Command("git", "-C", t.cwd, "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD").Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) == 2 {
			proj, branch = f[0], f[1]
		}
	}
	state := "new"
	if t.ctx > 0 {
		state = "resumed"
	}
	sep := cFrame + " · " + cGray
	if t.width < 70 {
		t.print(" " + cQwen + cBold + "▌qh" + cReset + cGray + " " + version + sep + cUser + clipLeft(shortPath(proj), max(10, t.width-20)))
		return
	}
	server := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(cfg.URL, "http://"), "https://"), "/v1")
	if server == "" {
		server = "127.0.0.1:8080"
	}
	row := func(k, v, extra string) string {
		l := cQwen + "▌" + cReset + "  " + cGray + fmt.Sprintf("%-7s", k) + " " + cUser + v
		if extra != "" {
			l += sep + extra
		}
		return l
	}
	lines := []string{
		cQwen + cBold + "▌qh" + cReset + " " + cGray + version,
		row("server", server, ""),
		row("project", clipLeft(shortPath(proj), max(10, t.width-40)), branch),
		row("session", t.session, state),
	}
	pad := " "
	logo := ""
	if cfg.Logo != "" && kittyOK() {
		if t.logo == "" {
			if fr, d, err := logoFrames(cfg.Logo); err == nil && len(fr) > 0 {
				t.logo = kittyLogo(fr, d)
			}
		}
		if t.logo != "" {
			logo = t.logo
			pad = strings.Repeat(" ", logoCols+3)
			lines = append([]string{""}, lines...)
		}
	}
	for i := range lines {
		if lines[i] != "" || logo == "" {
			lines[i] = pad + lines[i]
		}
	}
	if logo == "" {
		t.print(lines...)
		return
	}
	t.print(lines...)
	t.w(fmt.Sprintf("\x1b[%dA\r\x1b[1C", len(lines)) + logo + fmt.Sprintf("\x1b[%dB\r", len(lines)))
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
		switch p[1] { // Alt+key, as terminals send it with Option/Alt as Meta
		case 0x7f, 0x08:
			t.killBack(t.wordLeft(t.cur))
		case 'b':
			t.cur = t.wordLeft(t.cur)
		case 'f':
			t.cur = t.wordRight(t.cur)
		case 'd':
			t.killFwd(t.wordRight(t.cur))
		}
		return 2, actNone
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
	case "1;5C", "1;3C", "1;9C", "5C":
		t.cur = t.wordRight(t.cur)
	case "1;5D", "1;3D", "1;9D", "5D":
		t.cur = t.wordLeft(t.cur)
	case "3;5~", "3;3~":
		t.killFwd(t.wordRight(t.cur))
	case "1;5H", "1;3H":
		t.cur = 0
	case "1;5F", "1;3F":
		t.cur = len(t.buf)
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

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordLeft is the start of the word before i, skipping spaces and
// punctuation first, like Alt+B in readline.
func (t *tui) wordLeft(i int) int {
	for i > 0 && !isWord(t.buf[i-1]) {
		i--
	}
	for i > 0 && isWord(t.buf[i-1]) {
		i--
	}
	return i
}

// wordRight is the end of the word after i.
func (t *tui) wordRight(i int) int {
	for i < len(t.buf) && !isWord(t.buf[i]) {
		i++
	}
	for i < len(t.buf) && isWord(t.buf[i]) {
		i++
	}
	return i
}

// killBack deletes from s to the cursor and keeps it for Ctrl+Y.
func (t *tui) killBack(s int) {
	if s >= t.cur {
		return
	}
	t.killed = append([]rune(nil), t.buf[s:t.cur]...)
	t.buf = append(t.buf[:s], t.buf[t.cur:]...)
	t.cur = s
}

// killFwd deletes from the cursor to e and keeps it for Ctrl+Y.
func (t *tui) killFwd(e int) {
	if e <= t.cur {
		return
	}
	t.killed = append([]rune(nil), t.buf[t.cur:e]...)
	t.buf = append(t.buf[:t.cur], t.buf[e:]...)
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
	case 0x7f:
		if t.cur > 0 {
			t.buf = append(t.buf[:t.cur-1], t.buf[t.cur:]...)
			t.cur--
		}
	case 0x08: // Ctrl+Backspace in most terminals
		t.killBack(t.wordLeft(t.cur))
	case 2:
		t.cur = max(0, t.cur-1)
	case 6:
		t.cur = min(len(t.buf), t.cur+1)
	case 0x19:
		t.insert(t.killed...)
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
		t.killBack(s)
	case 0x0b:
		_, e := t.lineBounds()
		t.killFwd(e)
	case 0x17:
		s := t.cur
		for s > 0 && t.buf[s-1] == ' ' {
			s--
		}
		for s > 0 && t.buf[s-1] != ' ' && t.buf[s-1] != '\n' {
			s--
		}
		t.killBack(s)
	case 0x0c:
		t.clearLive()
		t.w("\x1b[2J\x1b[H")
	case 0x0f:
		if t.lastSummary == "" {
			t.notice = "no compaction summary in this session"
			break
		}
		t.showSummary = !t.showSummary
	case 0x12:
		if t.running {
			t.notice = "a run is active; stop it first (esc)"
		} else {
			t.picker = newPicker(t.session)
		}
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
	rows    []SessionInfo
	sel     int
	current string // the session of this chat, never deleted from here
	cfg     Config
	note    string
}

func newPicker(current string) *picker {
	return &picker{rows: nonEmpty(listSessionsReq()), current: current, cfg: loadConfig()}
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// byLabel is who started a session, in a word.
func byLabel(by string) string {
	switch {
	case by == "", by == "chat", strings.HasPrefix(by, "chat:"):
		return "chat"
	case by == "qh run":
		return "run"
	}
	return by
}

func (p *picker) state(r SessionInfo) string {
	switch {
	case r.Running && r.Waiting:
		return cGray + "queued"
	case r.Running:
		return cAccent + "RUNNING"
	}
	at := p.cfg.CompactAt
	if at <= 0 {
		at = p.cfg.Context * 8 / 10
	}
	if at > 0 && r.Ctx*10 >= at*9 {
		return cYou + "compacts soon"
	}
	return ""
}

func (t *tui) pickerRows() []string {
	p := t.picker
	w := t.width - 1
	rows := []string{"", clipCols(" "+cBold+"Sessions"+cReset+cGray+"  ↑↓ move · enter open · d delete · x delete finished runs · esc back", w)}
	if len(p.rows) == 0 {
		return append(rows, cGray+"   no saved sessions", "")
	}
	rows = append(rows, cGray+clipCols("   "+padRight("name", 18)+padRight("by", 7)+padRight("turns", 7)+padRight("ctx", 13)+padRight("last", 10)+"state", w))
	window := max(1, p.cfg.Context)
	const show = 10
	start := max(0, min(p.sel-show/2, len(p.rows)-show))
	for i := start; i < min(len(p.rows), start+show); i++ {
		r := p.rows[i]
		mark, style := "   ", ""
		if i == p.sel {
			mark, style = " "+cAccent+"❯"+cReset+" ", cBold
		}
		by := byLabel(r.By)
		byc := cUser
		if by != "chat" {
			byc = cOpus
		}
		name := r.Name
		if name == p.current {
			name += "*"
		}
		ctx := fmt.Sprintf("%-6s %3d%%", human(r.Ctx), r.Ctx*100/window)
		rows = append(rows, clipCols(mark+style+padRight(clipCols(name, 17), 18)+cReset+byc+padRight(by, 7)+cReset+
			padRight(fmt.Sprint(r.Turns), 7)+padRight(ctx, 13)+cGray+padRight(ago(r.Updated), 10)+p.state(r), w))
	}
	if more := len(p.rows) - min(len(p.rows), start+show); more > 0 || start > 0 {
		rows = append(rows, cGray+fmt.Sprintf("   %d of %d shown", min(show, len(p.rows)), len(p.rows)))
	}
	r := p.rows[p.sel]
	rows = append(rows, "", cFrame+" ── "+r.Name+" "+strings.Repeat("─", max(0, w-5-utf8.RuneCountInString(r.Name))))
	if r.First != "" {
		rows = append(rows, clipCols(" "+cGray+"first "+cReset+r.First, w))
	}
	if r.Last != "" && r.Last != r.First {
		rows = append(rows, clipCols(" "+cGray+"last  "+cReset+r.Last, w))
	}
	rows = append(rows, clipCols(" "+cGray+shortPath(r.Cwd), w))
	if p.note != "" {
		rows = append(rows, " "+cGray+p.note)
	}
	return rows
}

func deleteReq(name string) error {
	c, dec, err := send(ClientReq{Op: "delete", Session: name})
	if err != nil {
		return err
	}
	defer c.Close()
	var e Event
	if err := dec.Decode(&e); err != nil {
		return err
	}
	if e.T == "error" {
		return fmt.Errorf("%s", e.Text)
	}
	return nil
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
		case "d":
			if len(p.rows) == 0 {
				break
			}
			r := p.rows[p.sel]
			if r.Name == p.current {
				p.note = "this chat uses " + r.Name
				break
			}
			if err := deleteReq(r.Name); err != nil {
				p.note = err.Error()
				break
			}
			p.note = "deleted " + r.Name
			p.reload()
		case "x":
			n, kept := 0, 0
			for _, r := range p.rows {
				if byLabel(r.By) == "chat" || r.Running || r.Name == p.current {
					continue
				}
				if deleteReq(r.Name) == nil {
					n++
				} else {
					kept++
				}
			}
			p.note = fmt.Sprintf("deleted %s", plural(n, "finished run"))
			if kept > 0 {
				p.note += fmt.Sprintf(", %d in use", kept)
			}
			p.reload()
		case "\x1b", "q", "\x03":
			return "", true
		}
	}
	return "", false
}

func (p *picker) reload() {
	p.rows = nonEmpty(listSessionsReq())
	p.sel = min(p.sel, max(0, len(p.rows)-1))
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
	t.tbl = nil
	t.ctx, t.lastTS, t.attached = 0, "", false
	for _, r := range listSessionsReq() {
		if r.Name == name {
			t.ctx = r.Ctx
		}
	}
	if t.window == 0 {
		t.window = loadConfig().Context
	}
	t.clearLive()
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
		me: fmt.Sprintf("chat:%d", os.Getpid()), budget: loadConfig().ThinkBudget}
	t.w("\x1b[?2004h") // bracketed paste
	defer func() {
		t.clearLive()
		t.w("\x1b[?2004l")
		t.flush()
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
		t.picker = newPicker(t.session)
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
	t.flush()
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
				t.sent = true
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
			t.clearLive()
			t.width = termWidth(int(os.Stdout.Fd()))
		case <-tick.C:
			if !t.running {
				continue
			}
			t.frame++
		}
		t.draw()
		t.flush()
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
		t.clearLive()
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
		t.clearLive()
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
		t.picker = newPicker(t.session)
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
	case strings.HasPrefix(text, "/web"):
		on := strings.TrimSpace(strings.TrimPrefix(text, "/web")) != "off"
		if err := simpleQuiet(ClientReq{Op: "set", Session: t.session, Cwd: t.cwd, Web: &on}); err != nil {
			t.print(cRed + "  ⎿  " + err.Error())
		} else {
			t.notice = "web " + onOff(on) + " for this session"
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
