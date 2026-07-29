package tui

import (
	"strings"
	"testing"
)

func TestCheckTTY(t *testing.T) {
	tests := []struct {
		name    string
		isTerm  func(int) bool
		wantErr bool
	}{
		{"both a terminal", func(int) bool { return true }, false},
		{"neither a terminal", func(int) bool { return false }, true},
		{"stdout redirected", func(fd int) bool { return fd == 0 }, true},
		{"stdin piped", func(fd int) bool { return fd == 1 }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkTTY(0, 1, tt.isTerm)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkTTY() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// The refusal has to tell the operator what to run instead, and must not name
// the cluster: it is printed before kubeagent has touched the network, and it
// lands in whatever the operator redirected stdout to.
func TestCheckTTY_RefusalIsActionableAndIdentityFree(t *testing.T) {
	err := checkTTY(0, 1, func(int) bool { return false })
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kubeagent scan") {
		t.Errorf("refusal does not point at kubeagent scan: %q", msg)
	}
	if !strings.Contains(msg, "interactive terminal") {
		t.Errorf("refusal does not say why: %q", msg)
	}
	for _, forbidden := range []string{"kubeconfig", "--context", "https://"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("refusal contains %q: %q", forbidden, msg)
		}
	}
}
