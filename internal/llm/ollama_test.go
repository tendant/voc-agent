package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOllamaStream(t *testing.T) {
	var got ollamaReq
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		for _, p := range []string{"Hello", " there", "."} {
			fmt.Fprintf(w, `{"message":{"role":"assistant","content":%q},"done":false}`+"\n", p)
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":""},"done":true}`)
	}))
	defer ts.Close()

	o := &Ollama{BaseURL: ts.URL, Model: "m", Temperature: 0.3}
	var deltas []string
	full, err := o.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if full != "Hello there." || len(deltas) != 3 {
		t.Fatalf("full=%q deltas=%q", full, deltas)
	}
	if got.Think || !got.Stream || got.Model != "m" || got.Options["temperature"] != 0.3 {
		t.Fatalf("bad request: %+v", got)
	}
}

func TestOllamaErrorLine(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"error":"model not found"}`)
	}))
	defer ts.Close()
	_, err := (&Ollama{BaseURL: ts.URL, Model: "m"}).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("err = %v", err)
	}
}

// TestOllamaLive hits a real local Ollama; run with VOC_LIVE=1.
func TestOllamaLive(t *testing.T) {
	if os.Getenv("VOC_LIVE") == "" {
		t.Skip("set VOC_LIVE=1 to run against local Ollama")
	}
	o := &Ollama{BaseURL: "http://127.0.0.1:11434", Model: "qwen3:latest", KeepAlive: "30m"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := o.Warm(ctx); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	var ttft time.Duration
	full, err := o.Chat(ctx, []Message{{Role: "user", Content: "Say hello in five words."}}, func(string) {
		if ttft == 0 {
			ttft = time.Since(t0)
		}
	})
	if err != nil || full == "" {
		t.Fatalf("%q %v", full, err)
	}
	t.Logf("ttft=%v total=%v reply=%q", ttft, time.Since(t0), full)
}
