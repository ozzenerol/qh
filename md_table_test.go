package main

import (
	"strings"
	"testing"
)

var m5Table = []string{
	"| Feature | M5 | M5 Pro | M5 Max | M5 Ultra |",
	"|---------|----|--------|--------|----------|",
	"| CPU Cores | 9-10 (4 Super + 6 Efficiency) | 15-18 (5-6 Super + 10-12 Performance) | 18 (6 Super + 12 Performance) | 30-36 (10-12 Super + 20-24 Performance) |",
	"| GPU Cores | 8 or 10 | 16 or 20 | 32 or 40 | 64 or 80 |",
	"| RAM | 12-32 GB | 24-64 GB | 36-128 GB | 96-512 GB |",
	"| Process | TSMC 3nm (N3P) | TSMC 3nm (**Fusion** Architecture) | TSMC 3nm | `N/A` |",
}

func TestRenderTable(t *testing.T) {
	for _, w := range []int{140, 90, 60, 30} {
		out := renderTable(m5Table, w)
		for _, l := range out {
			if visible(l) > w {
				t.Errorf("width %d: line of %d: %q", w, visible(l), stripANSI(l))
			}
		}
		if testing.Verbose() {
			t.Logf("width %d\n%s", w, stripANSI(strings.Join(out, "\n")))
		}
	}
	out := stripANSI(strings.Join(renderTable(m5Table, 140), "\n"))
	if !strings.Contains(out, "│ Feature   │ M5 ") || !strings.Contains(out, "├───") || strings.Contains(out, "---|") {
		t.Errorf("got\n%s", out)
	}
	if got := tableCells(`| a \| b | c |`); len(got) != 2 || got[0] != "a | b" {
		t.Errorf("cells %q", got)
	}
}
