package tui

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/scan"
)

var update = flag.Bool("update", false, "rewrite golden files")

const goldenPath = "testdata/golden-frame.txt"

// visibleEscapes rewrites the escape byte so the golden file diffs like text.
// A frame is mostly ANSI control sequences; a golden file full of raw 0x1b is
// unreviewable in a pull request.
func visibleEscapes(s string) string {
	return strings.ReplaceAll(s, "\x1b", "␛")
}

// goldenModel is the fixed state the golden frame renders. Colour is off: the
// golden file is about layout, and a coloured frame is checked separately.
func goldenModel() Model {
	return Model{
		Version:   "v0.67.0",
		Scope:     "all namespaces",
		Generated: "2026-07-29 11:04:12 UTC",
		Health: clusterhealth.ClusterHealth{
			Verdict: "Degraded", NodesReady: 3, NodesTotal: 3,
		},
		Workloads: 62,
		All: []findings.Finding{
			{Level: findings.Critical, Kind: "Pod", Namespace: "shop", Name: "crasher-7d9f-x2", Issue: "CrashLoopBackOff", Reason: "back-off 5m0s restarting failed container", Owner: "Deployment/crasher"},
			{Level: findings.Critical, Kind: "Pod", Namespace: "shop", Name: "badimage-5b8-qq", Issue: "ImagePullBackOff", Reason: "manifest for registry.example/app:9.9 not found"},
			{Level: findings.Warning, Kind: "Deployment", Namespace: "shop", Name: "api", Issue: "not fully available", Reason: "1/3 ready"},
		},
		Blind: []scan.ReadFailure{
			{Resource: "poddisruptionbudgets", Reason: "poddisruptionbudgets is forbidden: User \"system:serviceaccount:kubeagent:kubeagent\" cannot list resource"},
			{Resource: "horizontalpodautoscalers", Reason: "the server could not find the requested resource"},
		},
		Mode:   ModeList,
		Width:  80,
		Height: 24,
	}
}

func TestGoldenFrame(t *testing.T) {
	got := visibleEscapes(Render(goldenModel()))
	if *update {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden frame rewritten")
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("frame differs from %s\n--- got ---\n%s\n--- want ---\n%s\nregenerate with: go test ./internal/tui -run TestGoldenFrame -update", goldenPath, got, want)
	}
}

// Raw mode turns off the terminal's newline translation, so a bare \n moves
// down a row without returning to column 0 and the whole frame staircases.
func TestRender_EveryLineEndsCRLF(t *testing.T) {
	for _, m := range []Model{
		goldenModel(),
		withMode(goldenModel(), ModeDetail),
		withMode(goldenModel(), ModeBlind),
		withMode(goldenModel(), ModeHelp),
	} {
		out := Render(m)
		for i, line := range strings.Split(out, "\n") {
			if i == len(strings.Split(out, "\n"))-1 {
				continue // trailing segment after the final \n
			}
			if !strings.HasSuffix(line, "\r") {
				t.Errorf("mode %v line %d does not end \\r: %q", m.Mode, i, line)
			}
		}
	}
}

func withMode(m Model, mode Mode) Model {
	m.Mode = mode
	return m
}

func TestRender_StartsWithClearAndHome(t *testing.T) {
	out := Render(goldenModel())
	if !strings.HasPrefix(out, "\x1b[2J\x1b[H") {
		t.Errorf("frame does not begin by clearing and homing: %q", out[:min(20, len(out))])
	}
}

// The cursor marks the selected row and nothing else.
func TestRender_CursorMarksSelectedRow(t *testing.T) {
	m := goldenModel()
	m.Cursor = 1
	lines := frameLines(Render(m))
	marked := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "▸ ") {
			marked++
			if !strings.Contains(l, "badimage-5b8-qq") {
				t.Errorf("cursor on the wrong row: %q", l)
			}
		}
	}
	if marked != 1 {
		t.Errorf("%d rows marked, want 1", marked)
	}
}

// No line may exceed the terminal width, or the terminal wraps it and the
// frame's row count no longer matches the screen's.
func TestRender_NarrowTerminalTruncates(t *testing.T) {
	m := goldenModel()
	m.Width = 50
	for _, l := range frameLines(Render(m)) {
		if n := len([]rune(l)); n > 50 {
			t.Errorf("line is %d runes wide, want <= 50: %q", n, l)
		}
	}
}

func TestRender_TooSmallSaysSo(t *testing.T) {
	m := goldenModel()
	m.Width, m.Height = 30, 8
	out := Render(m)
	if !strings.Contains(out, "terminal too small") {
		t.Errorf("frame = %q, want a too-small message", out)
	}
	if strings.Contains(out, "crasher-7d9f-x2") {
		t.Error("frame rendered findings into a terminal too small to hold them")
	}
}

func TestRender_EmptyFindingsSaysSo(t *testing.T) {
	m := goldenModel()
	m.All = nil
	out := Render(m)
	if !strings.Contains(out, "No findings") {
		t.Errorf("frame = %q, want a no-findings message", out)
	}
}

// Under a filter that hides everything the message must name the filter, not
// claim the cluster is clean.
func TestRender_EmptyUnderFilterNamesTheFilter(t *testing.T) {
	m := goldenModel()
	m.All = m.All[2:] // one warning, no criticals
	m.Filter = FilterCritical
	out := Render(m)
	if !strings.Contains(out, "No findings at this filter") {
		t.Errorf("frame = %q, want the filtered wording", out)
	}
}

func TestRender_ColourWrapsSeverity(t *testing.T) {
	m := goldenModel()
	m.Colour = true
	out := Render(m)
	if !strings.Contains(out, "\x1b[31m") {
		t.Error("no red for critical")
	}
	if !strings.Contains(out, "\x1b[33m") {
		t.Error("no yellow for warning")
	}
	if plain := Render(goldenModel()); strings.Contains(plain, "\x1b[31m") {
		t.Error("colour leaked into an uncoloured frame")
	}
}

func TestRender_DetailShowsTheSelectedFinding(t *testing.T) {
	m := withMode(goldenModel(), ModeDetail)
	m.Cursor = 0
	out := Render(m)
	for _, want := range []string{"crasher-7d9f-x2", "CrashLoopBackOff", "back-off 5m0s", "Deployment/crasher"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail pane missing %q", want)
		}
	}
}

// Blind-spot reasons are the cluster's own words and stay verbatim: a TUI frame
// is the operator's own screen, not a forwarded artifact.
func TestRender_BlindShowsReasonsVerbatim(t *testing.T) {
	out := Render(withMode(goldenModel(), ModeBlind))
	for _, want := range []string{"poddisruptionbudgets", "system:serviceaccount:kubeagent:kubeagent", "horizontalpodautoscalers"} {
		if !strings.Contains(out, want) {
			t.Errorf("blind pane missing %q", want)
		}
	}
}

// The coverage claim has to be somewhere the operator can find it. The footer
// is the key map and has no room, so the help screen carries it.
func TestRender_HelpStatesTheCoverageContract(t *testing.T) {
	out := Render(withMode(goldenModel(), ModeHelp))
	if !strings.Contains(out, "kubeagent scan") {
		t.Error("help does not point at kubeagent scan for the opt-in checks")
	}
	if !strings.Contains(out, "--security") {
		t.Error("help does not name an opt-in advisory flag")
	}
}

func TestRender_BusyFrameSaysScanning(t *testing.T) {
	m := goldenModel()
	m.Scanning = true
	if !strings.Contains(Render(m), "scanning") {
		t.Error("busy frame does not say it is scanning")
	}
}

func TestRender_FooterShowsRescanError(t *testing.T) {
	m := goldenModel()
	m.Err = "Get \"https://192.0.2.10:6443/api\": connection refused"
	out := Render(m)
	if !strings.Contains(out, "connection refused") {
		t.Error("footer does not show the re-scan error")
	}
}

// The frame never names the cluster kubeagent is pointed at.
func TestRender_CarriesNoClusterIdentity(t *testing.T) {
	m := goldenModel()
	m.Scope = "namespace shop"
	for _, mode := range []Mode{ModeList, ModeDetail, ModeBlind, ModeHelp} {
		out := Render(withMode(m, mode))
		for _, forbidden := range []string{"kubeconfig", "current-context", "--context"} {
			if strings.Contains(out, forbidden) {
				t.Errorf("mode %v frame contains %q", mode, forbidden)
			}
		}
	}
}

// frameLines splits a frame into displayable lines with the leading control
// sequence and the line terminators removed.
func frameLines(frame string) []string {
	frame = strings.TrimPrefix(frame, "\x1b[2J\x1b[H")
	var out []string
	for _, l := range strings.Split(frame, "\r\n") {
		if l != "" {
			out = append(out, stripSGR(l))
		}
	}
	return out
}

// stripSGR removes colour sequences so a width assertion counts displayed
// runes rather than control bytes.
func stripSGR(s string) string {
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "m")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+1:]
	}
}

func TestRule_SpansTheFullWidth(t *testing.T) {
	if got := len([]rune(rule(80))); got != 80 {
		t.Errorf("rule(80) = %d runes, want 80", got)
	}
	if got := rule(0); got != "" {
		t.Errorf("rule(0) = %q, want empty", got)
	}
}

func TestFooter_AlwaysShowsHelpAndQuit(t *testing.T) {
	// The footer is shed from the right, never truncated mid-word, because the
	// two hints an operator cannot do without are how to get the rest of the key
	// map and how to leave. 80 is the common case; minWidth is the floor.
	for _, w := range []int{minWidth, 60, 80, 120} {
		m := listModel()
		m.Width = w
		got := footer(m)
		if n := len([]rune(got)); n > w {
			t.Errorf("width %d: footer is %d runes: %q", w, n, got)
		}
		if !strings.Contains(got, "q quit") {
			t.Errorf("width %d: footer has no quit hint: %q", w, got)
		}
		if !strings.Contains(got, "? help") {
			t.Errorf("width %d: footer has no help hint: %q", w, got)
		}
	}
}

func TestFooter_KeepsEveryHintWhenItFits(t *testing.T) {
	m := listModel()
	m.Width = 120
	got := footer(m)
	for _, want := range []string{"[1] critical", "↑↓ move", "⏎ detail", "b blind", "r rescan", "? help", "q quit"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer at 120 dropped %q: %q", want, got)
		}
	}
}
