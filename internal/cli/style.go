package cli

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
)

// styler is the few ANSI attributes a human table uses: bold, faint and
// one accent (basic green), SGR codes every terminal theme maps to
// something legible on light and dark alike. It is on only for a terminal
// with NO_COLOR unset (no-color.org) and TERM not dumb, so a pipe or a
// file never sees an escape code, and nothing it marks is told by its
// colour alone: the text says it too.
type styler struct{ on bool }

type attr int

const (
	plain attr = iota
	bold
	faint
	accent
)

var sgr = map[attr]string{bold: "1", faint: "2", accent: "32"}

// styleFor is w's styler: on for a terminal, per the rule above.
func styleFor(w io.Writer) styler {
	f, ok := w.(*os.File)
	return styler{on: ok && isatty.IsTerminal(f.Fd()) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"}
}

func (s styler) paint(a attr, text string) string {
	if !s.on || a == plain || text == "" {
		return text
	}
	return "\x1b[" + sgr[a] + "m" + text + "\x1b[0m"
}

// cell is one table cell: its text, its attribute, and for a right-aligned
// column the spaces after it that line it up with its neighbours (a price
// beside a "(price)"), kept outside the escape codes.
type cell struct {
	text  string
	a     attr
	trail int
}

// renderTable lays rows out in columns two spaces apart, right-aligned
// where right says. Widths come from the uncoloured text and the padding
// stays outside the escape codes, so the styled table lines up exactly as
// the plain one does; trailing spaces are trimmed.
func renderTable(st styler, right []bool, rows [][]cell) []string {
	var widths []int
	for _, r := range rows {
		for i, c := range r {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(c.text)+c.trail)
		}
	}
	out := make([]string, len(rows))
	for ri, r := range rows {
		var b strings.Builder
		for i, c := range r {
			if i > 0 {
				b.WriteString("  ")
			}
			pad := widths[i] - utf8.RuneCountInString(c.text) - c.trail
			if i < len(right) && right[i] {
				b.WriteString(strings.Repeat(" ", pad))
				b.WriteString(st.paint(c.a, c.text))
				b.WriteString(strings.Repeat(" ", c.trail))
			} else {
				b.WriteString(st.paint(c.a, c.text))
				b.WriteString(strings.Repeat(" ", pad+c.trail))
			}
		}
		out[ri] = strings.TrimRight(b.String(), " ")
	}
	return out
}
