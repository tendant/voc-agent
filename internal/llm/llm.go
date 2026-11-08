// Package llm talks to language models. Ollama is the local default; a cloud
// client arrives in M3 behind the same interface.
package llm

import "context"

type Message struct {
	Role    string `json:"role"` // system | user | assistant | tool
	Content string `json:"content"`
}

// Chat streams a completion. onDelta receives text as it is generated and may
// be nil. It returns the full reply.
type Chat interface {
	Chat(ctx context.Context, msgs []Message, onDelta func(string)) (string, error)
}
