package tui

import (
	"fmt"
	"strings"

	"github.com/imantaba/kubeagent/internal/findings"
)

// The frame refuses to draw below this. A findings table needs the header, two
// rules, a column header and a footer before it can show a single row.
const (
	minWidth  = 40
	minHeight = 10
)

// Control sequences. Only these five: the TUI repaints the whole screen every
// frame rather than tracking dirty regions, which costs a few hundred bytes a
// keystroke and removes a whole class of stale-cell bugs.
const (
	escClear    = "\x1b[2J"
	escHome     = "\x1b[H"
	escReset    = "\x1b[0m"
	sgrCritical = "\x1b[31m" // red
	sgrWarning  = "\x1b[33m" // yellow
)

// Column widths for the findings table, excluding the two-column cursor gutter
// and the single space between columns. ISSUE takes what is left.
const (
	colLevel = 8
	colKind  = 12
	colNS    = 11
	colName  = 18
	// fixedCols is the gutter plus the four fixed columns and their separators.
	fixedCols = 2 + colLevel + 1 + colKind + 1 + colNS + 1 + colName + 1
)

// Render returns the complete frame: the exact bytes to write to the terminal.
// It is pure — no terminal, no clock, no cluster — so every screen the TUI can
// show is reachable from a test.
func Render(m Model) string {
	var b strings.Builder
	b.WriteString(escClear)
	b.WriteString(escHome)
	if m.Width < minWidth || m.Height < minHeight {
		fmt.Fprintf(&b, "terminal too small (need %dx%d, have %dx%d)\r\n", minWidth, minHeight, m.Width, m.Height)
		return b.String()
	}
	var lines []string
	switch m.Mode {
	case ModeDetail:
		lines = detailLines(m)
	case ModeBlind:
		lines = blindLines(m)
	case ModeHelp:
		lines = helpLines(m)
	default:
		lines = listLines(m)
	}
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	return b.String()
}

func listLines(m Model) []string {
	lines := headerLines(m)
	lines = append(lines, rule(m.Width))
	issueW := issueWidth(m.Width)
	lines = append(lines, truncate("  "+
		pad("LEVEL", colLevel)+" "+
		pad("KIND", colKind)+" "+
		pad("NAMESPACE", colNS)+" "+
		pad("NAME", colName)+" "+
		"ISSUE", m.Width))

	rows := m.listRows()
	vis := m.visible()
	if len(vis) == 0 {
		msg := "No findings. Every default check passed."
		if m.Filter != FilterAll {
			msg = "No findings at this filter. Press 0 to show all."
		}
		lines = append(lines, "", "  "+truncate(msg, m.Width-2))
		for len(lines) < m.Height-2 {
			lines = append(lines, "")
		}
	} else {
		for i := 0; i < rows; i++ {
			idx := m.Top + i
			if idx >= len(vis) {
				lines = append(lines, "")
				continue
			}
			lines = append(lines, row(m, vis[idx], idx == m.Cursor, issueW))
		}
	}
	lines = append(lines, rule(m.Width))
	return append(lines, footer(m))
}

// row is one finding. The line is assembled at full width and then truncated,
// so a narrow terminal loses the right-hand columns rather than wrapping.
func row(m Model, f findings.Finding, selected bool, issueW int) string {
	gutter := "  "
	if selected {
		gutter = "▸ "
	}
	line := gutter +
		pad(f.Level.String(), colLevel) + " " +
		pad(f.Kind, colKind) + " " +
		pad(f.Namespace, colNS) + " " +
		pad(f.Name, colName) + " " +
		truncate(f.Issue, issueW)
	line = truncate(line, m.Width)
	if c := levelColour(m, f.Level); c != "" {
		return c + line + escReset
	}
	return line
}

func headerLines(m Model) []string {
	first := fmt.Sprintf("kubeagent %s · %s · %d workloads · %s", m.Version, m.Scope, m.Workloads, m.Generated)
	second := fmt.Sprintf("Cluster: %s · %d/%d nodes Ready", m.Health.Verdict, m.Health.NodesReady, m.Health.NodesTotal)
	if n := len(m.Blind); n > 0 {
		// Blind spots are announced in the chrome, not hidden behind a key the
		// operator has to know about: a screen that silently omits what it could
		// not read is the green-when-blind failure the CI gate exists to prevent.
		second += fmt.Sprintf(" · %d blind spots (b)", n)
	}
	return []string{truncate(first, m.Width), truncate(second, m.Width)}
}

func footer(m Model) string {
	if m.Scanning {
		return truncate("scanning…", m.Width)
	}
	if m.Err != "" {
		return truncate("re-scan failed: "+m.Err+"  ·  r retry  q quit", m.Width)
	}
	return truncate(fmt.Sprintf(
		"[1] critical [2] warning+ [0] all (%d)  ↑↓ move  ⏎ detail  b blind  r rescan  ? help  q quit",
		len(m.visible())), m.Width)
}

func detailLines(m Model) []string {
	vis := m.visible()
	lines := headerLines(m)
	lines = append(lines, rule(m.Width))
	if m.Cursor < len(vis) {
		f := vis[m.Cursor]
		lines = append(lines,
			"  "+truncate(strings.ToUpper(f.Level.String())+"  "+f.Kind+"  "+f.Namespace+"/"+f.Name, m.Width-2),
			"",
			"  "+truncate(f.Issue, m.Width-2),
		)
		if f.Owner != "" {
			lines = append(lines, "", "  owner: "+truncate(f.Owner, m.Width-11))
		}
		if f.Reason != "" {
			lines = append(lines, "")
			for _, l := range wrap(f.Reason, m.Width-4) {
				lines = append(lines, "  "+l)
			}
		}
	}
	for len(lines) < m.Height-2 {
		lines = append(lines, "")
	}
	lines = lines[:m.Height-2]
	lines = append(lines, rule(m.Width))
	return append(lines, truncate("esc back  ↑↓ move  q quit", m.Width))
}

func blindLines(m Model) []string {
	lines := headerLines(m)
	lines = append(lines, rule(m.Width))
	if len(m.Blind) == 0 {
		lines = append(lines, "  kubeagent read everything it asked for.")
	} else {
		lines = append(lines, "  kubeagent could not read the following, so the findings are incomplete.", "")
		for _, b := range m.Blind {
			lines = append(lines, "  "+truncate(b.Resource, m.Width-2))
			// Verbatim: the reason is the diagnosis, and this frame is the
			// operator's own screen. Do not classify it the way the forwarded
			// HTML report has to.
			for _, l := range wrap(b.Reason, m.Width-6) {
				lines = append(lines, "    "+l)
			}
			lines = append(lines, "")
		}
	}
	for len(lines) < m.Height-2 {
		lines = append(lines, "")
	}
	lines = lines[:m.Height-2]
	lines = append(lines, rule(m.Width))
	return append(lines, truncate("b or esc back  q quit", m.Width))
}

func helpLines(m Model) []string {
	lines := headerLines(m)
	lines = append(lines, rule(m.Width))
	for _, l := range []string{
		"  ↑ k        up                   1     critical only",
		"  ↓ j        down                 2     warning and above",
		"  g G        first / last         0 a   all findings",
		"  ⏎ → l      detail               b     blind spots",
		"  esc ← h    back                 r     re-scan",
		"  ?          this screen          q     quit",
		"",
		"  This browses exactly what bare 'kubeagent scan' reports. The opt-in",
		"  checks (--security, --certs, --capacity, --drift, --operators, and",
		"  the rest) are not run here — run 'kubeagent scan' for those.",
	} {
		lines = append(lines, truncate(l, m.Width))
	}
	for len(lines) < m.Height-2 {
		lines = append(lines, "")
	}
	lines = lines[:m.Height-2]
	lines = append(lines, rule(m.Width))
	return append(lines, truncate("? or esc back  q quit", m.Width))
}

// issueWidth is what is left for the ISSUE column. It never drops below 8: at
// that point the whole line is truncated to the terminal width anyway, and a
// floor keeps the arithmetic from going negative.
func issueWidth(width int) int {
	if w := width - fixedCols; w > 8 {
		return w
	}
	return 8
}

func rule(width int) string {
	if width < 6 {
		return ""
	}
	return strings.Repeat("─", width-6)
}

func levelColour(m Model, l findings.Level) string {
	if !m.Colour {
		return ""
	}
	switch l {
	case findings.Critical:
		return sgrCritical
	case findings.Warning:
		return sgrWarning
	}
	return ""
}

// truncate cuts s to at most w columns, marking the cut with an ellipsis. It
// counts runes rather than bytes: a namespace with a multi-byte character would
// otherwise blow its column and wrap the line.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// pad truncates s to w and pads it to exactly w columns.
func pad(s string, w int) string {
	s = truncate(s, w)
	if n := w - len([]rune(s)); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

// wrap breaks s into lines of at most w columns, splitting on spaces. A single
// word longer than w — a long image reference, a full API path — is hard-split
// rather than allowed to overflow.
func wrap(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		for len([]rune(word)) > w {
			if line != "" {
				out = append(out, line)
				line = ""
			}
			r := []rune(word)
			out = append(out, string(r[:w]))
			word = string(r[w:])
		}
		switch {
		case line == "":
			line = word
		case len([]rune(line))+1+len([]rune(word)) <= w:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}
