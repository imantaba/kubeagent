package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestRecordFor_CarriesRevisions(t *testing.T) {
	a := remediate.Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		Target: "shop/web (Deployment)", CurrentRevision: 5, TargetRevision: 4}
	r := RecordFor(a, "applied", "rolled back", fixedNow)
	if r.FromRevision != 5 || r.ToRevision != 4 {
		t.Errorf("revisions = %d/%d, want 5/4", r.FromRevision, r.ToRevision)
	}
}

func TestRecordFor_UncordonHasNoRevisions(t *testing.T) {
	a := remediate.Action{Kind: "Uncordon", Name: "worker-1", Target: "node/worker-1"}
	r := RecordFor(a, "applied", "uncordoned", fixedNow)
	if r.FromRevision != 0 || r.ToRevision != 0 {
		t.Errorf("node action must have zero revisions, got %d/%d", r.FromRevision, r.ToRevision)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "fromRevision") {
		t.Errorf("zero revisions must be omitted from JSON: %s", b)
	}
}

func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func applied(r Record) bool { return r.Disposition == "applied" }

func TestReadLast_ReturnsNewestMatch(t *testing.T) {
	p := writeLines(t,
		`{"time":"2026-07-24T06:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied","fromRevision":3,"toRevision":2}`,
		`{"time":"2026-07-24T07:00:00Z","kind":"Uncordon","name":"w1","target":"node/w1","disposition":"refused"}`,
		`{"time":"2026-07-24T08:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied","fromRevision":5,"toRevision":4}`,
	)
	rec, found, err := ReadLast(p, applied)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if rec.FromRevision != 5 || rec.ToRevision != 4 {
		t.Errorf("want the newest applied record (5→4), got %d→%d", rec.FromRevision, rec.ToRevision)
	}
}

func TestReadLast_SkipsMalformedLines(t *testing.T) {
	p := writeLines(t,
		`{"time":"2026-07-24T06:00:00Z","kind":"Uncordon","name":"w1","target":"node/w1","disposition":"applied"}`,
		`this is not json`,
		`{"partial":`,
	)
	rec, found, err := ReadLast(p, applied)
	if err != nil || !found || rec.Name != "w1" {
		t.Fatalf("malformed lines must be skipped; got rec=%+v found=%v err=%v", rec, found, err)
	}
}

func TestReadLast_NoMatch(t *testing.T) {
	p := writeLines(t, `{"time":"2026-07-24T06:00:00Z","kind":"Uncordon","name":"w1","target":"node/w1","disposition":"declined"}`)
	if _, found, err := ReadLast(p, applied); err != nil || found {
		t.Fatalf("want found=false without error, got found=%v err=%v", found, err)
	}
}

func TestReadLast_MissingFileErrors(t *testing.T) {
	if _, _, err := ReadLast(filepath.Join(t.TempDir(), "nope.log"), applied); err == nil {
		t.Error("expected an error for a missing audit file")
	}
}
