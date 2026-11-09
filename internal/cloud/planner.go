package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/policy"
)

// Planner runs goal mode's planning, step details and report on Claude. It
// implements llm.Chat for the single-turn, structured requests goal mode
// makes. There is no per-request approval (the user chose that), but every
// request: has personal identifiers redacted, is checked at send time for
// local_only text (Guard), and is written to the audit log (Audit).
type Planner struct {
	C     *Claude
	Guard Guard
	// Audit records each request: the exact bytes sent and the reply.
	Audit func(sent Sent, reply string, err error)
}

func (p *Planner) Chat(ctx context.Context, req llm.Request, onDelta func(string)) (llm.Message, error) {
	var system, user []string
	for _, m := range req.Messages {
		text, _ := policy.Redact(m.Content)
		switch m.Role {
		case "system":
			system = append(system, text)
		case "user", "tool":
			user = append(user, text)
		case "assistant":
			user = append(user, "(Earlier reply: "+text+")")
		}
	}
	params := p.C.Params(strings.Join(system, "\n\n"), strings.Join(user, "\n\n"))
	// Planning thinks harder; filling in details and reporting are simple.
	params.OutputConfig.Effort = anthropic.BetaOutputConfigEffortLow
	if req.Think {
		params.OutputConfig.Effort = anthropic.BetaOutputConfigEffortMedium
	}
	if len(req.Format) > 0 {
		schema, err := sanitizeSchema(req.Format)
		if err != nil {
			return llm.Message{}, err
		}
		params.OutputConfig.Format = anthropic.BetaJSONOutputFormatParam{Schema: schema}
	}
	var sent Sent
	ans, err := p.C.Ask(ctx, params, nil, p.Guard, &sent)
	if p.Audit != nil && (sent.Body != nil || err != nil) {
		p.Audit(sent, ans.Text, err)
	}
	if err != nil {
		return llm.Message{}, err
	}
	if onDelta != nil {
		onDelta(ans.Text)
	}
	return llm.Message{Role: "assistant", Content: ans.Text}, nil
}

// sanitizeSchema adapts a JSON Schema to what structured outputs accept:
// every object gets additionalProperties:false, and numeric, length and
// array-size constraints are dropped (goal mode enforces those in code).
func sanitizeSchema(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("bad schema: %w", err)
	}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for _, k := range []string{"maxItems", "minItems", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
				"multipleOf", "minLength", "maxLength", "uniqueItems", "pattern"} {
				delete(t, k)
			}
			if t["type"] == "object" {
				t["additionalProperties"] = false
				if _, ok := t["properties"]; !ok {
					t["properties"] = map[string]any{}
				}
			}
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	return json.Marshal(v)
}
