package fleetfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    []Entry
		wantErr string // a substring the error must contain; "" means no error
	}{
		{
			name: "a multi-entry file loads in file order",
			yaml: "- context: prod-eu\n" +
				"- context: prod-us\n" +
				"- name: edge-a\n" +
				"  kubeconfig: /fleet/edge-a.kubeconfig\n" +
				"  context: default\n",
			want: []Entry{
				{Name: "prod-eu", Context: "prod-eu"},
				{Name: "prod-us", Context: "prod-us"},
				{Name: "edge-a", Kubeconfig: "/fleet/edge-a.kubeconfig", Context: "default"},
			},
		},
		{
			name: "name defaults to context",
			yaml: "- context: staging\n",
			want: []Entry{{Name: "staging", Context: "staging"}},
		},
		{
			name:    "a missing context is refused",
			yaml:    "- name: sandbox\n",
			wantErr: "entry 1 has no context",
		},
		{
			name:    "a whitespace-only context is refused",
			yaml:    "- context: \"   \"\n",
			wantErr: "entry 1 has no context",
		},
		{
			name:    "an empty list is refused",
			yaml:    "[]\n",
			wantErr: "names no clusters",
		},
		{
			name:    "an empty document is refused",
			yaml:    "",
			wantErr: "names no clusters",
		},
		{
			name:    "two entries resolving to one identity are refused",
			yaml:    "- context: default\n- context: default\n",
			wantErr: `entry 1 and entry 2 are both named "default"`,
		},
		{
			name:    "an unknown field is refused",
			yaml:    "- context: prod-eu\n  cluster: prod-eu\n",
			wantErr: "cluster",
		},
		{
			name:    "a server URL is refused: the format has no field for one",
			yaml:    "- context: prod-eu\n  server: https://api.example.com\n",
			wantErr: "server",
		},
		{
			name:    "a token is refused: the format has no field for one",
			yaml:    "- context: prod-eu\n  token: <PLACEHOLDER>\n",
			wantErr: "token",
		},
		{
			name:    "a typo'd kubeconfig key fails loudly rather than falling back",
			yaml:    "- context: prod-eu\n  kubconfig: /fleet/prod-eu.kubeconfig\n",
			wantErr: "kubconfig",
		},
		{
			name:    "a mapping instead of a list is refused",
			yaml:    "context: prod-eu\n",
			wantErr: "invalid YAML",
		},
		{
			name:    "malformed YAML is refused",
			yaml:    "- context: [unclosed\n",
			wantErr: "invalid YAML",
		},
		{
			name: "a name carrying a control character is sanitized at ingress",
			yaml: "- name: \"edge\\ta\"\n  context: prod-eu\n",
			want: []Entry{{Name: "edge a", Context: "prod-eu"}},
		},
		{
			name:    "a name that sanitizes to empty is refused",
			yaml:    "- name: \"\\u202E\"\n  context: prod-eu\n",
			wantErr: "entry 1 has an empty name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() error = nil, want one containing %q (got %+v)", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want none", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// No error this package produces may name an entry's kubeconfig path. No
// validation failure is about that field, so the question never arises — and a
// path in an error is a credential in whatever collects that error.
func TestLoadErrorsNameNoKubeconfigPath(t *testing.T) {
	const marker = "/fleet/MARKERVALUE.kubeconfig"
	for _, body := range []string{
		"- kubeconfig: " + marker + "\n",
		"- kubeconfig: " + marker + "\n  context: prod-eu\n  cluster: prod-eu\n",
		"- kubeconfig: " + marker + "\n  name: \"\\u202E\"\n  context: prod-eu\n",
	} {
		_, err := Load([]byte(body))
		if err == nil {
			t.Fatalf("Load(%q) error = nil, want one", body)
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("Load(%q) error = %q, want it to name no kubeconfig path", body, err)
		}
	}
}
