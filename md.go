package main

import (
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
	if w < 10 {
		w = 10
	}
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
