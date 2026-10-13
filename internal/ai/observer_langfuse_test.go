package ai

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLangfuseEventsCarryNoContent(t *testing.T) {
	l := &Langfuse{}
	rec := ExecutionRecord{ID: uuid.New(), Task: TaskChatAnswering, Mode: "production", ModelRef: "anthropic/claude", Status: "failed",
		LatencyMS: 1200, InputTokens: 100, OutputTokens: 20, CostUSD: 0.01, ErrorCat: "transient", SubjectType: "chat", SubjectID: "s1"}
	ev := l.Events([]ExecutionRecord{rec}, time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	if len(ev) != 2 || ev[0]["type"] != "trace-create" || ev[1]["type"] != "generation-create" {
		t.Fatalf("events: %+v", ev)
	}
	b := ev[1]["body"].(map[string]any)
	if b["level"] != "ERROR" || b["model"] != "anthropic/claude" || b["usage"].(map[string]any)["input"] != 100 {
		t.Fatalf("generation: %+v", b)
	}
	if !strings.HasPrefix(b["startTime"].(string), "2026-10-13T11:59:58.8") {
		t.Fatalf("start time: %v", b["startTime"])
	}
	for _, k := range []string{"input", "output", "prompt", "completion"} {
		if _, ok := b[k]; ok {
			t.Fatalf("content field %q must not be exported", k)
		}
	}
}
