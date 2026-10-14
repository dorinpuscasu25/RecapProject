package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Contract tests: each adapter is exercised against a local server that checks the wire format
// the real API expects and replays representative responses (success, refusal/truncation, errors).

var testSchema = map[string]any{"type": "object", "additionalProperties": false, "required": []any{"answer"},
	"properties": map[string]any{"answer": map[string]any{"type": "string", "maxLength": 200}}}

func req(model *Model) ProviderRequest {
	return ProviderRequest{Model: model, Task: TaskChatAnswering, System: "You are careful.", SchemaName: "answer", Schema: testSchema,
		Messages: []Message{{Role: "user", Content: "Q?"}, {Role: "assistant", Content: "prev"}, {Role: "user", Content: "Q2?"}}, Temperature: 0.1, MaxTokens: 256}
}

func category(err error) ErrorCategory {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Category
	}
	return ""
}

type capture struct {
	path   string
	header http.Header
	body   map[string]any
}

func server(t *testing.T, c *capture, respond func(w http.ResponseWriter, body map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		c.path, c.header, c.body = r.URL.Path, r.Header.Clone(), body
		w.Header().Set("Content-Type", "application/json")
		respond(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAICompatibleContract(t *testing.T) {
	var c capture
	reply := `{"choices":[{"message":{"content":"{\"answer\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`
	srv := server(t, &c, func(w http.ResponseWriter, body map[string]any) {
		switch {
		case strings.HasSuffix(c.path, "/embeddings"):
			_, _ = io.WriteString(w, `{"data":[{"index":1,"embedding":[0,1,0]},{"index":0,"embedding":[1,0,0]}],"usage":{"prompt_tokens":4}}`)
		case strings.HasSuffix(c.path, "/audio/speech"):
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = io.WriteString(w, "ID3-bytes")
		default:
			_, _ = io.WriteString(w, reply)
		}
	})
	p := &Provider{Slug: "oa", Kind: "openai_compatible", BaseURL: srv.URL + "/v1", APIKey: "sk-test"}
	o := &OpenAICompatible{P: p, Client: srv.Client()}
	m := &Model{Name: "gpt-x", SupportsStructured: true}
	res, err := o.Generate(context.Background(), req(m))
	if err != nil || res.Text != `{"answer":"ok"}` || res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 3 {
		t.Fatalf("generate: %+v %v", res, err)
	}
	if c.path != "/v1/chat/completions" || c.header.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("request: %s %v", c.path, c.header)
	}
	msgs := c.body["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" || msgs[2].(map[string]any)["role"] != "assistant" {
		t.Fatalf("messages: %+v", msgs)
	}
	rf := c.body["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["strict"] != true || strings.Contains(mustJSON(js["schema"]), "maxLength") {
		t.Fatalf("structured output request (unsupported keywords must be stripped): %+v", rf)
	}

	// JSON-mode-only servers get the schema in the system prompt instead.
	m2 := &Model{Name: "local", SupportsJSONMode: true}
	if _, err := o.Generate(context.Background(), req(m2)); err != nil {
		t.Fatal(err)
	}
	if c.body["response_format"].(map[string]any)["type"] != "json_object" ||
		!strings.Contains(c.body["messages"].([]any)[0].(map[string]any)["content"].(string), "JSON schema") {
		t.Fatalf("json mode: %+v", c.body)
	}

	// Refusal, truncation and HTTP failures map to explicit categories.
	for _, tc := range []struct {
		reply  string
		status int
		want   ErrorCategory
	}{
		{`{"choices":[{"message":{"content":"","refusal":"no"},"finish_reason":"stop"}]}`, 200, ErrPermanent},
		{`{"choices":[{"message":{"content":"{\"ans"},"finish_reason":"length"}]}`, 200, ErrInvalid},
		{`{"error":{"message":"rate limited"}}`, 429, ErrTransient},
		{`{"error":{"message":"bad key"}}`, 401, ErrPermanent},
		{`{"error":{"message":"down"}}`, 503, ErrTransient},
		{`not json`, 200, ErrTransient},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.reply)
		}))
		o := &OpenAICompatible{P: &Provider{BaseURL: srv.URL, APIKey: "k"}, Client: srv.Client()}
		_, err := o.Generate(context.Background(), req(m))
		srv.Close()
		if category(err) != tc.want {
			t.Fatalf("reply %d %s: got %v want %s", tc.status, tc.reply, err, tc.want)
		}
	}

	// Embeddings are reordered by index; dimensions are validated.
	vecs, u, err := o.Embed(context.Background(), &Model{Name: "emb", EmbeddingDims: 3}, []string{"a", "b"})
	if err != nil || vecs[0][0] != 1 || vecs[1][1] != 1 || u.InputTokens != 4 {
		t.Fatalf("embed: %v %v", vecs, err)
	}
	if _, _, err := o.Embed(context.Background(), &Model{Name: "emb", EmbeddingDims: 8}, []string{"a", "b"}); category(err) != ErrPermanent {
		t.Fatalf("dimension mismatch should be permanent: %v", err)
	}
	sp, err := o.Speak(context.Background(), &Model{Name: "tts-1"}, SpeechRequest{Text: "Bună", Voice: "alloy"})
	if err != nil || string(sp.Audio) != "ID3-bytes" || sp.ContentType != "audio/mpeg" || c.body["voice"] != "alloy" || c.body["response_format"] != "mp3" {
		t.Fatalf("speak: %+v %v %+v", sp, err, c.body)
	}
}

func TestAnthropicContract(t *testing.T) {
	var c capture
	var reply string
	var status int
	srv := server(t, &c, func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	})
	msg := func(stop, text string) string {
		return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":` + mustJSON(text) +
			`}],"stop_reason":"` + stop + `","stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":7}}`
	}
	a := NewAnthropic(&Provider{Slug: "anthropic", Kind: "anthropic", BaseURL: srv.URL, APIKey: "sk-ant-test"}, srv.Client())
	m := &Model{Name: "claude-test", SupportsStructured: true}

	status, reply = 200, msg("end_turn", `{"answer":"ok"}`)
	res, err := a.Generate(context.Background(), req(m))
	if err != nil || res.Text != `{"answer":"ok"}` || res.Usage.InputTokens != 20 || res.Usage.OutputTokens != 7 {
		t.Fatalf("generate: %+v %v", res, err)
	}
	if c.path != "/v1/messages" || c.header.Get("X-Api-Key") != "sk-ant-test" || c.header.Get("Anthropic-Version") == "" {
		t.Fatalf("request: %s %v", c.path, c.header)
	}
	if c.body["model"] != "claude-test" || c.body["max_tokens"].(float64) != 256 {
		t.Fatalf("body: %+v", c.body)
	}
	sys := c.body["system"].([]any)[0].(map[string]any)
	if sys["text"] != "You are careful." {
		t.Fatalf("system: %+v", c.body["system"])
	}
	msgs := c.body["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("messages: %+v", msgs)
	}
	format := c.body["output_config"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"] == nil || strings.Contains(mustJSON(format["schema"]), "maxLength") {
		t.Fatalf("structured output: %+v", c.body["output_config"])
	}

	for _, tc := range []struct {
		status int
		reply  string
		want   ErrorCategory
	}{
		{200, msg("refusal", ""), ErrPermanent},
		{200, msg("max_tokens", `{"ans`), ErrInvalid},
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, ErrTransient},
		{529, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`, ErrTransient},
		{401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, ErrPermanent},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad schema"}}`, ErrPermanent},
	} {
		status, reply = tc.status, tc.reply
		_, err := a.Generate(context.Background(), req(m))
		if category(err) != tc.want {
			t.Fatalf("%d %s: got %v want %s", tc.status, tc.reply, err, tc.want)
		}
	}
}

func TestGeminiContract(t *testing.T) {
	var c capture
	var reply string
	srv := server(t, &c, func(w http.ResponseWriter, _ map[string]any) {
		if strings.HasSuffix(c.path, ":batchEmbedContents") {
			_, _ = io.WriteString(w, `{"embeddings":[{"values":[1,0]},{"values":[0,1]}]}`)
			return
		}
		_, _ = io.WriteString(w, reply)
	})
	g := &Gemini{P: &Provider{Slug: "gemini", Kind: "gemini", BaseURL: srv.URL + "/v1beta", APIKey: "AIza-test"}, Client: srv.Client()}
	m := &Model{Name: "gemini-test", SupportsStructured: true}
	reply = `{"candidates":[{"content":{"parts":[{"text":"{\"answer\":"},{"text":"\"ok\"}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":4}}`
	res, err := g.Generate(context.Background(), req(m))
	if err != nil || res.Text != `{"answer":"ok"}` || res.Usage.InputTokens != 9 {
		t.Fatalf("generate: %+v %v", res, err)
	}
	if c.path != "/v1beta/models/gemini-test:generateContent" || c.header.Get("X-Goog-Api-Key") != "AIza-test" {
		t.Fatalf("request: %s %v", c.path, c.header)
	}
	gen := c.body["generationConfig"].(map[string]any)
	if gen["responseMimeType"] != "application/json" || strings.Contains(mustJSON(gen["responseSchema"]), "additionalProperties") {
		t.Fatalf("gemini schema must drop additionalProperties: %+v", gen)
	}
	if roles := c.body["contents"].([]any); roles[1].(map[string]any)["role"] != "model" {
		t.Fatalf("assistant turns map to role model: %+v", roles)
	}
	for _, tc := range []struct {
		reply string
		want  ErrorCategory
	}{
		{`{"promptFeedback":{"blockReason":"SAFETY"}}`, ErrPermanent},
		{`{"candidates":[{"content":{"parts":[{"text":"{"}]},"finishReason":"MAX_TOKENS"}]}`, ErrInvalid},
		{`{"candidates":[]}`, ErrTransient},
	} {
		reply = tc.reply
		if _, err := g.Generate(context.Background(), req(m)); category(err) != tc.want {
			t.Fatalf("%s: got %v want %s", tc.reply, err, tc.want)
		}
	}
	vecs, _, err := g.Embed(context.Background(), &Model{Name: "text-embedding", EmbeddingDims: 2}, []string{"a", "b"})
	if err != nil || vecs[1][1] != 1 || c.body["requests"].([]any)[0].(map[string]any)["outputDimensionality"].(float64) != 2 {
		t.Fatalf("embed: %v %v %+v", vecs, err, c.body)
	}
}

func TestOpenAICompatibleRerankContract(t *testing.T) {
	var c capture
	srv := server(t, &c, func(w http.ResponseWriter, body map[string]any) {
		_, _ = io.WriteString(w, `{"model":"r","results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}],"usage":{"total_tokens":12}}`)
	})
	o := &OpenAICompatible{P: &Provider{BaseURL: srv.URL + "/v1"}, Client: srv.Client()}
	scores, u, err := o.Rerank(context.Background(), &Model{Name: "multilingual-reranker"}, "q", []string{"a", "b"})
	if err != nil || scores[0] != 0.1 || scores[1] != 0.9 || u.InputTokens != 12 {
		t.Fatalf("rerank: %v %v %v", scores, u, err)
	}
	if c.path != "/v1/rerank" || c.body["query"] != "q" || len(c.body["documents"].([]any)) != 2 || c.body["model"] != "multilingual-reranker" {
		t.Fatalf("request: %s %+v", c.path, c.body)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"index":5,"relevance_score":1}]}`)
	}))
	defer bad.Close()
	o2 := &OpenAICompatible{P: &Provider{BaseURL: bad.URL}, Client: bad.Client()}
	if _, _, err := o2.Rerank(context.Background(), &Model{Name: "x"}, "q", []string{"a"}); category(err) != ErrTransient {
		t.Fatalf("out-of-range index must be rejected: %v", err)
	}
}
