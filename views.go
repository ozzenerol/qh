package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Full-screen views of a session, drawn in the terminal's alternate screen:
// Contract (the brief and what qh checked), Review (the diff), Profiler
// (where the time went) and Sequence (who sent what to whom). F1..F4 open
// them, Esc goes back to the chat.

const (
	eAccent = "\x1b[38;2;255;138;61m"
	eOK     = "\x1b[38;2;92;200;184m"
	eErr    = "\x1b[38;2;255;107;107m"
	eThink  = "\x1b[38;2;199;146;234m"
	eMuted  = "\x1b[38;2;154;157;168m"
	eLine   = "\x1b[38;2;72;76;90m"
)

var viewNames = []string{"chat", "contract", "review", "profiler", "sequence"}

// trace -----------------------------------------------------------------------

type vstep struct {
	n                          int
	end                        time.Time
	prompt, cached, out, think int // think is in chars
	prefill, gen               float64
	text                       string // the answer text of the step
}

type vtool struct {
	start, end       time.Time
	name, arg, short string
	ok, done         bool
	step             int
}

type vnote struct {
	step       int
	file, head string
	text       string
	fail       bool
}

// seqRow is one message of the Sequence view: an arrow between two lanes, or
// a box on one lane when from == to. Lanes: 0 requester, 1 qh, 2 qwen, 3 tools.
type seqRow struct {
	rel      float64 // seconds since the run started, -1 for history
	from, to int
	text     string
	color    string
	both     bool
}

// trace keeps what the views need, built from the events as they come.
type trace struct {
	brief, by string
	runStart  time.Time
	runEnd    time.Time
	steps     []vstep
	tools     []vtool
	checks    []string
	touched   []string
	notes     []vnote
	seq       []seqRow
	text      string // answer text since the last step
	think     int
}

func (tr *trace) rel(now time.Time, replay bool) float64 {
	if replay || tr.runStart.IsZero() {
		return -1
	}
	return now.Sub(tr.runStart).Seconds()
}

func (tr *trace) row(e Event, now time.Time, from, to int, color, text string, both bool) {
	tr.seq = append(tr.seq, seqRow{tr.rel(now, e.Replay), from, to, oneLine(text, 200), color, both})
	if len(tr.seq) > 2000 {
		tr.seq = tr.seq[len(tr.seq)-1500:]
	}
}

func (tr *trace) flushText(e Event, now time.Time) {
	if t := strings.TrimSpace(tr.text); t != "" {
		tr.row(e, now, 2, 1, "", "says: "+t, false)
	}
	tr.text = ""
}

func (tr *trace) add(e Event, now time.Time, by string) {
	switch e.T {
	case "user":
		tr.flushText(e, now)
		if e.From == "qh" || e.From == "compact" {
			tr.row(e, now, 1, 2, eMuted, e.Text, false)
			return
		}
		if !e.Replay {
			*tr = trace{seq: tr.seq, runStart: now}
		}
		tr.brief, tr.by = e.Text, by
		rules, _, _ := briefRules(e.Text)
		lead := briefTitle(e.Text)
		if len(rules) > 0 {
			lead = fmt.Sprintf("%d rules · %s", len(rules), lead)
		}
		tr.row(e, now, 0, 1, eAccent, lead, false)
	case "thinking":
		tr.think += len(e.Text)
	case "text":
		tr.text += e.Text
		if len(tr.text) > 4000 {
			tr.text = tr.text[len(tr.text)-4000:]
		}
	case "step":
		st := e.Stats
		if st == nil {
			return
		}
		s := vstep{n: st.Steps, end: now, prompt: st.Prompt, cached: st.Cached, out: st.Output,
			think: st.Thinking, prefill: st.PrefillSecs, gen: st.GenSecs, text: strings.TrimSpace(tr.text)}
		tr.steps = append(tr.steps, s)
		req := "prompt " + human(st.Prompt)
		if st.Prompt > 0 {
			req += fmt.Sprintf(" · %d%% cached", st.Cached*100/st.Prompt)
		}
		tr.row(e, now, 1, 2, eMuted, req, false)
		if st.Thinking > 0 {
			ts, _ := s.split()
			tr.row(e, now, 2, 2, eThink, fmt.Sprintf("thinking %.0fs · %s chars", ts, human(st.Thinking)), false)
		}
		tr.flushText(e, now)
		tr.think = 0
	case "tool":
		if e.Sub {
			return
		}
		tr.flushText(e, now)
		tr.tools = append(tr.tools, vtool{start: now, name: e.Tool, arg: e.Text, step: len(tr.steps)})
		tr.row(e, now, 2, 1, "", e.Tool+" "+e.Text, false)
		if e.Tool == "write" || e.Tool == "edit" {
			p := strings.TrimSpace(e.Text)
			if !contains(tr.touched, p) {
				tr.touched = append(tr.touched, p)
			}
			if n := len(tr.steps); n > 0 && tr.steps[n-1].text != "" {
				tr.notes = append(tr.notes, vnote{step: n, file: p, text: tr.steps[n-1].text})
				tr.steps[n-1].text = ""
			}
		}
	case "result":
		if e.Sub {
			return
		}
		var arg string
		for i := len(tr.tools) - 1; i >= 0; i-- {
			if v := &tr.tools[i]; !v.done && v.name == e.Tool {
				v.end, v.ok, v.short, v.done = now, e.OK, e.Short, true
				arg = v.arg
				break
			}
		}
		short := e.Short
		if short == "" {
			short = map[bool]string{true: "ok", false: "failed"}[e.OK]
		}
		col := eOK
		if !e.OK {
			col = eErr
		}
		tr.row(e, now, 1, 3, col, e.Tool+" → "+short, true)
		if !e.OK {
			file := ""
			if e.Tool == "write" || e.Tool == "edit" {
				file = strings.TrimSpace(arg)
			} else if m := fileLineRe.FindStringSubmatch(e.Text); m != nil {
				file = m[1]
			}
			n := vnote{step: len(tr.steps), file: file, head: e.Tool + " " + oneLine(arg, 40), text: e.Text, fail: true}
			for i, x := range tr.notes {
				if x.fail && x.head == n.head && x.text == n.text {
					tr.notes = append(tr.notes[:i], tr.notes[i+1:]...) // keep only the newest of the same failure
					break
				}
			}
			tr.notes = append(tr.notes, n)
		}
	case "info":
		tr.flushText(e, now)
		tr.row(e, now, 1, 1, eMuted, e.Text, false)
	case "error":
		tr.flushText(e, now)
		tr.row(e, now, 1, 1, eErr, e.Text, false)
	case "compacted":
		tr.row(e, now, 1, 1, eMuted, "compacted", false)
	case "done":
		tr.flushText(e, now)
		tr.runEnd = now
		text := "report"
		if st := e.Stats; st != nil {
			tr.checks = st.Checks
			text += fmt.Sprintf(" · %d steps · %.0fs", st.Steps, st.Secs)
			if n := len(st.Checks); n > 0 {
				ok := 0
				for _, c := range st.Checks {
					if strings.HasPrefix(c, "qh check passed") {
						ok++
					}
				}
				text += fmt.Sprintf(" · checks %d/%d passed", ok, n)
			}
		}
		tr.row(e, now, 1, 0, eAccent, text, false)
	}
}

var fileLineRe = regexp.MustCompile(`([\w./-]+\.[a-zA-Z]+):(\d+)`)

// split cuts the generation time of a step into thinking and writing, by the
// share of thinking in the output (about 3.5 chars per token).
func (s vstep) split() (think, write float64) {
	if s.out <= 0 {
		return 0, s.gen
	}
	f := min(1, float64(s.think)/3.5/float64(s.out))
	return s.gen * f, s.gen * (1 - f)
}

// brief parsing ---------------------------------------------------------------

var ruleRe = regexp.MustCompile(`^(?:\d+[.)]|[-*•]|R\d+[:.)])\s+(.+)`)

// briefRules finds the rules of a brief: numbered and bulleted lines, the
// "Done when" line and the files of a "Change" line.
func briefRules(b string) (rules []string, done string, scope []string) {
	for _, l := range strings.Split(b, "\n") {
		t := strings.TrimSpace(l)
		low := strings.ToLower(t)
		switch {
		case strings.HasPrefix(low, "done when"):
			done = strings.TrimSpace(strings.TrimLeft(t[len("done when"):], ": "))
		case strings.HasPrefix(low, "change:") || strings.HasPrefix(low, "change only"):
			for _, f := range strings.FieldsFunc(t[6:], func(r rune) bool { return r == ' ' || r == ',' || r == '`' || r == '(' || r == ')' }) {
				f = strings.TrimRight(f, ".;:")
				if strings.ContainsAny(f, "./") && !strings.HasPrefix(f, "only") {
					scope = append(scope, f)
				}
			}
		default:
			if m := ruleRe.FindStringSubmatch(t); m != nil {
				rules = append(rules, m[1])
			}
		}
	}
	return
}

// briefTitle is the first line of the brief, without a "Goal:" label.
func briefTitle(b string) string {
	for _, l := range strings.Split(b, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if i := strings.Index(t, ":"); i > 0 && i < 12 && strings.EqualFold(t[:i], "goal") {
			t = strings.TrimSpace(t[i+1:])
		}
		return t
	}
	return ""
}

// drawing helpers -------------------------------------------------------------

// fit clips or pads s to exactly w visible columns.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return padRight(clipCols(s, w), w) + cReset
}

// joinCols puts columns side by side, gap spaces apart.
func joinCols(cols [][]string, widths []int, gap int) []string {
	n := 0
	for _, c := range cols {
		n = max(n, len(c))
	}
	out := make([]string, n)
	for i := range n {
		var b strings.Builder
		for j, c := range cols {
			if j > 0 {
				b.WriteString(strings.Repeat(" ", gap))
			}
			l := ""
			if i < len(c) {
				l = c[i]
			}
			b.WriteString(fit(l, widths[j]))
		}
		out[i] = b.String()
	}
	return out
}

// card draws lines in a rounded box w columns wide.
func card(lines []string, w int, border string) []string {
	out := []string{border + "╭" + strings.Repeat("─", max(0, w-2)) + "╮" + cReset}
	for _, l := range lines {
		out = append(out, border+"│"+cReset+" "+fit(l, w-4)+" "+border+"│"+cReset)
	}
	return append(out, border+"╰"+strings.Repeat("─", max(0, w-2))+"╯"+cReset)
}

// wrapPlain wraps text without escapes to w columns.
func wrapPlain(s string, w int) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		out = append(out, wrap(l, max(4, w))...)
	}
	return out
}

// canvas is one row of cells, for arrows and lanes.
type canvas struct {
	r []rune
	c []string
}

func newCanvas(w int) *canvas {
	cv := &canvas{make([]rune, w), make([]string, w)}
	for i := range cv.r {
		cv.r[i] = ' '
	}
	return cv
}

func (cv *canvas) set(i int, r rune, color string) {
	if i >= 0 && i < len(cv.r) {
		cv.r[i], cv.c[i] = r, color
	}
}

func (cv *canvas) text(i int, s, color string) {
	for _, r := range s {
		cv.set(i, r, color)
		i++
	}
}

func (cv *canvas) String() string {
	var b strings.Builder
	cur := ""
	for i, r := range cv.r {
		if cv.c[i] != cur {
			b.WriteString(cReset + cv.c[i])
			cur = cv.c[i]
		}
		b.WriteRune(r)
	}
	return b.String() + cReset
}

// the view frame --------------------------------------------------------------

func (t *tui) setView(n int) {
	if n == t.view || n < 0 || n >= len(viewNames) {
		return
	}
	if t.view == 0 {
		t.w("\x1b[?1049h\x1b[2J")
	}
	t.view, t.viewKey, t.vscroll, t.vsel = n, "", 0, -1
	if n != 0 {
		return
	}
	t.w("\x1b[?1049l")
	t.lastKey = ""
	if t.resized {
		t.resized = false
		t.clearLive()
	}
	held := t.held
	t.held = nil
	t.print(held...)
}

func viewByName(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return -1
	}
	for i, n := range viewNames {
		if strings.HasPrefix(n, s) {
			return i
		}
	}
	return -1
}

func (t *tui) drawView() {
	h := termHeight(int(t.out.Fd()))
	w := t.width - 1
	rows := []string{t.viewHeader(w), t.viewTabs(w), eLine + strings.Repeat("─", w)}
	bh := max(3, h-len(rows)-3)
	var body []string
	switch t.view {
	case 1:
		body = t.contractView(w, bh)
	case 2:
		body = t.reviewView(w, bh)
	case 3:
		body = t.profilerView(w, bh)
	case 4:
		body = t.sequenceView(w, bh)
	}
	for len(body) < bh {
		body = append(body, "")
	}
	rows = append(rows, body[:bh]...)
	rows = append(rows, eLine+strings.Repeat("─", w))
	in := strings.ReplaceAll(string(t.buf), "\n", "⏎")
	pre := " " + eAccent + cBold + "›" + cReset + " "
	iw := w - 3
	col := 3 + min(iw-1, utf8.RuneCountInString(string(t.buf[:t.cur])))
	if in == "" {
		rows = append(rows, pre+eLine+fit("message qwen; Esc goes back to the chat", iw))
	} else {
		r := []rune(in)
		if len(r) > iw-1 {
			r = r[len(r)-(iw-1):]
		}
		rows = append(rows, pre+string(r))
	}
	rows = append(rows, eMuted+fit(" "+t.viewKeys(), w))

	key := strings.Join(rows, "\n") + fmt.Sprintf("|%d", col)
	if key == t.viewKey {
		return
	}
	t.viewKey = key
	t.w("\x1b[H")
	for i, r := range rows {
		t.w(clipCols(r, w) + cReset + "\x1b[K")
		if i < len(rows)-1 {
			t.w("\r\n")
		}
	}
	t.w("\x1b[J")
	t.w(fmt.Sprintf("\x1b[%d;%dH", len(rows)-1, col+1))
}

func (t *tui) viewHeader(w int) string {
	left := " " + eAccent + cBold + "qh" + cReset + "  " + t.session + "  " + eMuted + filepath.Base(t.gitRoot())
	if m := t.shortModel(); m != "" {
		left += " · " + m
	}
	var r []string
	if t.ctx > 0 {
		c := "ctx " + human(t.ctx)
		if t.window > 0 {
			c += "/" + human(t.window)
		}
		r = append(r, c)
	}
	if n := len(t.tr.steps); n > 0 {
		s := t.tr.steps[n-1]
		if s.gen > 0 {
			r = append(r, fmt.Sprintf("%.0f tok/s", float64(s.out)/s.gen))
		}
		r = append(r, fmt.Sprintf("step %d", s.n))
	}
	if !t.tr.runStart.IsZero() {
		end := t.tr.runEnd
		if t.running || end.Before(t.tr.runStart) {
			end = time.Now()
		}
		r = append(r, clock(end.Sub(t.tr.runStart)))
	}
	right := eMuted + strings.Join(r, " · ")
	if t.running {
		right = eAccent + spinFrames[t.frame%len(spinFrames)] + " " + right
	}
	gap := w - visible(left) - visible(right)
	if gap < 1 {
		return fit(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

func clock(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}

func (t *tui) shortModel() string {
	m := strings.TrimSuffix(strings.TrimSuffix(t.model, "-8bit"), "-5bit")
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m
}

func (t *tui) viewTabs(w int) string {
	var b strings.Builder
	b.WriteString(" ")
	for i, n := range []string{"Contract", "Review", "Profiler", "Sequence"} {
		lab := fmt.Sprintf("F%d %s", i+1, n)
		if t.view == i+1 {
			b.WriteString(eAccent + cBold + "▸ " + lab + cReset + "   ")
		} else {
			b.WriteString(eMuted + "  " + lab + cReset + "   ")
		}
	}
	b.WriteString(eMuted + "  esc chat")
	return fit(b.String(), w)
}

func (t *tui) viewKeys() string {
	k := "f1..f4 views · esc chat · "
	switch t.view {
	case 1:
		k += "↑↓ scroll"
	case 2:
		k += "←→ hunk · tab file · ↑↓ scroll · ^N note on this hunk"
	case 3:
		k += "←→ step"
	case 4:
		k += "↑↓ scroll · pgup/pgdn"
	}
	if t.running {
		k += " · ^C stops the run"
	}
	return k
}

// viewNav handles a navigation key in a view: up, down, left, right, pgup,
// pgdn, tab, backtab. It returns false when the key is not for the view.
func (t *tui) viewNav(k string) bool {
	switch k {
	case "up":
		t.vscroll++
	case "down":
		t.vscroll = max(0, t.vscroll-1)
	case "pgup":
		t.vscroll += 10
	case "pgdn":
		t.vscroll = max(0, t.vscroll-10)
	case "left", "right":
		if len(t.buf) > 0 {
			return false
		}
		d := map[string]int{"left": -1, "right": 1}[k]
		t.vsel += d
		if t.vsel < -1 {
			t.vsel = -1
		}
		t.vscroll = 0
	case "tab", "backtab":
		if t.view != 2 || len(t.buf) > 0 {
			return false
		}
		d := map[string]int{"tab": 1, "backtab": -1}[k]
		if n := len(t.rv.files); n > 0 {
			t.rvFile = (t.rvFile + d + n) % n
		}
		t.vsel, t.vscroll = 0, 0
	default:
		return false
	}
	return true
}

// Contract --------------------------------------------------------------------

func (t *tui) loadTodo() []TodoItem {
	if time.Since(t.todoAt) < time.Second {
		return t.todo
	}
	t.todoAt = time.Now()
	d, err := qhDir()
	if err != nil {
		return t.todo
	}
	b, err := os.ReadFile(filepath.Join(d, "sessions", t.session+".json"))
	if err != nil {
		return t.todo
	}
	var s struct {
		Todo []TodoItem `json:"todo"`
	}
	if json.Unmarshal(b, &s) == nil {
		t.todo = s.Todo
	}
	return t.todo
}

func (t *tui) contractView(w, h int) []string {
	tr := &t.tr
	if tr.brief == "" {
		return t.welcome(w)
	}
	rules, done, scope := briefRules(tr.brief)
	todo := t.loadTodo()
	var nd, ng, np int
	for _, it := range todo {
		switch it.Status {
		case "done":
			nd++
		case "doing":
			ng++
		default:
			np++
		}
	}
	by := tr.by
	if by == "" {
		by = "you"
	}
	title := " " + cBold + briefTitle(tr.brief) + cReset
	sub := eMuted + fmt.Sprintf("brief from %s · %d rules", by, len(rules))
	if len(scope) > 0 {
		sub += " · scope " + fmt.Sprint(len(scope)) + " files"
	}
	counts := ""
	if len(todo) > 0 {
		counts = eOK + fmt.Sprintf("%d done", nd) + eMuted + " · " + eAccent + fmt.Sprintf("%d doing", ng) + eMuted + " · " + fmt.Sprintf("%d pending", np)
	}
	out := []string{"", fit(title, w-visible(counts)-1) + counts, " " + fit(sub, w-1)}
	bar := ""
	if len(todo) > 0 {
		bw := w - 2
		a, b := nd*bw/len(todo), ng*bw/len(todo)
		bar = eOK + strings.Repeat("━", a) + eAccent + strings.Repeat("━", b) + eLine + strings.Repeat("━", bw-a-b)
	} else {
		bar = eLine + strings.Repeat("━", w-2)
	}
	out = append(out, " "+bar, "")

	cw := (w - 2 - 4) / 3
	in := cw - 4
	var c1, c2, c3 []string

	c1 = append(c1, eMuted+fmt.Sprintf("BRIEF · %d RULES", len(rules)))
	for i, r := range rules {
		lines := wrapPlain(r, in-4)
		lines[0] = eMuted + fmt.Sprintf("R%-3d", i+1) + cReset + lines[0]
		for j := 1; j < len(lines); j++ {
			lines[j] = "    " + lines[j]
		}
		c1 = append(c1, card(lines, cw, eLine)...)
	}
	if len(rules) == 0 {
		for _, l := range wrapPlain(tr.brief, cw) {
			c1 = append(c1, eMuted+l)
		}
	}

	c2 = append(c2, eMuted+"QWEN'S PLAN")
	if len(todo) == 0 {
		c2 = append(c2, eLine+"no todo list yet")
	}
	for _, it := range todo {
		mark, col := "○", eMuted
		switch it.Status {
		case "done":
			mark, col = "✓", eOK
		case "doing":
			mark, col = "◐", eAccent
		}
		lines := wrapPlain(it.Text, in-2)
		lines[0] = col + mark + cReset + " " + lines[0]
		for j := 1; j < len(lines); j++ {
			lines[j] = "  " + lines[j]
		}
		border := eLine
		if it.Status == "doing" {
			border = eAccent
		}
		c2 = append(c2, card(lines, cw, border)...)
	}

	c3 = append(c3, eOK+"VERIFIED BY QH")
	var chk []string
	if done != "" {
		chk = append(chk, eMuted+"Done when: "+cReset+done)
	}
	for _, c := range tr.checks {
		mark := eMuted + "·"
		switch {
		case strings.HasPrefix(c, "qh check passed"):
			mark = eOK + "✓"
		case strings.HasPrefix(c, "qh check FAILED"):
			mark = eErr + "✗"
		}
		c = strings.TrimPrefix(c, "qh check ")
		for j, l := range wrapPlain(c, in-2) {
			if j == 0 {
				chk = append(chk, mark+cReset+" "+l)
			} else {
				chk = append(chk, "  "+l)
			}
		}
	}
	if len(tr.checks) == 0 {
		msg := "checks run after the report"
		if done == "" {
			msg = "the brief has no Done when line"
		}
		for _, l := range wrapPlain(msg, in) {
			chk = append(chk, eMuted+l)
		}
	}
	c3 = append(c3, card(chk, cw, eLine)...)
	fails := 0
	for i := len(tr.notes) - 1; i >= 0 && fails < 3; i-- {
		n := tr.notes[i]
		if !n.fail {
			continue
		}
		fails++
		lines := []string{eErr + fmt.Sprintf("✗ step %d · %s", n.step, n.head)}
		for j, l := range wrapPlain(n.text, in) {
			if j >= 2 {
				break
			}
			lines = append(lines, eMuted+l)
		}
		c3 = append(c3, card(lines, cw, eLine)...)
	}
	sc := []string{eMuted + "SCOPE"}
	if len(scope) > 0 {
		sc = append(sc, eMuted+"allowed: "+cReset+strings.Join(scope, ", "))
	}
	var outside []string
	for _, p := range tr.touched {
		rel := t.relPath(p)
		if len(scope) == 0 || inScope(rel, scope) {
			sc = append(sc, eOK+"✓ "+cReset+rel)
		} else {
			outside = append(outside, rel)
		}
	}
	for _, p := range outside {
		sc = append(sc, eErr+"✗ "+cReset+p+eErr+" outside the brief")
	}
	if len(tr.touched) == 0 {
		sc = append(sc, eMuted+"no file written yet")
	}
	c3 = append(c3, card(sc, cw, eLine)...)

	cols := joinCols([][]string{c1, c2, c3}, []int{cw, cw, cw}, 2)
	for i := range cols {
		cols[i] = " " + cols[i]
	}
	top := min(t.vscroll, max(0, len(cols)-(h-len(out))))
	t.vscroll = top
	return append(out, cols[top:]...)
}

// relPath makes a tool path relative to the git root of the chat.
func (t *tui) relPath(p string) string {
	if !filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	root := t.gitRoot()
	if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func (t *tui) gitRoot() string {
	if t.root == "" {
		t.root = t.cwd
		if b, err := exec.Command("git", "-C", t.cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
			t.root = strings.TrimSpace(string(b))
		}
	}
	return t.root
}

// Review ----------------------------------------------------------------------

type rvHunk struct {
	head  string
	lines []string // diff lines with their ' ', '+' or '-'
	start int      // first line number in the new file
}

type rvFile struct {
	path     string
	add, del int
	hunks    []rvHunk
	isNew    bool
}

type review struct {
	at    time.Time
	files []rvFile
	err   string
}

func (t *tui) loadReview() {
	if time.Since(t.rv.at) < 2*time.Second {
		return
	}
	t.rv.at = time.Now()
	root := t.gitRoot()
	b, err := exec.Command("git", "-C", root, "diff", "--no-color", "--no-ext-diff", "-U3").Output()
	if err != nil {
		t.rv.files, t.rv.err = nil, "not a git repository: "+root
		return
	}
	t.rv.err = ""
	files := parseDiff(string(b))
	if u, err := exec.Command("git", "-C", root, "ls-files", "--others", "--exclude-standard").Output(); err == nil {
		for _, p := range strings.Fields(string(u)) {
			data, err := os.ReadFile(filepath.Join(root, p))
			if err != nil || len(data) > 200000 || !utf8.Valid(data) {
				continue
			}
			ls := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			h := rvHunk{head: "new file", start: 1}
			for _, l := range ls {
				h.lines = append(h.lines, "+"+l)
			}
			files = append(files, rvFile{path: p, add: len(ls), hunks: []rvHunk{h}, isNew: true})
		}
	}
	t.rv.files = files
	if t.rvFile >= len(files) {
		t.rvFile = 0
	}
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@ ?(.*)`)

func parseDiff(d string) []rvFile {
	var files []rvFile
	var f *rvFile
	var h *rvHunk
	for _, l := range strings.Split(d, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			parts := strings.Fields(l)
			files = append(files, rvFile{path: strings.TrimPrefix(parts[len(parts)-1], "b/")})
			f, h = &files[len(files)-1], nil
		case f == nil:
		case strings.HasPrefix(l, "@@"):
			m := hunkRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			var start int
			fmt.Sscan(m[1], &start)
			f.hunks = append(f.hunks, rvHunk{head: m[2], start: start})
			h = &f.hunks[len(f.hunks)-1]
		case h == nil:
			if strings.HasPrefix(l, "new file") {
				f.isNew = true
			}
		case strings.HasPrefix(l, "+"):
			f.add++
			h.lines = append(h.lines, l)
		case strings.HasPrefix(l, "-"):
			f.del++
			h.lines = append(h.lines, l)
		case strings.HasPrefix(l, " "):
			h.lines = append(h.lines, l)
		}
	}
	return files
}

func (t *tui) reviewView(w, h int) []string {
	t.loadReview()
	files := t.rv.files
	var add, del int
	for _, f := range files {
		add += f.add
		del += f.del
	}
	top := " " + cBold + "working tree" + cReset + "  " + fmt.Sprintf("%d files ", len(files)) + eOK + fmt.Sprintf("+%d ", add) + eErr + fmt.Sprintf("−%d", del) + cReset
	if n := len(t.tr.checks); n > 0 {
		top += eMuted + "  │  "
		for _, c := range t.tr.checks {
			if strings.HasPrefix(c, "qh check passed") {
				top += eOK + "✓ "
			} else if strings.HasPrefix(c, "qh check FAILED") {
				top += eErr + "✗ "
			}
		}
	}
	if t.running {
		top += eAccent + "  │  ◐ " + t.liveState()
	}
	out := []string{fit(top, w), ""}
	if t.rv.err != "" {
		return append(out, eMuted+"  "+t.rv.err)
	}
	if len(files) == 0 {
		return append(out, eMuted+"  no changes in the working tree")
	}
	f := files[t.rvFile]
	if t.vsel < 0 {
		t.vsel = 0
	}
	if t.vsel >= len(f.hunks) {
		// past the last hunk: on to the next file
		if t.rvFile < len(files)-1 {
			t.rvFile++
			f, t.vsel = files[t.rvFile], 0
		} else {
			t.vsel = len(f.hunks) - 1
		}
	}
	if t.vsel < 0 {
		t.vsel = 0
	}

	lw := min(28, w/4)
	var left []string
	left = append(left, eMuted+"FILES")
	for i, x := range files {
		st := fmt.Sprintf("+%d −%d", x.add, x.del)
		if x.isNew {
			st = fmt.Sprintf("new +%d", x.add)
		}
		name := clipLeft(x.path, lw-len(st)-3)
		if i == t.rvFile {
			left = append(left, eAccent+"▸ "+name+strings.Repeat(" ", max(1, lw-2-visible(name)-len(st)))+st)
		} else {
			left = append(left, "  "+name+strings.Repeat(" ", max(1, lw-2-visible(name)-len(st)))+eMuted+st)
		}
	}
	left = append(left, "", eMuted+"HUNKS")
	for i, hk := range f.hunks {
		l := fmt.Sprintf("@@ %-4d %s", hk.start, hk.head)
		if i == t.vsel {
			left = append(left, eAccent+"● "+l)
		} else {
			left = append(left, eMuted+"  "+l)
		}
	}

	mw := w - lw - 3
	notes := t.fileNotes(f.path)
	nw := 0
	if mw > 90 && len(notes) > 0 {
		nw = min(40, mw/3)
		mw -= nw + 2
	}
	var mid []string
	hk := f.hunks[min(t.vsel, len(f.hunks)-1)]
	mid = append(mid, eMuted+f.path+" · @@ +"+fmt.Sprint(hk.start)+" @@ "+hk.head, "")
	// the selected hunk and the ones after it, scrolled by vscroll lines
	var body []string
	for hi := t.vsel; hi < len(f.hunks); hi++ {
		x := f.hunks[hi]
		if hi > t.vsel {
			body = append(body, eLine+"@@ "+fmt.Sprint(x.start)+" "+x.head)
		}
		n := x.start
		for _, l := range x.lines {
			code := strings.ReplaceAll(l[1:], "\t", "    ")
			switch l[0] {
			case '+':
				body = append(body, eOK+fmt.Sprintf("%5d + ", n)+code)
				n++
			case '-':
				body = append(body, eErr+"      − "+code)
			default:
				body = append(body, eLine+fmt.Sprintf("%5d   ", n)+cReset+code)
				n++
			}
		}
	}
	t.vscroll = min(t.vscroll, max(0, len(body)-1))
	mid = append(mid, body[t.vscroll:]...)

	cols := [][]string{left, mid}
	widths := []int{lw, mw}
	if nw > 0 {
		var right []string
		right = append(right, eMuted+"NOTES")
		for _, n := range notes {
			var lines []string
			border := eLine
			if n.fail {
				border = eErr
				lines = append(lines, eErr+fmt.Sprintf("qh · step %d · %s failed", n.step, strings.Fields(n.head + " x")[0]))
			} else {
				lines = append(lines, eAccent+fmt.Sprintf("qwen · step %d · why", n.step))
			}
			for j, l := range wrapPlain(n.text, nw-4) {
				if j >= 6 {
					break
				}
				lines = append(lines, l)
			}
			right = append(right, card(lines, nw, border)...)
		}
		cols, widths = append(cols, right), append(widths, nw)
	}
	rows := joinCols(cols, widths, 2)
	for i := range rows {
		rows[i] = " " + rows[i]
	}
	return append(out, rows...)
}

// fileNotes are the failures and the model's reasons that concern a file,
// newest first, plus failures that name no file.
func (t *tui) fileNotes(path string) []vnote {
	var out []vnote
	for i := len(t.tr.notes) - 1; i >= 0 && len(out) < 4; i-- {
		n := t.tr.notes[i]
		if n.file == "" && n.fail || n.file != "" && (t.relPath(n.file) == path || strings.HasSuffix(path, "/"+n.file)) {
			out = append(out, n)
		}
	}
	return out
}

// reviewNote puts "path:line: " for the selected hunk into the input and goes
// back to the chat, so the user can say what is wrong there.
func (t *tui) reviewNote() {
	files := t.rv.files
	if t.view != 2 || len(files) == 0 {
		return
	}
	f := files[t.rvFile]
	if len(f.hunks) == 0 {
		return
	}
	hk := f.hunks[min(max(0, t.vsel), len(f.hunks)-1)]
	line := hk.start
	for _, l := range hk.lines {
		if l[0] == '+' || l[0] == '-' {
			break
		}
		line++
	}
	t.buf = []rune(fmt.Sprintf("%s:%d: ", f.path, line))
	t.cur = len(t.buf)
	t.setView(0)
}

func (t *tui) liveState() string {
	switch {
	case t.compacting:
		return "compacting"
	case t.tool != "":
		return fmt.Sprintf("%s %s · %ds", t.toolName, oneLine(t.toolArg, 40), int(time.Since(t.toolStart).Seconds()))
	case t.textPart != "" || t.inMsg:
		return "writing"
	}
	return "thinking"
}

// Profiler --------------------------------------------------------------------

func (t *tui) profilerView(w, h int) []string {
	tr := &t.tr
	if len(tr.steps) == 0 && len(tr.tools) == 0 {
		return []string{"", eMuted + "  no run of this chat yet: the profiler shows the runs that start while it is open"}
	}
	var read, think, write, tools float64
	for _, s := range tr.steps {
		th, wr := s.split()
		read += s.prefill
		think += th
		write += wr
	}
	for _, x := range tr.tools {
		end := x.end
		if !x.done {
			end = time.Now()
		}
		tools += end.Sub(x.start).Seconds()
	}
	total := max(0.001, read+think+write+tools)
	cell := func(lab, col string, v float64) []string {
		return []string{eMuted + lab, col + cBold + fmt.Sprintf("%.1f s", v) + cReset + eMuted + fmt.Sprintf("  %.0f%%", v*100/total)}
	}
	cw := (w - 2) / 4
	out := []string{""}
	for _, r := range joinCols([][]string{cell("reading prompts", "", read), cell("thinking", eThink, think), cell("writing", eAccent, write), cell("tools", eOK, tools)}, []int{cw, cw, cw, cw}, 0) {
		out = append(out, " "+r)
	}
	out = append(out, "")

	// timeline
	t0 := tr.runStart
	t1 := tr.runEnd
	if t.running || t1.Before(t0) {
		t1 = time.Now()
	}
	span := max(1, t1.Sub(t0).Seconds())
	lab := 9
	tw := w - lab - 2
	at := func(tm time.Time) int { return int(tm.Sub(t0).Seconds() / span * float64(tw)) }
	fill := func(cv *canvas, a, b time.Time, r rune, col string) {
		x0, x1 := at(a), at(b)
		if x1 <= x0 {
			x1 = x0 + 1
		}
		for x := x0; x < x1; x++ {
			cv.set(x, r, col)
		}
	}
	axis := newCanvas(tw)
	for i := 0; i <= 4; i++ {
		x := i * (tw - 6) / 4
		f := "%.0fs"
		if span < 10 {
			f = "%.1fs"
		}
		axis.text(x, fmt.Sprintf(f, span*float64(x)/float64(tw)), eMuted)
	}
	lanes := map[string]*canvas{}
	names := []string{"steps", "read", "think", "write", "tools"}
	for _, n := range names {
		cv := newCanvas(tw)
		for x := range tw {
			cv.set(x, '·', eLine)
		}
		lanes[n] = cv
	}
	sel := t.selStep()
	for i, s := range tr.steps {
		th, _ := s.split()
		gs := s.end.Add(-time.Duration(s.gen * float64(time.Second)))
		ps := gs.Add(-time.Duration(s.prefill * float64(time.Second)))
		te := gs.Add(time.Duration(th * float64(time.Second)))
		fill(lanes["read"], ps, gs, '█', eMuted)
		fill(lanes["think"], gs, te, '█', eThink)
		fill(lanes["write"], te, s.end, '█', eAccent)
		col := eMuted
		if i == sel {
			col = eAccent + cBold
		}
		lanes["steps"].set(at(ps), '│', eLine)
		lanes["steps"].text(at(ps)+1, fmt.Sprint(s.n), col)
	}
	for _, x := range tr.tools {
		end := x.end
		col := eOK
		if !x.done {
			end = time.Now()
		} else if !x.ok {
			col = eErr
		}
		fill(lanes["tools"], x.start, end, '█', col)
	}
	out = append(out, strings.Repeat(" ", lab+1)+axis.String())
	for _, n := range names {
		out = append(out, " "+eMuted+fit(n, lab)+cReset+lanes[n].String())
	}
	out = append(out, "")

	// charts and the selected step
	dw := min(38, w/3)
	gw2 := (w - dw - 6) / 2
	ch := max(3, min(8, h-len(out)-4))
	var tps, ctx []float64
	for _, s := range tr.steps {
		v := 0.0
		if s.gen > 0 {
			v = float64(s.out) / s.gen
		}
		tps = append(tps, v)
		ctx = append(ctx, float64(s.prompt))
	}
	first, last := func(xs []float64) float64 { return xs[0] }, func(xs []float64) float64 { return xs[len(xs)-1] }
	var g1, g2 []string
	if len(tps) > 0 {
		g1 = chart(fmt.Sprintf("tok/s · now %.0f · first %.0f", last(tps), first(tps)), tps, gw2, ch, eAccent, sel)
		cached := 0
		if s := tr.steps[len(tr.steps)-1]; s.prompt > 0 {
			cached = s.cached * 100 / s.prompt
		}
		g2 = chart(fmt.Sprintf("context · %s · %d%% cached", human(int(last(ctx))), cached), ctx, gw2, ch, eOK, sel)
	}
	var d []string
	if sel >= 0 && sel < len(tr.steps) {
		s := tr.steps[sel]
		th, wr := s.split()
		kv := func(k, v string) string { return eMuted + fit(k, 9) + cReset + v }
		d = append(d, eAccent+fmt.Sprintf("step %d", s.n))
		d = append(d, kv("prompt", fmt.Sprintf("%s, %s cached", human(s.prompt), human(s.cached))))
		d = append(d, kv("read", fmt.Sprintf("%.1f s", s.prefill)))
		d = append(d, kv("think", fmt.Sprintf("%s chars · %.1f s", human(s.think), th)))
		d = append(d, kv("write", fmt.Sprintf("%d tok · %.1f s", s.out, wr)))
		if s.gen > 0 {
			d = append(d, kv("speed", fmt.Sprintf("%.0f tok/s", float64(s.out)/s.gen)))
		}
		for _, x := range tr.tools {
			if x.step != sel+1 {
				continue
			}
			mark, ms := eOK+"✓", ""
			if !x.done {
				mark = eAccent + "◐"
			} else {
				if !x.ok {
					mark = eErr + "✗"
				}
				ms = fmt.Sprintf(" %.1fs", x.end.Sub(x.start).Seconds())
			}
			d = append(d, mark+cReset+" "+x.name+" "+eMuted+oneLine(x.arg, 60)+ms)
		}
	}
	box := card(d, dw, eAccent)
	rows := joinCols([][]string{g1, g2, box}, []int{gw2, gw2, dw}, 2)
	for i := range rows {
		rows[i] = " " + rows[i]
	}
	return append(out, rows...)
}

// selStep is the step the arrows picked; -1 (the default) means the last.
func (t *tui) selStep() int {
	n := len(t.tr.steps)
	if t.vsel < 0 || t.vsel >= n {
		if t.vsel >= n {
			t.vsel = -1
		}
		return n - 1
	}
	return t.vsel
}

var bars = []rune(" ▁▂▃▄▅▆▇█")

// chart draws a column chart of vs, h rows high, inside a card w wide.
func chart(title string, vs []float64, w, h int, col string, sel int) []string {
	iw := w - 4
	top := 0.0
	for _, v := range vs {
		top = max(top, v)
	}
	if top <= 0 {
		top = 1
	}
	step := max(1, min(3, iw/max(1, len(vs))))
	from := 0
	if len(vs)*step > iw {
		from = len(vs) - iw/step
	}
	lines := []string{eMuted + title}
	for r := h - 1; r >= 0; r-- {
		cv := newCanvas(iw)
		for i := from; i < len(vs); i++ {
			eighths := int(vs[i] / top * float64(h*8))
			e := min(8, max(0, eighths-r*8))
			c := col
			if i == sel {
				c = cBold + col
			}
			x := (i - from) * step
			for k := 0; k < max(1, step-1); k++ {
				cv.set(x+k, bars[e], c)
			}
		}
		lines = append(lines, cv.String())
	}
	lines = append(lines, eMuted+fmt.Sprintf("steps %d..%d · max %s", from+1, len(vs), human(int(top))))
	return card(lines, w, eLine)
}

// Sequence --------------------------------------------------------------------

func (t *tui) sequenceView(w, h int) []string {
	tr := &t.tr
	tl := 8
	lw := (w - tl) / 4
	center := func(l int) int { return tl + lw*l + lw/2 }
	req := strings.ToUpper(tr.by)
	if req == "" {
		req = "YOU"
	}
	head := newCanvas(w)
	head.text(0, "time", eMuted)
	for i, n := range []string{req, "QH", "QWEN", "TOOLS"} {
		col := ""
		if i == 1 {
			col = eAccent + cBold
		}
		s := "[ " + n + " ]"
		head.text(center(i)-utf8.RuneCountInString(s)/2, s, col)
	}
	out := []string{head.String()}

	rows := tr.seq
	var live []seqRow
	if t.running && t.tool != "" {
		live = append(live, seqRow{rel: tr.rel(time.Now(), false), from: 1, to: 3, color: eAccent,
			text: fmt.Sprintf("%s · running %ds", t.toolName, int(time.Since(t.toolStart).Seconds()))})
	} else if t.running {
		live = append(live, seqRow{rel: tr.rel(time.Now(), false), from: 2, to: 2, color: eAccent, text: t.liveState() + "…"})
	}
	all := append(append([]seqRow(nil), rows...), live...)
	room := h - 1
	t.vscroll = min(t.vscroll, max(0, len(all)-room))
	end := len(all) - t.vscroll
	start := max(0, end-room)
	for i, r := range all[start:end] {
		cv := newCanvas(w)
		for l := range 4 {
			cv.set(center(l), '│', eLine)
		}
		ts := "·"
		if r.rel >= 0 {
			ts = fmt.Sprintf("%.1fs", r.rel)
		}
		cv.text(0, ts, eMuted)
		dashed := start+i >= len(rows)
		col := r.color
		if r.from == r.to {
			c := center(r.from)
			s := "▏" + r.text + "▕"
			n := utf8.RuneCountInString(s)
			maxw := lw*2 - 2
			if n > maxw {
				s = string([]rune(s)[:maxw-2]) + "…▕"
				n = maxw
			}
			x := min(max(tl, c-n/2), w-n)
			cv.text(x, s, col)
		} else {
			a, b := center(r.from), center(r.to)
			lo, hi := min(a, b), max(a, b)
			line := '─'
			if dashed {
				line = '╌'
			}
			for x := lo; x <= hi; x++ {
				cv.set(x, line, col)
			}
			if b > a || r.both {
				cv.set(hi, '▶', col)
			}
			if b < a || r.both {
				cv.set(lo, '◀', col)
			}
			s := " " + r.text + " "
			room := hi - lo - 3
			if r.from == 0 || r.to == 0 {
				room = hi - lo - 3
			}
			if utf8.RuneCountInString(s) > room {
				s = string([]rune(s)[:max(0, room-2)]) + "… "
			}
			cv.text(lo+2, s, cReset+col)
			if col == "" {
				cv.text(lo+2, s, "")
			}
		}
		out = append(out, cv.String())
	}
	return out
}

// welcome is the Contract view before the first brief.
func (t *tui) welcome(w int) []string {
	cfg := loadConfig()
	srv := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(cfg.URL, "http://"), "https://"), "/v1")
	cw := min(64, w-4)
	kv := func(k, v string) string { return eMuted + fit(k, 10) + cReset + clipLeft(v, cw-15) }
	info := []string{
		eAccent + cBold + "qh " + version + cReset,
		kv("server", srv),
		kv("project", shortPath(t.gitRoot())),
		kv("session", t.session),
	}
	if m := t.shortModel(); m != "" {
		info = append(info, kv("model", m))
	}
	help := []string{
		eMuted + "WRITE A BRIEF",
		"Type the task below. qh reads it like a contract:",
		"",
		eAccent + "1. 2. - *" + cReset + "    numbered and bulleted lines become rules",
		eAccent + "Done when:" + cReset + "   the commands qh runs to check the work",
		eAccent + "Change:" + cReset + "      the files qwen may touch",
		"",
		eMuted + "End a line with \\ to start a new one, or paste the brief.",
		eMuted + "F2 review · F3 profiler · F4 sequence",
		eMuted + "esc plain chat; config \"view\": \"chat\" opens there",
	}
	out := []string{""}
	for _, l := range card(info, cw, eLine) {
		out = append(out, "  "+l)
	}
	out = append(out, "")
	for _, l := range card(help, cw, eAccent) {
		out = append(out, "  "+l)
	}
	return out
}
