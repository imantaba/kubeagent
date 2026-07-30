package scan

import "testing"

func TestScanWorkersDefault(t *testing.T) {
	t.Setenv("KUBEAGENT_SCAN_WORKERS", "")
	if got := scanWorkers(); got != 8 {
		t.Errorf("scanWorkers() = %d, want 8", got)
	}
}

func TestScanWorkersHonoursTheEnvironment(t *testing.T) {
	t.Setenv("KUBEAGENT_SCAN_WORKERS", "3")
	if got := scanWorkers(); got != 3 {
		t.Errorf("scanWorkers() = %d, want 3", got)
	}
}

// A bad knob must degrade to a working scan, never to an error.
func TestScanWorkersFallsBackOnAnUnparseableValue(t *testing.T) {
	for _, v := range []string{"eight", "3.5", "1_000", " 4"} {
		t.Setenv("KUBEAGENT_SCAN_WORKERS", v)
		if got := scanWorkers(); got != 8 {
			t.Errorf("KUBEAGENT_SCAN_WORKERS=%q gave %d, want the default 8", v, got)
		}
	}
}

func TestScanWorkersClampsToTheNearerBound(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"0", 1},
		{"-1", 1},
		{"-1000", 1},
		{"1", 1},
		{"64", 64},
		{"65", 64},
		{"100000", 64},
	}
	for _, tc := range cases {
		t.Setenv("KUBEAGENT_SCAN_WORKERS", tc.env)
		if got := scanWorkers(); got != tc.want {
			t.Errorf("KUBEAGENT_SCAN_WORKERS=%q gave %d, want %d", tc.env, got, tc.want)
		}
	}
}
