package tui

import (
	"reflect"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/scan"
)

// sample is a fixed finding list: two critical, one warning, one info, already
// in findings.Sort order. Tests assert against it rather than building ad-hoc
// slices so a filter count is readable at the call site.
func sample() []findings.Finding {
	return []findings.Finding{
		{Level: findings.Critical, Kind: "Pod", Namespace: "shop", Name: "crasher", Issue: "CrashLoopBackOff"},
		{Level: findings.Critical, Kind: "Pod", Namespace: "shop", Name: "badimage", Issue: "ImagePullBackOff"},
		{Level: findings.Warning, Kind: "Deployment", Namespace: "shop", Name: "api", Issue: "not fully available"},
		{Level: findings.Info, Kind: "Service", Namespace: "web", Name: "cache", Issue: "no endpoints"},
	}
}

// listModel is a Model sized so listRows() is 18 — big enough that scrolling
// does not interfere with cursor tests.
func listModel() Model {
	return Model{All: sample(), Mode: ModeList, Width: 80, Height: 24}
}

func press(m Model, r rune) Model {
	return Update(m, Event{Kind: EventKey, Key: Key{Kind: KeyRune, Rune: r}})
}

func TestUpdate_CursorMoves(t *testing.T) {
	m := listModel()
	m = Update(m, Event{Kind: EventKey, Key: Key{Kind: KeyDown}})
	if m.Cursor != 1 {
		t.Fatalf("down: cursor = %d, want 1", m.Cursor)
	}
	m = press(m, 'j')
	if m.Cursor != 2 {
		t.Fatalf("j: cursor = %d, want 2", m.Cursor)
	}
	m = press(m, 'k')
	if m.Cursor != 1 {
		t.Fatalf("k: cursor = %d, want 1", m.Cursor)
	}
	m = Update(m, Event{Kind: EventKey, Key: Key{Kind: KeyUp}})
	if m.Cursor != 0 {
		t.Fatalf("up: cursor = %d, want 0", m.Cursor)
	}
}

func TestUpdate_CursorClampsAtBothEnds(t *testing.T) {
	m := listModel()
	for i := 0; i < 10; i++ {
		m = press(m, 'k')
	}
	if m.Cursor != 0 {
		t.Fatalf("past the top: cursor = %d, want 0", m.Cursor)
	}
	for i := 0; i < 10; i++ {
		m = press(m, 'j')
	}
	if m.Cursor != 3 {
		t.Fatalf("past the bottom: cursor = %d, want 3", m.Cursor)
	}
}

func TestUpdate_FirstAndLast(t *testing.T) {
	m := listModel()
	m = press(m, 'G')
	if m.Cursor != 3 {
		t.Fatalf("G: cursor = %d, want 3", m.Cursor)
	}
	m = press(m, 'g')
	if m.Cursor != 0 {
		t.Fatalf("g: cursor = %d, want 0", m.Cursor)
	}
}

func TestUpdate_FiltersSelectLevels(t *testing.T) {
	m := listModel()
	m = press(m, '1')
	if m.Filter != FilterCritical || len(m.visible()) != 2 {
		t.Fatalf("1: filter = %v, visible = %d; want FilterCritical, 2", m.Filter, len(m.visible()))
	}
	m = press(m, '2')
	if m.Filter != FilterWarning || len(m.visible()) != 3 {
		t.Fatalf("2: filter = %v, visible = %d; want FilterWarning, 3", m.Filter, len(m.visible()))
	}
	m = press(m, '0')
	if m.Filter != FilterAll || len(m.visible()) != 4 {
		t.Fatalf("0: filter = %v, visible = %d; want FilterAll, 4", m.Filter, len(m.visible()))
	}
	m = press(m, '1')
	m = press(m, 'a')
	if m.Filter != FilterAll {
		t.Fatalf("a: filter = %v, want FilterAll", m.Filter)
	}
}

// The cursor can be sitting on a row a narrower filter removes. It must be
// pulled back to the last visible row, not left pointing past the end.
func TestUpdate_FilterPullsCursorBack(t *testing.T) {
	m := listModel()
	m = press(m, 'G')
	if m.Cursor != 3 {
		t.Fatalf("setup: cursor = %d, want 3", m.Cursor)
	}
	m = press(m, '1')
	if m.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1 (last of 2 critical)", m.Cursor)
	}
}

func TestUpdate_EmptyListKeepsCursorAtZero(t *testing.T) {
	m := Model{Mode: ModeList, Width: 80, Height: 24}
	m = press(m, 'j')
	m = press(m, 'G')
	if m.Cursor != 0 || m.Top != 0 {
		t.Fatalf("cursor = %d, top = %d; want 0, 0", m.Cursor, m.Top)
	}
}

// Top is the first visible row. It must follow the cursor off both ends of the
// viewport so the selected row is always drawn.
func TestUpdate_TopFollowsCursor(t *testing.T) {
	var all []findings.Finding
	for i := 0; i < 50; i++ {
		all = append(all, findings.Finding{Level: findings.Critical, Kind: "Pod", Name: "p"})
	}
	m := Model{All: all, Mode: ModeList, Width: 80, Height: 24} // listRows() == 18
	for i := 0; i < 20; i++ {
		m = press(m, 'j')
	}
	if m.Cursor != 20 {
		t.Fatalf("cursor = %d, want 20", m.Cursor)
	}
	if m.Top != 3 {
		t.Fatalf("top = %d, want 3 (cursor 20 - 18 rows + 1)", m.Top)
	}
	for i := 0; i < 20; i++ {
		m = press(m, 'k')
	}
	if m.Top != 0 {
		t.Fatalf("scrolling back: top = %d, want 0", m.Top)
	}
}

func TestUpdate_ModeTransitions(t *testing.T) {
	m := listModel()
	m = Update(m, Event{Kind: EventKey, Key: Key{Kind: KeyEnter}})
	if m.Mode != ModeDetail {
		t.Fatalf("enter: mode = %v, want ModeDetail", m.Mode)
	}
	m = Update(m, Event{Kind: EventKey, Key: Key{Kind: KeyEsc}})
	if m.Mode != ModeList {
		t.Fatalf("esc: mode = %v, want ModeList", m.Mode)
	}
	m = press(m, 'l')
	if m.Mode != ModeDetail {
		t.Fatalf("l: mode = %v, want ModeDetail", m.Mode)
	}
	m = press(m, 'h')
	if m.Mode != ModeList {
		t.Fatalf("h: mode = %v, want ModeList", m.Mode)
	}
	m = press(m, 'b')
	if m.Mode != ModeBlind {
		t.Fatalf("b: mode = %v, want ModeBlind", m.Mode)
	}
	m = press(m, 'b')
	if m.Mode != ModeList {
		t.Fatalf("b again: mode = %v, want ModeList", m.Mode)
	}
	m = press(m, '?')
	if m.Mode != ModeHelp {
		t.Fatalf("?: mode = %v, want ModeHelp", m.Mode)
	}
	m = press(m, '?')
	if m.Mode != ModeList {
		t.Fatalf("? again: mode = %v, want ModeList", m.Mode)
	}
}

// Detail needs a row to show. On an empty list enter must do nothing rather
// than open a pane that would index past the end.
func TestUpdate_EnterOnEmptyListStaysInList(t *testing.T) {
	m := Model{Mode: ModeList, Width: 80, Height: 24}
	m = Update(m, Event{Kind: EventKey, Key: Key{Kind: KeyEnter}})
	if m.Mode != ModeList {
		t.Fatalf("mode = %v, want ModeList", m.Mode)
	}
}

func TestUpdate_Quit(t *testing.T) {
	if m := press(listModel(), 'q'); !m.Quit {
		t.Error("q did not set Quit")
	}
	m := Update(listModel(), Event{Kind: EventKey, Key: Key{Kind: KeyCtrlC}})
	if !m.Quit {
		t.Error("ctrl-c did not set Quit")
	}
}

func TestUpdate_UnknownKeyIsIgnored(t *testing.T) {
	m := listModel()
	m = press(m, 'j')
	before := m
	after := Update(m, Event{Kind: EventKey, Key: Key{Kind: KeyUnknown}})
	if !reflect.DeepEqual(after, before) {
		t.Errorf("unknown key changed the model: %+v", after)
	}
	if z := press(m, 'z'); !reflect.DeepEqual(z, before) {
		t.Errorf("unbound rune changed the model: %+v", z)
	}
}

func TestUpdate_ResizeSetsSize(t *testing.T) {
	m := Update(listModel(), Event{Kind: EventResize, Width: 100, Height: 40})
	if m.Width != 100 || m.Height != 40 {
		t.Fatalf("width = %d, height = %d; want 100, 40", m.Width, m.Height)
	}
}

// Shrinking the terminal shrinks the viewport, so Top must be pulled up to keep
// the cursor drawn.
func TestUpdate_ResizePullsTopUp(t *testing.T) {
	var all []findings.Finding
	for i := 0; i < 50; i++ {
		all = append(all, findings.Finding{Level: findings.Critical, Kind: "Pod", Name: "p"})
	}
	m := Model{All: all, Mode: ModeList, Width: 80, Height: 24}
	for i := 0; i < 20; i++ {
		m = press(m, 'j')
	}
	m = Update(m, Event{Kind: EventResize, Width: 80, Height: 16}) // listRows() == 10
	if m.Top != 11 {
		t.Fatalf("top = %d, want 11 (cursor 20 - 10 rows + 1)", m.Top)
	}
}

func TestUpdate_RescanKeySetsScanning(t *testing.T) {
	m := press(listModel(), 'r')
	if !m.Scanning {
		t.Error("r did not set Scanning")
	}
}

func TestUpdate_ScannedReplacesState(t *testing.T) {
	m := listModel()
	m.Scanning = true
	m.Err = "an earlier failure"
	snap := ScanSnapshot{
		Findings:  sample()[:1],
		Blind:     []scan.ReadFailure{{Resource: "pods", Reason: "forbidden"}},
		Health:    clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 3, NodesTotal: 3},
		Workloads: 7,
		Generated: "2026-07-29 11:04:12 UTC",
	}
	m = Update(m, Event{Kind: EventScanned, Result: &snap})
	if m.Scanning {
		t.Error("Scanning still set after the scan finished")
	}
	if m.Err != "" {
		t.Errorf("Err = %q, want cleared", m.Err)
	}
	if len(m.All) != 1 || len(m.Blind) != 1 || m.Workloads != 7 || m.Generated != snap.Generated {
		t.Errorf("state not replaced: %+v", m)
	}
	if m.Health.Verdict != "Healthy" {
		t.Errorf("health = %+v, want Healthy", m.Health)
	}
}

// A failed re-scan must not blank the screen: the findings already on it are
// still the best information the operator has.
func TestUpdate_ScanFailureKeepsPreviousFindings(t *testing.T) {
	m := listModel()
	m.Workloads = 62
	m.Scanning = true
	m = Update(m, Event{Kind: EventScanned, Err: "connection refused"})
	if m.Scanning {
		t.Error("Scanning still set after the scan failed")
	}
	if m.Err != "connection refused" {
		t.Errorf("Err = %q, want %q", m.Err, "connection refused")
	}
	if len(m.All) != 4 || m.Workloads != 62 {
		t.Errorf("findings were lost: %d findings, %d workloads", len(m.All), m.Workloads)
	}
}

// A shorter list arriving under a parked cursor must clamp it, exactly as a
// filter change does.
func TestUpdate_ScannedClampsCursor(t *testing.T) {
	m := listModel()
	m = press(m, 'G')
	snap := ScanSnapshot{Findings: sample()[:1]}
	m = Update(m, Event{Kind: EventScanned, Result: &snap})
	if m.Cursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.Cursor)
	}
}

func TestSnapshot_ProjectsResult(t *testing.T) {
	res := scan.Result{
		Health:       clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		PartialReads: []scan.ReadFailure{{Resource: "poddisruptionbudgets", Reason: "forbidden"}},
		Inventory: inventory.Result{Workloads: []inventory.Workload{
			{Kind: "Deployment", Namespace: "shop", Name: "api"},
			{Kind: "Deployment", Namespace: "shop", Name: "web"},
		}},
	}
	got := Snapshot(res, time.Date(2026, 7, 29, 11, 4, 12, 0, time.UTC))
	if got.Workloads != 2 {
		t.Errorf("workloads = %d, want 2", got.Workloads)
	}
	if got.Health.Verdict != "Degraded" {
		t.Errorf("verdict = %q, want Degraded", got.Health.Verdict)
	}
	if len(got.Blind) != 1 || got.Blind[0].Resource != "poddisruptionbudgets" {
		t.Errorf("blind = %+v", got.Blind)
	}
	if got.Generated != "2026-07-29 11:04:12 UTC" {
		t.Errorf("generated = %q", got.Generated)
	}
}

// The timestamp is rendered in UTC whatever the operator's zone, so two
// screenshots from different machines describe the same instant.
func TestSnapshot_GeneratedIsUTC(t *testing.T) {
	zone := time.FixedZone("UTC+5", 5*60*60)
	got := Snapshot(scan.Result{}, time.Date(2026, 7, 29, 16, 4, 12, 0, zone))
	if got.Generated != "2026-07-29 11:04:12 UTC" {
		t.Errorf("generated = %q, want the UTC rendering", got.Generated)
	}
}

func TestDrainKeys_AppliesEveryCompleteKey(t *testing.T) {
	m := listModel()
	rest, m := drainKeys([]byte("jj"), m, false)
	if len(rest) != 0 {
		t.Fatalf("rest = %q, want empty", rest)
	}
	if m.Cursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.Cursor)
	}
}

// A trailing incomplete escape sequence stays in the buffer for the next read.
func TestDrainKeys_KeepsIncompleteTail(t *testing.T) {
	rest, m := drainKeys(append([]byte("j"), 0x1b), listModel(), false)
	if string(rest) != "\x1b" {
		t.Fatalf("rest = %q, want the lone esc", rest)
	}
	if m.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.Cursor)
	}
}

// Draining must stop the moment a key asks for a scan, so the loop can run it
// before interpreting anything typed behind it.
func TestDrainKeys_StopsOnScanRequest(t *testing.T) {
	rest, m := drainKeys([]byte("rj"), listModel(), false)
	if !m.Scanning {
		t.Fatal("Scanning not set")
	}
	if string(rest) != "j" {
		t.Fatalf("rest = %q, want the unread j", rest)
	}
}

func TestDrainKeys_StopsOnQuit(t *testing.T) {
	rest, m := drainKeys([]byte("qj"), listModel(), false)
	if !m.Quit {
		t.Fatal("Quit not set")
	}
	if string(rest) != "j" {
		t.Fatalf("rest = %q, want the unread j", rest)
	}
}

// bigModel is 20 critical then 20 info findings in findings.Sort order, sized so
// listRows() is 6. Scrolling is the point here, unlike listModel.
func bigModel() Model {
	var all []findings.Finding
	for i := 0; i < 20; i++ {
		all = append(all, findings.Finding{Level: findings.Critical, Kind: "Pod", Namespace: "shop", Name: "crasher", Issue: "CrashLoopBackOff"})
	}
	for i := 0; i < 20; i++ {
		all = append(all, findings.Finding{Level: findings.Info, Kind: "Service", Namespace: "web", Name: "cache", Issue: "no endpoints"})
	}
	return Model{All: all, Mode: ModeList, Width: 80, Height: 12}
}

func TestUpdate_ShrinkRefillsTheViewport(t *testing.T) {
	m := bigModel()
	if rows := m.listRows(); rows != 6 {
		t.Fatalf("setup: listRows = %d, want 6", rows)
	}
	m = press(m, 'G')
	if m.Cursor != 39 || m.Top != 34 {
		t.Fatalf("setup: cursor = %d, top = %d; want 39, 34", m.Cursor, m.Top)
	}
	// Filtering to critical drops the list from 40 to 20. The cursor lands on the
	// last critical, but Top must come up far enough to fill the window again —
	// leaving it where the longer list put it would paint one row and five blanks
	// with 14 findings scrolled off above.
	m = press(m, '1')
	if got := len(m.visible()); got != 20 {
		t.Fatalf("visible = %d, want 20", got)
	}
	if m.Cursor != 19 {
		t.Fatalf("cursor = %d, want 19", m.Cursor)
	}
	if m.Top != 14 {
		t.Fatalf("top = %d, want 14 (a full window ending on the cursor)", m.Top)
	}
}

func TestUpdate_ShortListStartsAtTheTop(t *testing.T) {
	// Fewer findings than rows: there is nothing to scroll, so Top is 0 whatever
	// the cursor does.
	m := listModel()
	m.Height = 12
	m = press(m, 'G')
	if m.Top != 0 {
		t.Fatalf("top = %d, want 0 (4 findings, 6 rows)", m.Top)
	}
}
