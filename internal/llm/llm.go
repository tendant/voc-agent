// Package llm talks to language models. Ollama is the local default; a cloud
// client arrives in M3 behind the same interface.
package llm

import (
	"context"
	"encoding/json"
)

type Message struct {
	Role      string     `json:"role"` // system | user | assistant | tool
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"` // assistant asking to run tools
	ToolName  string     `json:"tool_name,omitempty"`  // on role=tool: which tool this answers
}

type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // a JSON object
}

// Tool describes a function the model may call.
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function FunctionSpec `json:"function"`
}

type FunctionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema object
}

type Request struct {
	Messages []Message
	Tools    []Tool // may be empty
}

// Chat streams a completion. onDelta receives text as it is generated and may
// be nil. The returned message holds the full text and any tool calls.
type Chat interface {
	Chat(ctx context.Context, req Request, onDelta func(string)) (Message, error)
}
