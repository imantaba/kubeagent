// Package audit writes a durable, append-only JSON-Lines record of every --fix
// remediation outcome. It records only safe display values (the same revision /
// image / count fields the preview shows, plus our own detail strings) — never
// pod specs, env, or secrets.
package audit

import (
	"encoding/json"
	"io"
	"time"

	"github.com/imantaba/kubeagent/internal/remediate"
)

// Record is one durable audit entry: what was proposed and what became of it.
type Record struct {
	Time        string             `json:"time"`
	Kind        string             `json:"kind"`
	Namespace   string             `json:"namespace,omitempty"`
	Name        string             `json:"name"`
	Target      string             `json:"target"`
	Changes     []remediate.Change `json:"changes,omitempty"`
	Disposition string             `json:"disposition"`
	Detail      string             `json:"detail,omitempty"`
}

// RecordFor builds a Record from an action, its disposition, a detail string, and a
// clock. Pure — no I/O. now is formatted as RFC3339 in UTC.
func RecordFor(a remediate.Action, disposition, detail string, now time.Time) Record {
	return Record{
		Time:        now.UTC().Format(time.RFC3339),
		Kind:        a.Kind,
		Namespace:   a.Namespace,
		Name:        a.Name,
		Target:      a.Target,
		Changes:     a.Changes,
		Disposition: disposition,
		Detail:      detail,
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
