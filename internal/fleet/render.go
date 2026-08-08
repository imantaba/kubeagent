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

// maxNamedClusters caps the row identities a shared-signal line spells out
// before it counts the rest. Three, on the same reasoning that caps TopIssues:
// the line has to stay readable when a signal spans three hundred clusters, and
// the document carries every name for whoever needs them all.
const maxNamedClusters = 3

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
		rows = append(rows, row{id: identity(u.Name, u.Context), verdict: "unreachable", detail: u.Reason})
	}
	for _, c := range rep.Clusters {
		rows = append(rows, row{
			id:      identity(c.Name, c.Context),
			verdict: c.Verdict,
			crit:    fmt.Sprint(c.Critical),
			warn:    fmt.Sprint(c.Warning),
			info:    fmt.Sprint(c.Info),
			detail:  detailOf(c),
		})
	}

	width := len("CLUSTER")
	for _, r := range rows {
		if len(r.id) > width {
			width = len(r.id)
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

	if err := writeRow(w, width, row{id: "CLUSTER", verdict: "VERDICT",
		crit: "CRIT", warn: "WARN", info: "INFO", detail: "TOP ISSUES"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := writeRow(w, width, r); err != nil {
			return err
		}
	}

	if err := renderShared(w, rep); err != nil {
		return err
	}

	_, err := fmt.Fprintf(w, "\nverdict: %s (exit %d)\n", rep.Verdict, rep.Code)
	return err
}

// row is one rendered line. The count cells are strings rather than ints so an
// unreachable cluster can leave them blank — it has no counts, and printing 0
// would claim kubeagent looked and found nothing.
type row struct {
	id, verdict, crit, warn, info, detail string
}

func writeRow(w io.Writer, width int, r row) error {
	line := fmt.Sprintf("%-*s  %-*s  %4s  %4s  %4s  %s",
		width, r.id, verdictWidth, r.verdict, r.crit, r.warn, r.info, r.detail)
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

// renderShared writes the two correlation sections, between the cluster table
// and the verdict line.
//
// A section with no entries is omitted entirely, heading included: a heading
// over nothing reads as a failed render. The signal column is padded to one
// width computed across both sections rather than per section, because two
// sections that nearly line up read as a bug.
//
// The verdict line's own leading newline supplies the blank line after the last
// row here, so this function emits no trailing blank of its own.
func renderShared(w io.Writer, rep Report) error {
	judged := len(rep.Clusters)

	var issues, blindspots []Shared
	countWidth, signalWidth := 0, 0
	for _, s := range rep.Shared {
		if s.Source == SourceIssue {
			issues = append(issues, s)
		} else {
			blindspots = append(blindspots, s)
		}
		if n := len(countCell(s, judged)); n > countWidth {
			countWidth = n
		}
		if n := len(s.Signal); n > signalWidth {
			signalWidth = n
		}
	}

	for _, section := range []struct {
		title   string
		entries []Shared
	}{
		{"SHARED ISSUES", issues},
		{"SHARED BLIND SPOTS", blindspots},
	} {
		if len(section.entries) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "\n%s  in %d or more of %d judged clusters\n\n",
			section.title, minShared, judged); err != nil {
			return err
		}
		for _, s := range section.entries {
			line := fmt.Sprintf("  %*s  %-*s  %s",
				countWidth, countCell(s, judged),
				signalWidth, s.Signal,
				namedClusters(s.Clusters))
			if _, err := fmt.Fprintln(w, strings.TrimRight(line, " ")); err != nil {
				return err
			}
		}
	}
	return nil
}

// countCell is the N/M cell: how many judged clusters showed this signal, out
// of how many were judged.
//
// The denominator is judged clusters and not selected ones. An unreachable
// cluster produced no verdict and so contributed no evidence, and counting it
// would make a 2-of-2 correlation read as 2-of-5 — understating exactly the
// thing the section exists to surface.
func countCell(s Shared, judged int) string {
	return fmt.Sprintf("%d/%d", len(s.Clusters), judged)
}

// namedClusters spells out at most maxNamedClusters row identities and then
// says how many it left out.
func namedClusters(ids []string) string {
	if len(ids) <= maxNamedClusters {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s, +%d more",
		strings.Join(ids[:maxNamedClusters], ", "), len(ids)-maxNamedClusters)
}
