package ai

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Anthropic adapts the Claude Messages API via the official Go SDK. SDK types stay in this file.
// Structured tasks use output_config.format (JSON schema) rather than forced tool use, which
// current models reject. Sampling parameters are not sent (rejected by current models).
type Anthropic struct {
	P      *Provider
	client anthropic.Client
}

func NewAnthropic(p *Provider, hc *http.Client) *Anthropic {
	opts := []option.RequestOption{option.WithAPIKey(p.APIKey), option.WithMaxRetries(0), option.WithHTTPClient(hc)}
	if p.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.BaseURL))
	}
	for k, v := range p.ExtraHeaders {
		opts = append(opts, option.WithHeader(k, v))
	}
	return &Anthropic{P: p, client: anthropic.NewClient(opts...)}
}

func (a *Anthropic) Generate(ctx context.Context, r ProviderRequest) (*ProviderResponse, error) {
	msgs := make([]anthropic.MessageParam, 0, len(r.Messages))
	for _, m := range r.Messages {
		if m.Role == "assistant" {
			msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		} else {
			msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		}
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(r.Model.Name),
		MaxTokens: int64(r.MaxTokens),
		System:    []anthropic.TextBlockParam{{Text: r.System}},
		Messages:  msgs,
	}
	if r.Schema != nil {
		params.OutputConfig = anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{Schema: ProviderSchema(r.Schema)}}
	}
	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return nil, httpError(apiErr.StatusCode, []byte(apiErr.Error()))
		}
		return nil, &ProviderError{Category: ErrTransient, Msg: err.Error()}
	}
	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return nil, &ProviderError{Category: ErrPermanent, Msg: "model declined the request (refusal)"}
	case anthropic.StopReasonMaxTokens:
		return nil, &ProviderError{Category: ErrInvalid, Msg: "output truncated (max_tokens)"}
	}
	var sb strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	return &ProviderResponse{Text: sb.String(), Usage: Usage{InputTokens: int(resp.Usage.InputTokens), OutputTokens: int(resp.Usage.OutputTokens)}}, nil
}
