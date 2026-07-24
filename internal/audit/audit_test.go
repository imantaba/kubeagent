package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/remediate"
)

var fixedNow = time.Date(2026, 7, 24, 6, 30, 0, 0, time.UTC)

func TestRecordFor_MapsActionAndDisposition(t *testing.T) {
	a := remediate.Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		Target:  "shop/web (Deployment)",
		Changes: []remediate.Change{{Field: "revision", From: "5", To: "4"}},
	}
	r := RecordFor(a, "applied", "rolled back shop/web to revision 4", fixedNow)
	if r.Time != "2026-07-24T06:30:00Z" {
		t.Errorf("time = %q, want RFC3339 UTC", r.Time)
	}
	if r.Kind != "RolloutUndo" || r.Namespace != "shop" || r.Name != "web" || r.Target != "shop/web (Deployment)" {
		t.Errorf("action fields not mapped: %+v", r)
	}
	if r.Disposition != "applied" || r.Detail != "rolled back shop/web to revision 4" {
		t.Errorf("disposition/detail wrong: %+v", r)
	}
	if len(r.Changes) != 1 || r.Changes[0] != (remediate.Change{Field: "revision", From: "5", To: "4"}) {
		t.Errorf("changes not passed through: %+v", r.Changes)
	}
}

func TestRecordFor_NodeActionEmptyNamespace(t *testing.T) {
	a := remediate.Action{Kind: "Uncordon", Name: "worker-1", Target: "node/worker-1"}
	r := RecordFor(a, "dry-run", "", fixedNow)
	if r.Namespace != "" {
		t.Errorf("node action namespace = %q, want empty", r.Namespace)
	}
	if r.Disposition != "dry-run" {
		t.Errorf("disposition = %q", r.Disposition)
	}
}

func TestWriter_LogWritesOneJSONLinePerCall(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Log(RecordFor(remediate.Action{Kind: "Uncordon", Name: "n1", Target: "node/n1"}, "applied", "uncordoned node n1", fixedNow)); err != nil {
		t.Fatal(err)
	}
	if err := w.Log(RecordFor(remediate.Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web", Target: "shop/web (Deployment)"}, "refused", "state changed since preview; no write made", fixedNow)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	for i, line := range lines {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Errorf("line %d is not standalone JSON: %v (%q)", i, err, line)
		}
	}
	// spot-check the second record's disposition round-trips
	var second Record
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if second.Disposition != "refused" {
		t.Errorf("second disposition = %q, want refused", second.Disposition)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriter_LogSurfacesWriteError(t *testing.T) {
	w := NewWriter(failWriter{})
	if err := w.Log(RecordFor(remediate.Action{Kind: "Uncordon", Name: "n1"}, "applied", "", fixedNow)); err == nil {
		t.Error("expected a write error to surface")
	}
}
