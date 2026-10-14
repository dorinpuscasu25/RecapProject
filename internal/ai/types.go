// Package ai is the provider-agnostic AI gateway. Business code calls task-level operations
// (GenerateStructured, GenerateText, EmbedTexts, Rerank, TextToSpeech); the gateway resolves the
// database-managed route for the task (primary + fallback chain, prompt version, budgets),
// invokes provider adapters, validates outputs and records every execution.
//
// No vendor SDK type escapes the adapter files.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

type Task string

const (
	TaskArticleExtraction  Task = "article_extraction"
	TaskClassification     Task = "category_classification"
	TaskEntityExtraction   Task = "entity_extraction"
	TaskClaimExtraction    Task = "claim_extraction"
	TaskEventEquivalence   Task = "event_equivalence"
	TaskEventSynthesis     Task = "event_synthesis"
	TaskNoveltyDetection   Task = "novelty_detection"
	TaskEvidenceAssessment Task = "evidence_assessment"
	TaskExplanation        Task = "explanation"
	TaskChatAnswering      Task = "chat_answering"
	TaskReranking          Task = "reranking"
	TaskEmbeddings         Task = "embeddings"
	TaskScriptGeneration   Task = "script_generation"
	TaskTTS                Task = "tts"
)

// AllTasks lists configurable tasks (admin UI / validation).
var AllTasks = []Task{TaskArticleExtraction, TaskClassification, TaskEntityExtraction, TaskClaimExtraction, TaskEventEquivalence,
	TaskEventSynthesis, TaskNoveltyDetection, TaskEvidenceAssessment, TaskExplanation, TaskChatAnswering, TaskReranking,
	TaskEmbeddings, TaskScriptGeneration, TaskTTS}

func ValidTask(t string) bool {
	for _, x := range AllTasks {
		if string(x) == t {
			return true
		}
	}
	return false
}

type Message struct {
	Role    string `json:"role"` // user | assistant
	Content string `json:"content"`
}

type Provider struct {
	ID             uuid.UUID
	Slug           string
	Kind           string // openai_compatible | anthropic | gemini | heuristic
	BaseURL        string
	APIKey         string // resolved at load time; never logged
	Enabled        bool
	RateLimitRPM   int
	MaxConcurrency int
	TimeoutMS      int
	ExtraHeaders   map[string]string
}

type Model struct {
	ID                 uuid.UUID
	ProviderID         uuid.UUID
	Slug               string
	Name               string
	Version            string
	Kind               string // chat | embedding | rerank | tts | stt
	SupportsStructured bool
	SupportsJSONMode   bool
	Languages          []string
	ContextWindow      int
	MaxOutputTokens    int
	EmbeddingDims      int
	InputPricePerMTok  float64
	OutputPricePerMTok float64
	PricePerMChar      float64
	Enabled            bool
	Metadata           map[string]any
	Provider           *Provider
}

// ProviderMeta returns free-form adapter metadata (never nil).
func (m *Model) ProviderMeta() map[string]any {
	if m.Metadata == nil {
		return map[string]any{}
	}
	return m.Metadata
}

// Ref returns a stable provenance string, e.g. "heuristic/local-rules@1".
func (m *Model) Ref() string {
	v := m.Version
	if v == "" {
		v = "unversioned"
	}
	return m.Provider.Slug + "/" + m.Slug + "@" + v
}

// ProviderRequest is what adapters receive.
type ProviderRequest struct {
	Model       *Model
	Task        Task
	System      string
	Messages    []Message
	Schema      map[string]any // JSON schema for structured output (nil for text)
	SchemaName  string
	Temperature float32
	MaxTokens   int
	Payload     any // typed task input for the heuristic provider
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	Characters   int
}

type ProviderResponse struct {
	Text  string
	Usage Usage
}

// ChatProvider generates text / structured JSON.
type ChatProvider interface {
	Generate(ctx context.Context, req ProviderRequest) (*ProviderResponse, error)
}

// EmbeddingProvider produces vectors.
type EmbeddingProvider interface {
	Embed(ctx context.Context, model *Model, texts []string) ([][]float32, Usage, error)
}

// RerankProvider scores documents against a query (cross-encoder). Higher is more relevant.
type RerankProvider interface {
	Rerank(ctx context.Context, model *Model, query string, docs []string) ([]float64, Usage, error)
}

// SpeechProvider synthesizes audio.
type SpeechProvider interface {
	Speak(ctx context.Context, model *Model, req SpeechRequest) (*SpeechResult, error)
}

type SpeechRequest struct {
	Text     string
	Voice    string
	Language string
	Format   string // mp3 | opus | wav
}

type SpeechResult struct {
	Audio       []byte
	ContentType string
	DurationMS  int
	Usage       Usage
}

// ---------- Errors ----------

// ErrorCategory classifies provider failures for retry/fallback decisions and accounting.
type ErrorCategory string

const (
	ErrTransient     ErrorCategory = "transient"      // 429/5xx/timeouts: retry, then fall back
	ErrPermanent     ErrorCategory = "permanent"      // 4xx config/auth errors: fall back, do not retry
	ErrInvalid       ErrorCategory = "invalid_output" // schema/semantic validation failure: retry boundedly
	ErrUnsupported   ErrorCategory = "unsupported"    // capability missing: skip model
	ErrBudget        ErrorCategory = "budget"         // route budget exhausted
	ErrNotConfigured ErrorCategory = "not_configured"
)

type ProviderError struct {
	Category ErrorCategory
	Status   int
	Msg      string
}

func (e *ProviderError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s (status %d): %s", e.Category, e.Status, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Category, e.Msg)
}

func categoryOf(err error) ErrorCategory {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Category
	}
	return ErrTransient
}

// ErrDeferred is returned when no model in the chain could complete the task and the route is
// configured to defer: callers must retry later and must NOT fabricate output.
var ErrDeferred = errors.New("ai task deferred: no provider in the route could complete it")

// ErrNoRoute is returned when a task has no enabled route.
var ErrNoRoute = errors.New("ai task has no enabled route")

// Execution summarizes one gateway call for provenance.
type Execution struct {
	ID            uuid.UUID  `json:"id"`
	Task          Task       `json:"task"`
	ModelRef      string     `json:"model_ref"`
	PromptVersion *uuid.UUID `json:"prompt_version_id,omitempty"`
	IsFallback    bool       `json:"is_fallback"`
	Attempts      int        `json:"attempts"`
	LatencyMS     int        `json:"latency_ms"`
	CostUSD       float64    `json:"cost_usd"`
}

// Validator lets a structured output perform semantic validation (offsets, referential integrity).
type Validator interface {
	Validate(input any) error
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
