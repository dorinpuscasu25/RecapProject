package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Gemini adapts the Google Generative Language API (generateContent / batchEmbedContents).
type Gemini struct {
	P      *Provider
	Client *http.Client
}

func (g *Gemini) base() string {
	if g.P.BaseURL != "" {
		return strings.TrimSuffix(g.P.BaseURL, "/")
	}
	return "https://generativelanguage.googleapis.com/v1beta"
}

func (g *Gemini) post(ctx context.Context, path string, body any) ([]byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base()+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.P.APIKey)
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, &ProviderError{Category: ErrTransient, Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		return nil, httpError(resp.StatusCode, raw)
	}
	return raw, nil
}

// geminiSchema converts JSON schema to Gemini's OpenAPI-subset schema (no additionalProperties).
func geminiSchema(s map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range ProviderSchema(s) {
		switch k {
		case "additionalProperties":
			continue
		case "properties":
			props := map[string]any{}
			for pk, pv := range v.(map[string]any) {
				props[pk] = geminiSchema(pv.(map[string]any))
			}
			out[k] = props
		case "items":
			out[k] = geminiSchema(v.(map[string]any))
		default:
			out[k] = v
		}
	}
	return out
}

func (g *Gemini) Generate(ctx context.Context, r ProviderRequest) (*ProviderResponse, error) {
	var contents []map[string]any
	for _, m := range r.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{"role": role, "parts": []map[string]string{{"text": m.Content}}})
	}
	gen := map[string]any{"temperature": r.Temperature, "maxOutputTokens": r.MaxTokens}
	if r.Schema != nil {
		gen["responseMimeType"] = "application/json"
		gen["responseSchema"] = geminiSchema(r.Schema)
	}
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]string{{"text": r.System}}},
		"contents":          contents,
		"generationConfig":  gen,
	}
	raw, err := g.post(ctx, "/models/"+url.PathEscape(r.Model.Name)+":generateContent", body)
	if err != nil {
		return nil, err
	}
	var gr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &gr); err != nil {
		return nil, &ProviderError{Category: ErrTransient, Msg: "malformed gemini response"}
	}
	if gr.PromptFeedback.BlockReason != "" {
		return nil, &ProviderError{Category: ErrPermanent, Msg: "blocked: " + gr.PromptFeedback.BlockReason}
	}
	if len(gr.Candidates) == 0 {
		return nil, &ProviderError{Category: ErrTransient, Msg: "no candidates"}
	}
	c := gr.Candidates[0]
	switch c.FinishReason {
	case "MAX_TOKENS":
		return nil, &ProviderError{Category: ErrInvalid, Msg: "output truncated (max tokens)"}
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST":
		return nil, &ProviderError{Category: ErrPermanent, Msg: "finish reason " + c.FinishReason}
	}
	var sb strings.Builder
	for _, p := range c.Content.Parts {
		sb.WriteString(p.Text)
	}
	return &ProviderResponse{Text: sb.String(), Usage: Usage{InputTokens: gr.UsageMetadata.PromptTokenCount, OutputTokens: gr.UsageMetadata.CandidatesTokenCount}}, nil
}

func (g *Gemini) Embed(ctx context.Context, m *Model, texts []string) ([][]float32, Usage, error) {
	var reqs []map[string]any
	for _, t := range texts {
		r := map[string]any{"model": "models/" + m.Name, "content": map[string]any{"parts": []map[string]string{{"text": t}}}}
		if m.EmbeddingDims > 0 {
			r["outputDimensionality"] = m.EmbeddingDims
		}
		reqs = append(reqs, r)
	}
	raw, err := g.post(ctx, "/models/"+url.PathEscape(m.Name)+":batchEmbedContents", map[string]any{"requests": reqs})
	if err != nil {
		return nil, Usage{}, err
	}
	var er struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &er); err != nil || len(er.Embeddings) != len(texts) {
		return nil, Usage{}, &ProviderError{Category: ErrTransient, Msg: "malformed embeddings response"}
	}
	out := make([][]float32, len(texts))
	for i, e := range er.Embeddings {
		if m.EmbeddingDims > 0 && len(e.Values) != m.EmbeddingDims {
			return nil, Usage{}, &ProviderError{Category: ErrPermanent, Msg: "embedding dimension mismatch"}
		}
		out[i] = e.Values
	}
	return out, Usage{}, nil
}
