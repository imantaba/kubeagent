package baseline

import "testing"

// FuzzBaselineLoad asserts that no byte sequence in an operator-supplied
// baseline file can panic the loader, and that anything Load accepts is safe to
// use: every number is finite, every entry is identifiable, and Compare can run
// against it without producing a non-finite rate. It joins the fuzz targets
// Theme H slice 3 added — the document is semi-trusted input in exactly the
// class already covered there.
func FuzzBaselineLoad(f *testing.F) {
	f.Add(`{"schemaVersion":"1.0","capturedAt":"2026-01-01T00:00:00Z","minPodAgeSeconds":3600,` +
		`"workloads":[{"kind":"Deployment","namespace":"prod","name":"api",` +
		`"restartsPerHour":0.5,"pods":3,"observedSeconds":10800}]}`)
	f.Add(`{"schemaVersion":"1.0","capturedAt":"2026-01-01T00:00:00Z","minPodAgeSeconds":3600,"workloads":[`)
	f.Add(`{"schemaVersion":"2.0","workloads":[]}`)
	f.Add(`{"schemaVersion":"1.0","minPodAgeSeconds":1e400,"workloads":[]}`)
	f.Add(`{"schemaVersion":"1.0","workloads":[{"kind":"Deployment","name":"api","restartsPerHour":1e400}]}`)
	f.Add(`{"schemaVersion":"1.0","workloads":null}`)
	f.Add(`{"schemaVersion":"\x00.0","workloads":[]}`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, src string) {
		doc, err := Load([]byte(src))
		if err != nil {
			return
		}
		if doc.Workloads == nil {
			t.Fatal("Load returned a nil Workloads slice")
		}
		for i, e := range doc.Workloads {
			if e.Kind == "" || e.Name == "" {
				t.Errorf("accepted entry %d has no kind or no name", i)
			}
			if !usableNumber(e.RestartsPerHour) || e.RestartsPerHour < 0 {
				t.Errorf("accepted entry %d has restartsPerHour %v", i, e.RestartsPerHour)
			}
		}

		// A document Load accepted must be comparable without panicking and
		// without producing a rate no renderer can print.
		rep := Compare(doc, []PodSample{
			{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 9, AgeSeconds: 7200},
		}, CompareOptions{})
		if rep.Deviations == nil {
			t.Error("Compare returned a nil Deviations slice")
		}
		for _, d := range rep.Deviations {
			if !usableNumber(d.BaselineRate) || !usableNumber(d.CurrentRate) {
				t.Errorf("deviation carries a non-finite rate: %+v", d)
			}
		}

		// Round trip: what Load accepts, Marshal must re-emit and Load must
		// accept again. A document that survives one hop but not two would make
		// re-capturing a file lossy.
		b, merr := doc.Marshal()
		if merr != nil {
			t.Fatalf("Marshal rejected a document Load accepted: %v", merr)
		}
		again, aerr := Load(b)
		if aerr != nil {
			t.Fatalf("a document Load accepted did not survive Marshal+Load: %v", aerr)
		}
		if len(again.Workloads) != len(doc.Workloads) {
			t.Errorf("round trip changed the workload count: %d then %d", len(doc.Workloads), len(again.Workloads))
		}
	})
}
