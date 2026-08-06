package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// verdictWidth is the width of the VERDICT column: the longest word that can
// appear in it. "unreachable" is 11 and "inconclusive" is 12.
const verdictWidth = 12

// RenderJSON writes the report as the versioned JSON document.
//
// It keeps Unreachable as its own array rather than interleaving it, because a
// consumer filtering clusters[] for failures must not have to know that some
// entries have no counts. The text renderer makes the opposite choice for the
// opposite reason.
func RenderJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// RenderText writes the one-row-per-cluster table, worst first.
//
// Unreachable clusters are interleaved into the same table at rank 0 with their
// reason in the TOP ISSUES column, rather than listed separately: a reader
// scanning rows top-down should not have to find a second table below the fold
// to learn that a cluster went unjudged.
//
// Every selected cluster gets a row. Three hundred clusters is three hundred
// rows — the summary that makes that readable is the row itself, not a cut-off.
func RenderText(w io.Writer, rep Report) error {
	rows := make([]row, 0, len(rep.Clusters)+len(rep.Unreachable))
	for _, u := range rep.Unreachable {
		rows = append(rows, row{context: u.Context, verdict: "unreachable", detail: u.Reason})
	}
	for _, c := range rep.Clusters {
		rows = append(rows, row{
			context: c.Context,
			verdict: c.Verdict,
			crit:    fmt.Sprint(c.Critical),
			warn:    fmt.Sprint(c.Warning),
			info:    fmt.Sprint(c.Info),
			detail:  detailOf(c),
		})
	}

	width := len("CLUSTER")
	for _, r := range rows {
		if len(r.context) > width {
			width = len(r.context)
		}
	}

	failing := 0
	for _, c := range rep.Clusters {
		if c.Verdict == "fail" {
			failing++
		}
	}

	if _, err := fmt.Fprintf(w, "FLEET  %d clusters, %d failing, %d unreachable\n\n",
		len(rep.Clusters)+len(rep.Unreachable), failing, len(rep.Unreachable)); err != nil {
		return err
	}

	if err := writeRow(w, width, row{context: "CLUSTER", verdict: "VERDICT",
		crit: "CRIT", warn: "WARN", info: "INFO", detail: "TOP ISSUES"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := writeRow(w, width, r); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintf(w, "\nverdict: %s (exit %d)\n", rep.Verdict, rep.Code)
	return err
}

// row is one rendered line. The count cells are strings rather than ints so an
// unreachable cluster can leave them blank — it has no counts, and printing 0
// would claim kubeagent looked and found nothing.
type row struct {
	context, verdict, crit, warn, info, detail string
}

func writeRow(w io.Writer, width int, r row) error {
	line := fmt.Sprintf("%-*s  %-*s  %4s  %4s  %4s  %s",
		width, r.context, verdictWidth, r.verdict, r.crit, r.warn, r.info, r.detail)
	_, err := fmt.Fprintln(w, strings.TrimRight(line, " "))
	return err
}

// detailOf builds the TOP ISSUES cell for a judged cluster. A blind-spot count
// is appended rather than replacing the issue kinds: a cluster can perfectly
// well have both, and showing only one would hide the other.
func detailOf(c ClusterSummary) string {
	detail := strings.Join(c.TopIssues, ", ")
	if c.Blindspots == 0 {
		return detail
	}
	noun := "blind spots"
	if c.Blindspots == 1 {
		noun = "blind spot"
	}
	blind := fmt.Sprintf("(%d %s)", c.Blindspots, noun)
	if detail == "" {
		return blind
	}
	return detail + " " + blind
}
