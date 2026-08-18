package controlplane

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// checkLines builds a synthetic /readyz?verbose body with n distinct "[-]"
// failed-check lines, named check-00, check-01, ….
func checkLines(n int) []byte {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("[-]check-%02d failed", i)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// TestFailedChecks_ExactlyAtCap pins the unchanged common case: a body with
// exactly MaxFailedChecks failing lines returns exactly that many entries —
// this is the row that proves R84 did not move the everyday cap.
func TestFailedChecks_ExactlyAtCap(t *testing.T) {
	got := failedChecks(checkLines(MaxFailedChecks))
	if len(got) != MaxFailedChecks {
		t.Fatalf("len(failedChecks) = %d, want %d", len(got), MaxFailedChecks)
	}
}

// TestFailedChecks_CapPlusOne pins R84: a body with more failing lines than
// the cap returns exactly MaxFailedChecks+1 entries, never more — enough for
// a caller to detect truncation happened without an unbounded count.
func TestFailedChecks_CapPlusOne(t *testing.T) {
	got := failedChecks(checkLines(25))
	if len(got) != MaxFailedChecks+1 {
		t.Fatalf("len(failedChecks) = %d, want %d", len(got), MaxFailedChecks+1)
	}
}

func TestParseReadyz_OK(t *testing.T) {
	body := []byte("[+]ping ok\n[+]etcd ok\nreadyz check passed\n")
	got := ParseReadyz(200, body)
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if len(got.Failed) != 0 {
		t.Errorf("Failed = %v, want none", got.Failed)
	}
}

func TestParseReadyz_UnhealthyListsFailedChecks(t *testing.T) {
	body := []byte("[+]ping ok\n[+]etcd ok\n[-]poststarthook/x failed: reason\n[-]informer-sync failed\nreadyz check failed\n")
	got := ParseReadyz(500, body)
	if got.Status != "unhealthy" {
		t.Fatalf("Status = %q, want unhealthy", got.Status)
	}
	want := []string{"poststarthook/x", "informer-sync"}
	if !reflect.DeepEqual(got.Failed, want) {
		t.Errorf("Failed = %v, want %v", got.Failed, want)
	}
}

func TestParseReadyz_EtcdFailure(t *testing.T) {
	body := []byte("[+]ping ok\n[-]etcd failed: reason withheld\nreadyz check failed\n")
	got := ParseReadyz(500, body)
	if got.Status != "unhealthy" {
		t.Fatalf("Status = %q, want unhealthy", got.Status)
	}
	found := false
	for _, f := range got.Failed {
		if f == "etcd" {
			found = true
		}
	}
	if !found {
		t.Errorf("Failed = %v, want it to contain etcd", got.Failed)
	}
}

func TestParseReadyz_Forbidden(t *testing.T) {
	for _, code := range []int{401, 403} {
		if got := ParseReadyz(code, nil); got.Status != "forbidden" {
			t.Errorf("code %d: Status = %q, want forbidden", code, got.Status)
		}
	}
}

func TestParseReadyz_Unreachable(t *testing.T) {
	if got := ParseReadyz(0, nil); got.Status != "unreachable" {
		t.Errorf("Status = %q, want unreachable", got.Status)
	}
}

func TestParseReadyz_EmptyBodyUnhealthy(t *testing.T) {
	got := ParseReadyz(503, nil)
	if got.Status != "unhealthy" {
		t.Errorf("Status = %q, want unhealthy", got.Status)
	}
	if len(got.Failed) != 0 {
		t.Errorf("Failed = %v, want none (generic not-ready)", got.Failed)
	}
}
