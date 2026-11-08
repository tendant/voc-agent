// Package cloud calls Claude for escalated questions. Every request passes
// through a middleware that sees the exact HTTP body leaving the machine: it
// verifies the body matches what the user approved, re-runs the local_only
// leak check, and reports the bytes for the audit log.
package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"
)

const DefaultModel = "claude-opus-5-5"

type Claude struct {
	Model  string
	Effort anthropic.BetaOutputConfigEffort
	client anthropic.Client
	host   string
}

// New creates a client. Credentials come from the environment the usual way
// (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or an `ant auth login` profile).
func New(model string, opts ...option.RequestOption) *Claude {
	if model == "" {
		model = DefaultModel
	}
	c := &Claude{Model: model, Effort: anthropic.BetaOutputConfigEffortMedium, host: "api.anthropic.com"}
	opts = append([]option.RequestOption{option.WithMiddleware(verify)}, opts...)
	c.client = anthropic.NewClient(opts...)
	return c
}

// Destination is shown to the user and recorded in the audit log.
func (c *Claude) Destination() string { return c.host }

// WithHost overrides the displayed destination (tests, proxies).
func (c *Claude) WithHost(h string) *Claude { c.host = h; return c }

// Params builds the request for a policy-approved system prompt and user text.
func (c *Claude) Params(system, user string) anthropic.BetaMessageNewParams {
	return anthropic.BetaMessageNewParams{
		Model:     c.Model,
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
		// Thinking is adaptive by default on this model; effort sets its depth.
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: c.Effort},
		// If a safety classifier declines, the server re-serves the request on
		// a fallback model chosen by refusal category.
		Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}
}

// Body renders params as the JSON request body the SDK will send (minus the
// "stream" flag the streaming call adds). This is what the user approves.
func Body(p anthropic.BetaMessageNewParams) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Guard inspects the outbound body; returning an error aborts the request.
type Guard func(body []byte) error

// Sent describes what actually went over the wire.
type Sent struct {
	Body   []byte
	SHA256 string
}

type ctxKey struct{}

type sendCtx struct {
	approved []byte
	guard    Guard
	sent     *Sent
}

// ErrMismatch means the SDK would have sent something other than what the
// user approved.
var ErrMismatch = errors.New("cloud: outbound request differs from the approved payload")

// verify is the SDK middleware enforcing approval and the leak check on the
// real bytes, per attempt (retries included).
func verify(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	sc, _ := req.Context().Value(ctxKey{}).(*sendCtx)
	if sc == nil {
		return nil, errors.New("cloud: request without approval context refused")
	}
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	}
	if !sameRequest(body, sc.approved) {
		return nil, ErrMismatch
	}
	if sc.guard != nil {
		if err := sc.guard(body); err != nil {
			return nil, err
		}
	}
	h := sha256.Sum256(body)
	*sc.sent = Sent{Body: body, SHA256: hex.EncodeToString(h[:])}
	return next(req)
}

// sameRequest compares JSON bodies semantically, ignoring "stream".
func sameRequest(actual, approved []byte) bool {
	var a, b map[string]any
	if json.Unmarshal(actual, &a) != nil || json.Unmarshal(approved, &b) != nil {
		return false
	}
	delete(a, "stream")
	delete(b, "stream")
	return reflect.DeepEqual(a, b)
}

// Answer is the cloud model's reply.
type Answer struct {
	Text       string
	Model      string // model that served it (differs if a fallback ran)
	StopReason string
}

// Ask sends params, which must serialize to approved, after guard passes on
// the actual bytes. sent is filled in whenever bytes left the machine, even
// if the call later fails.
func (c *Claude) Ask(ctx context.Context, p anthropic.BetaMessageNewParams, approved []byte, guard Guard, sent *Sent) (Answer, error) {
	ctx = context.WithValue(ctx, ctxKey{}, &sendCtx{approved: approved, guard: guard, sent: sent})
	stream := c.client.Beta.Messages.NewStreaming(ctx, p)
	defer stream.Close()
	var msg anthropic.BetaMessage
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return Answer{}, err
		}
	}
	if err := stream.Err(); err != nil {
		return Answer{}, err
	}
	ans := Answer{Model: string(msg.Model), StopReason: string(msg.StopReason)}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return ans, fmt.Errorf("cloud model declined the request")
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	ans.Text = strings.TrimSpace(b.String())
	if ans.Text == "" {
		return ans, fmt.Errorf("cloud model returned no text (stop reason %s)", msg.StopReason)
	}
	return ans, nil
}
