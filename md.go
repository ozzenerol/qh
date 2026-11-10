package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[2m"
	cItalic = "\x1b[3m"
	cAccent = "\x1b[38;5;173m" // the orange of the spinner and labels
	cGray   = "\x1b[38;5;245m"
	cFrame  = "\x1b[38;5;240m"
	cGreen  = "\x1b[38;5;114m"
	cRed    = "\x1b[38;5;167m"
	cCode   = "\x1b[38;5;110m"
	cUser   = "\x1b[38;5;252m"
	cYou    = "\x1b[38;5;179m" // speaker tags in the chat
	cQwen   = "\x1b[38;5;73m"
	cOpus   = "\x1b[38;5;176m"
)

// inline renders **bold**, `code` and *italic* in one line of Markdown.
func inline(s, base string) string {
	var b strings.Builder
	for len(s) > 0 {
		switch {
		case strings.HasPrefix(s, "**"):
			if end := strings.Index(s[2:], "**"); end > 0 {
				b.WriteString(cBold + s[2:2+end] + cReset + base)
				s = s[4+end:]
				continue
			}
		case s[0] == '`':
			if end := strings.IndexByte(s[1:], '`'); end > 0 {
				b.WriteString(cCode + s[1:1+end] + cReset + base)
				s = s[2+end:]
				continue
			}
		case s[0] == '*' && len(s) > 1 && s[1] != ' ':
			if end := strings.IndexByte(s[1:], '*'); end > 0 {
				b.WriteString(cItalic + s[1:1+end] + cReset + base)
				s = s[2+end:]
				continue
			}
		}
		_, n := utf8.DecodeRuneInString(s)
		b.WriteString(s[:n])
		s = s[n:]
	}
	return base + b.String() + cReset
}

// mdState renders Markdown line by line; it remembers if a code block is open.
type mdState struct {
	fence bool
}

func (m *mdState) line(l string) string {
	t := strings.TrimSpace(l)
	if strings.HasPrefix(t, "```") {
		m.fence = !m.fence
		return cFrame + l + cReset
	}
	if m.fence {
		return cCode + l + cReset
	}
	switch {
	case strings.HasPrefix(t, "#"):
		h := strings.TrimSpace(strings.TrimLeft(t, "#"))
		return cBold + inline(h, cBold) + cReset
	case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
		ind := l[:len(l)-len(strings.TrimLeft(l, " "))]
		return ind + "• " + inline(t[2:], "")
	case t == "---" || t == "***":
		return cFrame + strings.Repeat("─", 20) + cReset
	}
	return inline(l, "")
}

// visible is the number of terminal columns of s without escape sequences.
// Wide characters count as one column.
func visible(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
			}
			i = j + 1
			continue
		}
		_, sz := utf8.DecodeRuneInString(s[i:])
		i += sz
		n++
	}
	return n
}

// wrap breaks s (which may hold escape sequences) into lines of at most w
// columns, at spaces when it can. The style that is open at a break is
// closed at the end of the line and opened again on the next one.
func wrap(s string, w int) []string {
	return wrapAt(s, max(w, 10))
}

func wrapAt(s string, w int) []string {
	if visible(s) <= w {
		return []string{s}
	}
	var lines []string
	var cur strings.Builder
	col, lastSpace := 0, -1 // lastSpace: byte offset in cur after the space
	style := ""
	spaceStyle := ""
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
			}
			seq := s[i : j+1]
			if seq == cReset {
				style = ""
			} else {
				style += seq
			}
			cur.WriteString(seq)
			i = j + 1
			continue
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		if col == w {
			text := cur.String()
			if lastSpace > 0 && lastSpace < len(text) {
				lines = append(lines, text[:lastSpace]+cReset)
				rest := text[lastSpace:]
				cur.Reset()
				cur.WriteString(spaceStyle + rest)
				col = visible(rest)
			} else {
				lines = append(lines, text+cReset)
				cur.Reset()
				cur.WriteString(style)
				col = 0
			}
			lastSpace = -1
		}
		cur.WriteRune(r)
		col++
		if r == ' ' {
			lastSpace = cur.Len()
			spaceStyle = style
		}
		i += sz
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

func clipCols(s string, w int) string {
	if visible(s) <= w {
		return s
	}
	ls := wrap(s, w-1)
	return ls[0] + cReset + "…"
}

// isTableRow says if a line of Markdown is a row of a pipe table.
func isTableRow(l string) bool {
	t := strings.TrimSpace(l)
	return len(t) > 1 && t[0] == '|' && strings.Count(t, "|") >= 2
}

func tableCells(l string) []string {
	t := strings.TrimSpace(l)
	t = strings.TrimSuffix(strings.TrimPrefix(t, "|"), "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(t); i++ {
		switch {
		case t[i] == '\\' && i+1 < len(t) && t[i+1] == '|':
			cur.WriteByte('|')
			i++
		case t[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(t[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

func isSeparator(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, ":- ") != "" || !strings.Contains(c, "-") {
			return false
		}
	}
	return len(cells) > 0
}

// renderTable draws the rows of a Markdown pipe table with box lines, at most
// w columns wide. Long cells wrap inside their column; the header is bold.
func renderTable(lines []string, w int) []string {
	var rows [][]string
	var right []bool
	header := false
	n := 0
	for i, l := range lines {
		c := tableCells(l)
		if isSeparator(c) {
			if i == 1 {
				header = true
				right = make([]bool, len(c))
				for j, x := range c {
					right[j] = strings.HasSuffix(x, ":") && !strings.HasPrefix(x, ":")
				}
			}
			continue
		}
		rows = append(rows, c)
		n = max(n, len(c))
	}
	if n == 0 {
		return nil
	}
	cells := make([][]string, len(rows))
	nat := make([]int, n)
	for i, r := range rows {
		cells[i] = make([]string, n)
		for j := 0; j < n; j++ {
			if j < len(r) {
				base := ""
				if header && i == 0 {
					base = cBold
				}
				cells[i][j] = inline(r[j], base)
			}
			nat[j] = max(nat[j], visible(cells[i][j]))
		}
	}
	cw := append([]int(nil), nat...)
	low := make([]int, n) // a column never gets narrower than its longest word
	for j := range cw {
		cw[j] = max(cw[j], 1)
		for i := range cells {
			for _, word := range strings.Fields(stripANSI(cells[i][j])) {
				low[j] = max(low[j], min(utf8.RuneCountInString(word), 16))
			}
		}
		low[j] = max(1, min(low[j], cw[j]))
	}
	room := w - 3*n - 1
	for sum(cw) > room {
		big := -1
		for j := range cw {
			if cw[j] > low[j] && (big < 0 || cw[j] > cw[big]) {
				big = j
			}
		}
		if big < 0 {
			break
		}
		cw[big]--
	}
	if sum(cw) > room {
		return records(cells, header, w)
	}
	rule := func(l, m, r string) string {
		var b strings.Builder
		b.WriteString(cFrame + l)
		for j, x := range cw {
			if j > 0 {
				b.WriteString(m)
			}
			b.WriteString(strings.Repeat("─", x+2))
		}
		return b.String() + r + cReset
	}
	out := []string{rule("┌", "┬", "┐")}
	for i, r := range cells {
		parts := make([][]string, n)
		h := 1
		for j, c := range r {
			parts[j] = wrapAt(c, cw[j])
			h = max(h, len(parts[j]))
		}
		for k := 0; k < h; k++ {
			var b strings.Builder
			b.WriteString(cFrame + "│" + cReset)
			for j := range r {
				s := ""
				if k < len(parts[j]) {
					s = strings.TrimRight(parts[j][k], " ")
				}
				pad := strings.Repeat(" ", max(0, cw[j]-visible(s)))
				if j < len(right) && right[j] {
					s = pad + s
				} else {
					s += pad
				}
				b.WriteString(" " + s + cReset + " " + cFrame + "│" + cReset)
			}
			out = append(out, b.String())
		}
		if header && i == 0 && len(cells) > 1 {
			out = append(out, rule("├", "┼", "┤"))
		}
	}
	return append(out, rule("└", "┴", "┘"))
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// mdLines renders a whole Markdown text for a width, tables included.
func mdLines(text string, w int) []string {
	m := mdState{}
	var out, tbl []string
	flush := func() {
		if len(tbl) > 0 {
			out = append(out, renderTable(tbl, w)...)
			tbl = nil
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if !m.fence && isTableRow(l) {
			tbl = append(tbl, l)
			continue
		}
		flush()
		out = append(out, wrap(m.line(l), w)...)
	}
	flush()
	return out
}

// records shows a table that cannot fit as one block per row: the first
// cell as a bold title, then "Header: value" lines.
func records(cells [][]string, header bool, w int) []string {
	var out []string
	start := 0
	if header {
		start = 1
	}
	for i := start; i < len(cells); i++ {
		if len(out) > 0 {
			out = append(out, "")
		}
		for j, c := range cells[i] {
			if j == 0 {
				out = append(out, wrap(cBold+stripANSI(c)+cReset, w)...)
				continue
			}
			name := fmt.Sprintf("%d", j+1)
			if header {
				name = stripANSI(cells[0][j])
			}
			out = append(out, wrap("  "+cGray+name+":"+cReset+" "+c, w)...)
		}
	}
	return out
}
