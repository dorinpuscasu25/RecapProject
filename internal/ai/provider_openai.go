package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatible speaks the /chat/completions, /embeddings and /audio/speech wire format used
// by OpenAI and many compatible servers (vLLM, Ollama, LiteLLM, OpenRouter, Azure-style gateways).
type OpenAICompatible struct {
	P      *Provider
	Client *http.Client
}

func (o *OpenAICompatible) url(path string) string {
	base := strings.TrimSuffix(o.P.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return base + path
}

func (o *OpenAICompatible) do(ctx context.Context, path string, body any) ([]byte, http.Header, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url(path), bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.P.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.P.APIKey)
	}
	for k, v := range o.P.ExtraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, nil, &ProviderError{Category: ErrTransient, Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, nil, &ProviderError{Category: ErrTransient, Msg: err.Error()}
	}
	if resp.StatusCode >= 300 {
		return nil, nil, httpError(resp.StatusCode, raw)
	}
	return raw, resp.Header, nil
}

func httpError(status int, raw []byte) error {
	msg := string(raw)
	if len(msg) > 300 {
		msg = msg[:300]
	}
	cat := ErrPermanent
	if status == 429 || status == 408 || status >= 500 {
		cat = ErrTransient
	}
	return &ProviderError{Category: cat, Status: status, Msg: msg}
}

type oaChatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (o *OpenAICompatible) Generate(ctx context.Context, r ProviderRequest) (*ProviderResponse, error) {
	msgs := []map[string]string{{"role": "system", "content": r.System}}
	for _, m := range r.Messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{"model": r.Model.Name, "messages": msgs, "temperature": r.Temperature, "max_tokens": r.MaxTokens}
	if r.Schema != nil {
		switch {
		case r.Model.SupportsStructured:
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{
				"name": r.SchemaName, "strict": true, "schema": ProviderSchema(r.Schema)}}
		case r.Model.SupportsJSONMode:
			body["response_format"] = map[string]any{"type": "json_object"}
			msgs[0]["content"] += "\nReturn a single JSON object matching this JSON schema:\n" + mustJSON(r.Schema)
		default:
			msgs[0]["content"] += "\nReturn only a single JSON object (no prose, no code fences) matching this JSON schema:\n" + mustJSON(r.Schema)
		}
	}
	raw, _, err := o.do(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	var cr oaChatResp
	if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
		return nil, &ProviderError{Category: ErrTransient, Msg: "malformed chat response"}
	}
	c := cr.Choices[0]
	if c.Message.Refusal != "" {
		return nil, &ProviderError{Category: ErrPermanent, Msg: "model refused: " + c.Message.Refusal}
	}
	if c.FinishReason == "length" {
		return nil, &ProviderError{Category: ErrInvalid, Msg: "output truncated (max_tokens)"}
	}
	return &ProviderResponse{Text: c.Message.Content, Usage: Usage{InputTokens: cr.Usage.PromptTokens, OutputTokens: cr.Usage.CompletionTokens}}, nil
}

func (o *OpenAICompatible) Embed(ctx context.Context, m *Model, texts []string) ([][]float32, Usage, error) {
	body := map[string]any{"model": m.Name, "input": texts}
	if m.EmbeddingDims > 0 {
		if v, ok := m.ProviderMeta()["send_dimensions"].(bool); ok && v {
			body["dimensions"] = m.EmbeddingDims
		}
	}
	raw, _, err := o.do(ctx, "/embeddings", body)
	if err != nil {
		return nil, Usage{}, err
	}
	var er struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &er); err != nil || len(er.Data) != len(texts) {
		return nil, Usage{}, &ProviderError{Category: ErrTransient, Msg: "malformed embeddings response"}
	}
	out := make([][]float32, len(texts))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, Usage{}, &ProviderError{Category: ErrTransient, Msg: "embedding index out of range"}
		}
		if m.EmbeddingDims > 0 && len(d.Embedding) != m.EmbeddingDims {
			return nil, Usage{}, &ProviderError{Category: ErrPermanent, Msg: fmt.Sprintf("dimension mismatch: got %d want %d", len(d.Embedding), m.EmbeddingDims)}
		}
		out[d.Index] = d.Embedding
	}
	return out, Usage{InputTokens: er.Usage.PromptTokens}, nil
}

func (o *OpenAICompatible) Speak(ctx context.Context, m *Model, r SpeechRequest) (*SpeechResult, error) {
	format := r.Format
	if format == "" {
		format = "mp3"
	}
	raw, hdr, err := o.do(ctx, "/audio/speech", map[string]any{"model": m.Name, "input": r.Text, "voice": r.Voice, "response_format": format})
	if err != nil {
		return nil, err
	}
	ct := hdr.Get("Content-Type")
	if ct == "" {
		ct = "audio/mpeg"
	}
	return &SpeechResult{Audio: raw, ContentType: ct, Usage: Usage{Characters: len([]rune(r.Text))}}, nil
}

func defaultHTTPClient(timeoutMS int) *http.Client {
	t := time.Duration(timeoutMS) * time.Millisecond
	if t <= 0 {
		t = 60 * time.Second
	}
	return &http.Client{Timeout: t}
}
