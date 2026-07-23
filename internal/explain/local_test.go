package explain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
)

func TestOpenAISummarizer_PostsAndParses(t *testing.T) {
	var gotBody []byte
	var gotAuth, gotPath, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"two nodes NotReady"}}]}`)
	}))
	defer srv.Close()

	o := openaiSummarizer{endpoint: srv.URL, model: "llama3.1", apiKey: "sekret", http: srv.Client()}
	out, err := o.summarize(context.Background(), "PROMPT-BODY")
	if err != nil {
		t.Fatal(err)
	}
	if out != "two nodes NotReady" {
		t.Errorf("out = %q, want the response content", out)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("auth = %q, want Bearer sekret", gotAuth)
	}
	var req chatRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "llama3.1" {
		t.Errorf("model = %q", req.Model)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v, want [system,user]", req.Messages)
	}
	if req.Messages[0].Content != systemPrompt {
		t.Error("system message is not systemPrompt")
	}
	if req.Messages[1].Content != "PROMPT-BODY" {
		t.Error("user message is not the prompt")
	}
}

func TestOpenAISummarizer_NoAuthHeaderWhenNoKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	o := openaiSummarizer{endpoint: srv.URL, model: "m", apiKey: "", http: srv.Client()}
	if _, err := o.summarize(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("expected no Authorization header, got %q", gotAuth)
	}
}

func TestOpenAISummarizer_Errors(t *testing.T) {
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "model overloaded")
	}))
	defer srv500.Close()
	if _, err := (openaiSummarizer{endpoint: srv500.URL, model: "m", http: srv500.Client()}).summarize(context.Background(), "p"); err == nil {
		t.Error("want an error on a 500 response")
	}

	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srvEmpty.Close()
	if _, err := (openaiSummarizer{endpoint: srvEmpty.URL, model: "m", http: srvEmpty.Client()}).summarize(context.Background(), "p"); err == nil {
		t.Error("want an error when the response has no choices")
	}
}

func TestNewFromConfig_LocalBackendEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"local says degraded"}}]}`)
	}))
	defer srv.Close()

	c := NewFromConfig("llama3.1", srv.URL, "")
	out, err := c.ExplainInventory(context.Background(),
		clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 1, NodesTotal: 2}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "local says degraded" {
		t.Errorf("out = %q, want the local endpoint's content", out)
	}
}
