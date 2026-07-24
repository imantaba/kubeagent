// Package audit writes a durable, append-only JSON-Lines record of every --fix
// remediation outcome. It records only safe display values (the same revision /
// image / count fields the preview shows, plus our own detail strings) — never
// pod specs, env, or secrets.
package audit

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/imantaba/kubeagent/internal/remediate"
)

// Record is one durable audit entry: what was proposed and what became of it.
type Record struct {
	Time         string             `json:"time"`
	Kind         string             `json:"kind"`
	Namespace    string             `json:"namespace,omitempty"`
	Name         string             `json:"name"`
	Target       string             `json:"target"`
	Changes      []remediate.Change `json:"changes,omitempty"`
	Disposition  string             `json:"disposition"`
	Detail       string             `json:"detail,omitempty"`
	FromRevision int                `json:"fromRevision,omitempty"` // RolloutUndo: revision before the fix (enables rollback)
	ToRevision   int                `json:"toRevision,omitempty"`   // RolloutUndo: revision the fix landed on
}

// RecordFor builds a Record from an action, its disposition, a detail string, and a
// clock. Pure — no I/O. now is formatted as RFC3339 in UTC.
func RecordFor(a remediate.Action, disposition, detail string, now time.Time) Record {
	return Record{
		Time:         now.UTC().Format(time.RFC3339),
		Kind:         a.Kind,
		Namespace:    a.Namespace,
		Name:         a.Name,
		Target:       a.Target,
		Changes:      a.Changes,
		Disposition:  disposition,
		Detail:       detail,
		FromRevision: a.CurrentRevision,
		ToRevision:   a.TargetRevision,
	}
}

// Writer appends JSON-Lines records to an underlying writer (the open audit file,
// or any io.Writer in tests). One JSON object per line.
type Writer struct {
	w io.Writer
}

// NewWriter wraps w as an audit Writer.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Log marshals r to a single JSON line (terminated by "\n") and writes it. It
// returns any marshal or write error.
func (a *Writer) Log(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = a.w.Write(b)
	return err
}

// ReadLast scans the JSON-Lines audit file and returns the most recent record
// satisfying want. Malformed lines are skipped so a truncated tail cannot break
// rollback; found is false when no record matches.
func ReadLast(path string, want func(Record) bool) (Record, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, false, err
	}
	defer f.Close()
	var last Record
	var found bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue // skip malformed/truncated lines
		}
		if want(r) {
			last, found = r, true
		}
	}
	if err := sc.Err(); err != nil {
		return Record{}, false, err
	}
	return last, found, nil
}
