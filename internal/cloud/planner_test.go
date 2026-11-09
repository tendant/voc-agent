package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tendant/voc-agent/internal/llm"
)

func TestPlannerStructuredRedactedGuardedAudited(t *testing.T) {
	f := NewFakeAPI("")
	f.Replies = []string{`{"can_do":true,"summary":"x","steps":[]}`}
	defer f.Close()
	var audited []Sent
	p := &Planner{C: client(f), Audit: func(s Sent, reply string, err error) { audited = append(audited, s) }}
	m, err := p.Chat(context.Background(), llm.Request{Think: true,
		Format:   json.RawMessage(`{"type":"object","properties":{"steps":{"type":"array","maxItems":6,"items":{"type":"object","properties":{"n":{"type":"integer","minimum":1}}}}}}`),
		Messages: []llm.Message{{Role: "system", Content: "plan"}, {Role: "user", Content: "Goal: email priya.k@example.com"}}}, nil)
	if err != nil || m.Content != `{"can_do":true,"summary":"x","steps":[]}` {
		t.Fatalf("%+v %v", m, err)
	}
	body := string(f.Requests()[0])
	if strings.Contains(body, "priya.k@example.com") || !strings.Contains(body, "[EMAIL]") {
		t.Fatalf("not redacted: %s", body)
	}
	var b map[string]any
	json.Unmarshal(f.Requests()[0], &b)
	oc := b["output_config"].(map[string]any)
	schema, _ := json.Marshal(oc["format"])
	if oc["effort"] != "medium" || strings.Contains(string(schema), "maxItems") || strings.Contains(string(schema), "minimum") ||
		strings.Count(string(schema), `"additionalProperties":false`) != 2 {
		t.Fatalf("output_config: %s / %s", oc["effort"], schema)
	}
	if len(audited) != 1 || string(audited[0].Body) != body {
		t.Fatal("audit must hold the exact bytes sent")
	}
}

func TestPlannerGuardBlocks(t *testing.T) {
	f := NewFakeAPI("x")
	defer f.Close()
	p := &Planner{C: client(f), Guard: func(b []byte) error {
		if strings.Contains(string(b), "knee surgery") {
			return errors.New("local-only")
		}
		return nil
	}}
	_, err := p.Chat(context.Background(), llm.Request{Messages: []llm.Message{{Role: "user", Content: "my knee surgery recovery"}}}, nil)
	if err == nil || len(f.Requests()) != 0 {
		t.Fatalf("guard didn't block: %v, %d requests", err, len(f.Requests()))
	}
}
