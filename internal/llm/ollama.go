package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Ollama is a streaming client for Ollama's /api/chat.
type Ollama struct {
	BaseURL     string  // e.g. http://127.0.0.1:11434
	Model       string  // e.g. qwen3:latest
	Temperature float64 // 0 uses the model default
	KeepAlive   string  // how long Ollama keeps the model loaded, e.g. "30m"
	HTTP        *http.Client
}

type ollamaReq struct {
	Model     string         `json:"model"`
	Messages  []Message      `json:"messages"`
	Tools     []Tool         `json:"tools,omitempty"`
	Stream    bool           `json:"stream"`
	Think     bool           `json:"think"` // false: skip Qwen3 reasoning, too slow for voice
	KeepAlive string         `json:"keep_alive,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
}

type ollamaChunk struct {
	Message Message `json:"message"`
	Done    bool    `json:"done"`
	Error   string  `json:"error"`
}

func (o *Ollama) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return http.DefaultClient
}

func (o *Ollama) post(ctx context.Context, body ollamaReq) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w (is `ollama serve` running?)", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("ollama: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return resp, nil
}

func (o *Ollama) Chat(ctx context.Context, r Request, onDelta func(string)) (Message, error) {
	req := ollamaReq{Model: o.Model, Messages: r.Messages, Tools: r.Tools, Stream: true, KeepAlive: o.KeepAlive}
	if req.Messages == nil {
		req.Messages = []Message{}
	}
	if o.Temperature > 0 {
		req.Options = map[string]any{"temperature": o.Temperature}
	}
	resp, err := o.post(ctx, req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()

	var full strings.Builder
	var calls []ToolCall
	result := func() Message {
		return Message{Role: "assistant", Content: full.String(), ToolCalls: calls}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var c ollamaChunk
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return result(), fmt.Errorf("ollama: bad stream line: %w", err)
		}
		if c.Error != "" {
			return result(), fmt.Errorf("ollama: %s", c.Error)
		}
		calls = append(calls, c.Message.ToolCalls...)
		if d := c.Message.Content; d != "" {
			full.WriteString(d)
			if onDelta != nil {
				onDelta(d)
			}
		}
		if c.Done {
			return result(), nil
		}
	}
	if err := sc.Err(); err != nil {
		return result(), err
	}
	if ctx.Err() != nil {
		return result(), ctx.Err()
	}
	return result(), fmt.Errorf("ollama: stream ended early")
}

// Warm loads the model into memory so the first real turn doesn't pay the
// load time (~2 s for an 8B model).
func (o *Ollama) Warm(ctx context.Context) error {
	resp, err := o.post(ctx, ollamaReq{Model: o.Model, Messages: []Message{}, KeepAlive: o.KeepAlive})
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}
