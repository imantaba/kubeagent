// OpenAI-compatible /chat/completions backend for --explain, so the summary can be
// produced by a local model (Ollama, vLLM, llama.cpp, LM Studio, …) with no API key
// and no data leaving the caller's network. Read-only; one call, same prompt.
package explain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// openaiSummarizer talks to an OpenAI-compatible /chat/completions endpoint.
type openaiSummarizer struct {
	endpoint string // e.g. "http://localhost:11434/v1"
	model    string // e.g. "llama3.1"
	apiKey   string // optional bearer token ("" = no auth header)
	http     *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
}
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func (o openaiSummarizer) summarize(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:  o.model,
		Stream: false,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(o.endpoint, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling local explain endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("local explain endpoint returned %d: %s", resp.StatusCode, snippet(raw))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("parsing local explain response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("local explain endpoint returned no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

// snippet trims an endpoint's error body for inclusion in an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
