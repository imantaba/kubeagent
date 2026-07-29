package tui

import (
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/scan"
)

// Mode is which screen is showing. The TUI has four and no nesting: every mode
// returns to ModeList, so there is no navigation stack to get wrong.
type Mode int

const (
	ModeList   Mode = iota // the findings table
	ModeDetail             // one finding, full text
	ModeBlind              // what kubeagent could not read
	ModeHelp               // the key map
)

// Filter is the severity floor the list shows.
type Filter int

const (
	FilterAll      Filter = iota // everything
	FilterWarning                // warning and above
	FilterCritical               // critical only
)

// Model is the entire TUI state. Update is the only thing that changes it, and
// Update is a pure function, so every screen the TUI can show is reachable in a
// test by replaying a list of events.
type Model struct {
	Version   string
	Scope     string // "all namespaces" or "namespace shop"
	Generated string // "2026-07-29 11:04:12 UTC" — formatted by the caller, never time.Now here
	Health    clusterhealth.ClusterHealth
	Workloads int

	// All is every finding in findings.Sort order. It is never re-sorted here:
	// a second definition of that order would be free to drift from the one
	// findings.Flatten already applies.
	All   []findings.Finding
	Blind []scan.ReadFailure

	Mode     Mode
	Filter   Filter
	Cursor   int // index into the filtered list
	Top      int // first visible row, for scrolling
	Width    int
	Height   int
	Colour   bool   // false under NO_COLOR; the golden frame renders with this false
	Scanning bool   // true while a re-scan is in flight; renders the busy frame
	Err      string // a failed re-scan's message, shown in the footer; cleared by the next success
	Quit     bool
}

// visible is the filtered list. It is derived on demand rather than stored,
// because a stored copy would need invalidating everywhere Filter or All change
// and one missed spot is a stale screen.
func (m Model) visible() []findings.Finding {
	if m.Filter == FilterAll {
		return m.All
	}
	floor := findings.Warning
	if m.Filter == FilterCritical {
		floor = findings.Critical
	}
	out := make([]findings.Finding, 0, len(m.All))
	for _, f := range m.All {
		if f.Level >= floor {
			out = append(out, f)
		}
	}
	return out
}

// listRows is how many finding rows fit: the height less the two header lines,
// the two horizontal rules, the column header and the footer.
func (m Model) listRows() int {
	return m.Height - 6
}

// EventKind is what happened. Everything that can change the model arrives as
// one of these three, which is what keeps Update total and testable.
type EventKind int

const (
	EventKey     EventKind = iota // a decoded key press
	EventResize                   // SIGWINCH: Width and Height are set
	EventScanned                  // a re-scan finished: Result is set, or Err is
)

// ScanSnapshot is the part of a scan.Result the TUI keeps. Run projects one of
// these and hands it to Update; Update never sees a scan.Result or a client.
type ScanSnapshot struct {
	Findings  []findings.Finding
	Blind     []scan.ReadFailure
	Health    clusterhealth.ClusterHealth
	Workloads int
	Generated string
}

// Event is one input to Update. Which fields are meaningful depends on Kind.
type Event struct {
	Kind   EventKind
	Key    Key           // Kind == EventKey
	Width  int           // Kind == EventResize
	Height int           // Kind == EventResize
	Result *ScanSnapshot // Kind == EventScanned, on success
	Err    string        // Kind == EventScanned, on failure
}

// Snapshot projects a scan result into what the TUI shows. now is passed in
// rather than read here so the golden frame is reproducible.
func Snapshot(res scan.Result, now time.Time) ScanSnapshot {
	return ScanSnapshot{
		Findings:  findings.Flatten(res),
		Blind:     res.PartialReads,
		Health:    res.Health,
		Workloads: len(res.Inventory.Workloads),
		Generated: now.UTC().Format("2006-01-02 15:04:05 UTC"),
	}
}

// Update applies one event. It performs no I/O: a key that needs a cluster call
// sets Scanning and the input loop does the call, which is what keeps every
// state transition reachable from a unit test.
func Update(m Model, e Event) Model {
	switch e.Kind {
	case EventResize:
		m.Width, m.Height = e.Width, e.Height
		return clamp(m)
	case EventScanned:
		m.Scanning = false
		if e.Result == nil {
			// Keep All, Blind, Health and Workloads: what is already on screen
			// is still the best information the operator has.
			m.Err = e.Err
			return m
		}
		m.Err = ""
		m.All = e.Result.Findings
		m.Blind = e.Result.Blind
		m.Health = e.Result.Health
		m.Workloads = e.Result.Workloads
		m.Generated = e.Result.Generated
		return clamp(m)
	case EventKey:
		return updateKey(m, e.Key)
	}
	return m
}

func updateKey(m Model, k Key) Model {
	switch k.Kind {
	case KeyCtrlC:
		m.Quit = true
		return m
	case KeyUp:
		return move(m, -1)
	case KeyDown:
		return move(m, 1)
	case KeyEnter, KeyRight:
		return openDetail(m)
	case KeyEsc, KeyLeft:
		m.Mode = ModeList
		return m
	case KeyRune:
		return updateRune(m, k.Rune)
	}
	return m
}

func updateRune(m Model, r rune) Model {
	switch r {
	case 'q':
		m.Quit = true
	case 'j':
		return move(m, 1)
	case 'k':
		return move(m, -1)
	case 'l':
		return openDetail(m)
	case 'h':
		m.Mode = ModeList
	case 'g':
		m.Cursor = 0
		return clamp(m)
	case 'G':
		m.Cursor = len(m.visible()) - 1
		return clamp(m)
	case '1':
		m.Filter = FilterCritical
		return clamp(m)
	case '2':
		m.Filter = FilterWarning
		return clamp(m)
	case '0', 'a':
		m.Filter = FilterAll
		return clamp(m)
	case 'b':
		m.Mode = toggle(m.Mode, ModeBlind)
	case '?':
		m.Mode = toggle(m.Mode, ModeHelp)
	case 'r':
		// The loop sees this and performs the scan. Update stays pure.
		m.Scanning = true
	}
	return m
}

// toggle makes b its own on/off key: pressing it in that mode returns to the
// list rather than requiring esc.
func toggle(current, want Mode) Mode {
	if current == want {
		return ModeList
	}
	return want
}

func move(m Model, delta int) Model {
	m.Cursor += delta
	return clamp(m)
}

// openDetail refuses on an empty list: a detail pane with nothing to show would
// index past the end of the slice.
func openDetail(m Model) Model {
	if m.Mode == ModeList && len(m.visible()) > 0 {
		m.Mode = ModeDetail
	}
	return m
}

// clamp is the single place cursor and scroll bounds are enforced. Every path
// that can change the list length, the cursor or the viewport ends here, so a
// filter that shrinks the list under the cursor, a shorter re-scan result and a
// smaller terminal are all handled by the same three rules.
func clamp(m Model) Model {
	n := len(m.visible())
	if n == 0 {
		m.Cursor, m.Top = 0, 0
		// Leave the detail pane too. It shows visible()[Cursor], so with nothing
		// visible it draws an empty box between the rules — no finding, no message,
		// no hint that the filter is what emptied it. openDetail already refuses to
		// enter on an empty list; this is the same rule applied to a list that
		// empties while the pane is open, which takes two keystrokes: open a detail
		// pane, then press 1 with no critical findings, or re-scan a cluster the
		// operator has just fixed. ModeBlind and ModeHelp read no finding, so they
		// stay put.
		if m.Mode == ModeDetail {
			m.Mode = ModeList
		}
		return m
	}
	if m.Cursor >= n {
		m.Cursor = n - 1
	}
	if m.Cursor < 0 {
		m.Cursor = 0
	}
	rows := m.listRows()
	if rows < 1 {
		rows = 1
	}
	if m.Cursor < m.Top {
		m.Top = m.Cursor
	}
	if m.Cursor >= m.Top+rows {
		m.Top = m.Cursor - rows + 1
	}
	// Pull Top back up when the list is now too short to fill the window from
	// there. The two clauses above only ever chase the cursor, so a filter or a
	// re-scan that shrinks the list under a scrolled view would leave Top where
	// the longer list put it and paint a mostly blank window with findings
	// scrolled off above it. Top <= Cursor holds coming in, so Top > n-rows
	// implies Cursor > n-rows: the cursor stays inside the window this moves to.
	if m.Top > n-rows {
		m.Top = n - rows
	}
	if m.Top < 0 {
		m.Top = 0
	}
	return m
}

// drainKeys applies every complete key in pending and returns whatever bytes are
// left. It stops early on quit and on a scan request so the loop can act before
// interpreting anything typed behind them — a key pressed before the screen
// changed should not be applied to the screen after.
//
// It lives here rather than in tui.go because it is pure, and pure code is
// where the off-by-one lives.
func drainKeys(pending []byte, m Model, final bool) ([]byte, Model) {
	for len(pending) > 0 {
		k, n := decodeKey(pending, final)
		if n == 0 {
			break
		}
		pending = pending[n:]
		m = Update(m, Event{Kind: EventKey, Key: k})
		if m.Quit || m.Scanning {
			break
		}
	}
	return pending, m
}
