package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

func client(f *FakeAPI) *Claude {
	return New("", option.WithBaseURL(f.URL()), option.WithAPIKey("test"), option.WithMaxRetries(0))
}

func TestAskSendsExactlyTheApprovedBody(t *testing.T) {
	f := NewFakeAPI("Use a canary and keep the old image warm.")
	defer f.Close()
	c := client(f)
	p := c.Params("sys", "Is my rollout risky?")
	approved, err := Body(p)
	if err != nil {
		t.Fatal(err)
	}
	var sent Sent
	ans, err := c.Ask(context.Background(), p, approved, nil, &sent)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Text != "Use a canary and keep the old image warm." || ans.Model != DefaultModel {
		t.Fatalf("answer %+v", ans)
	}
	reqs := f.Requests()
	if len(reqs) != 1 || string(reqs[0]) != string(sent.Body) || sent.SHA256 == "" {
		t.Fatalf("audit bytes differ from what the server received")
	}
	var body map[string]any
	json.Unmarshal(sent.Body, &body)
	if body["model"] != "claude-opus-5-5" || body["fallbacks"] != "default" || body["stream"] != true {
		t.Fatalf("body: %s", sent.Body)
	}
	if oc := body["output_config"].(map[string]any); oc["effort"] != "medium" {
		t.Fatalf("effort: %v", oc)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatal("thinking must be omitted (adaptive by default)")
	}
}

func TestAskRefusesMismatchAndGuard(t *testing.T) {
	f := NewFakeAPI("x")
	defer f.Close()
	c := client(f)
	approved, _ := Body(c.Params("sys", "approved question"))

	var sent Sent
	_, err := c.Ask(context.Background(), c.Params("sys", "a DIFFERENT question"), approved, nil, &sent)
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
	p := c.Params("sys", "approved question")
	_, err = c.Ask(context.Background(), p, approved, func(b []byte) error {
		if strings.Contains(string(b), "approved") {
			return errors.New("guard says no")
		}
		return nil
	}, &sent)
	if err == nil || !strings.Contains(err.Error(), "guard says no") {
		t.Fatalf("guard not enforced: %v", err)
	}
	if len(f.Requests()) != 0 || sent.Body != nil {
		t.Fatal("blocked requests must not reach the network")
	}
}
