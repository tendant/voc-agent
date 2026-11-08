package cloud

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// FakeAPI is a stand-in Messages API for tests and offline demos. It streams
// Reply as the answer and records every request body it receives.
type FakeAPI struct {
	Reply string
	mu    sync.Mutex
	got   [][]byte
	srv   *httptest.Server
}

func NewFakeAPI(reply string) *FakeAPI {
	f := &FakeAPI{Reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *FakeAPI) URL() string { return f.srv.URL }
func (f *FakeAPI) Close()      { f.srv.Close() }

// Requests returns the bodies received so far.
func (f *FakeAPI) Requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.got...)
}

func (f *FakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.got = append(f.got, b)
	f.mu.Unlock()
	var req struct {
		Model string `json:"model"`
	}
	json.Unmarshal(b, &req)
	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(name string, v any) {
		d, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, d)
	}
	ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_fake", "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"stop_reason": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 0}}})
	ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": f.Reply}})
	ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 5}})
	ev("message_stop", map[string]any{"type": "message_stop"})
}
