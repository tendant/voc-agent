// Package tools defines the typed actions the agent can take. Each tool's
// arguments are a Go struct; its JSON Schema is generated from the struct so
// the model's view and the validation can't drift apart.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/tendant/voc-agent/internal/llm"
)

// Call carries per-invocation context.
type Call struct {
	// TurnKey is unique per user turn (random, survives nothing). Side-effecting
	// tools derive idempotency keys from it so a retried call in the same turn
	// doesn't act twice.
	TurnKey string
	// Approve asks the user to allow an action and blocks until they answer.
	// Nil means approvals are unavailable, which tools must treat as "no".
	Approve func(ctx context.Context, req ApprovalRequest) (bool, error)
}

// ApprovalRequest is shown to the user before data leaves the device
// (Kind "cloud", the default) or before a multi-step plan runs (Kind "plan").
type ApprovalRequest struct {
	Kind        string          `json:"kind,omitempty"`
	Prompt      string          `json:"prompt,omitempty"` // spoken when asking
	Plan        json.RawMessage `json:"plan,omitempty"`   // Kind "plan": summary and steps
	ID          string          `json:"id"`
	Destination string          `json:"destination"`
	Model       string          `json:"model"`
	Reason      string          `json:"reason"`
	Payload     json.RawMessage `json:"payload"` // the exact request body
	Policy      json.RawMessage `json:"policy"`  // included/excluded sources
}

// Sayer is implemented by tool results that should be spoken verbatim as the
// assistant's reply (e.g. a cloud answer), ending the turn.
type Sayer interface{ Say() string }

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	run         func(ctx context.Context, c Call, args json.RawMessage) (any, error)
	// enabled, if set, hides the tool when it can't be used right now (e.g.
	// page actions while no browser window is open), keeping prompts short.
	enabled func() bool
	// readOnly tools only look things up; goal mode runs them without asking.
	readOnly bool
}

// ReadOnly marks a tool as side-effect free.
func (t Tool) ReadOnly() Tool {
	t.readOnly = true
	return t
}

// Info describes a registered tool for planners.
type Info struct {
	Name, Description string
	Schema            json.RawMessage
	ReadOnly          bool
}

// Available lists the tools usable right now.
func (r *Registry) Available() []Info {
	var out []Info
	for i := range r.tools {
		if t := &r.tools[i]; t.available() {
			out = append(out, Info{t.Name, t.Description, t.Schema, t.readOnly})
		}
	}
	return out
}

// When returns a copy of t that is offered only while enabled reports true.
func (t Tool) When(enabled func() bool) Tool {
	t.enabled = enabled
	return t
}

func (t *Tool) available() bool { return t.enabled == nil || t.enabled() }

// Typed builds a tool from a function taking a struct of arguments. Struct
// fields use `json:"name"` (",omitempty" makes it optional) and
// `desc:"..."` for the description shown to the model.
func Typed[A any](name, desc string, fn func(ctx context.Context, c Call, args A) (any, error)) Tool {
	var zero A
	return Tool{
		Name: name, Description: desc, Schema: schemaFor(reflect.TypeOf(zero)),
		run: func(ctx context.Context, c Call, raw json.RawMessage) (any, error) {
			var a A
			if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
				raw = []byte("{}")
			}
			// Some models send arguments as a JSON-encoded string.
			var s string
			if json.Unmarshal(raw, &s) == nil {
				raw = []byte(s)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&a); err != nil {
				return nil, fmt.Errorf("invalid arguments: %v", err)
			}
			if err := checkRequired(reflect.ValueOf(a)); err != nil {
				return nil, err
			}
			return fn(ctx, c, a)
		},
	}
}

func schemaFor(t reflect.Type) json.RawMessage {
	props := map[string]any{}
	required := []string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, opt := jsonName(f)
		if name == "" {
			continue
		}
		p := map[string]any{"type": jsonType(f.Type.Kind())}
		if d := f.Tag.Get("desc"); d != "" {
			p["description"] = d
		}
		if e := f.Tag.Get("enum"); e != "" {
			p["enum"] = strings.Split(e, ",")
		}
		props[name] = p
		if !opt {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	b, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "required": required})
	return b
}

func jsonName(f reflect.StructField) (name string, optional bool) {
	tag := f.Tag.Get("json")
	if tag == "-" || !f.IsExported() {
		return "", false
	}
	name, rest, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, strings.Contains(rest, "omitempty")
}

func jsonType(k reflect.Kind) string {
	switch k {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	}
	return "object"
}

func checkRequired(v reflect.Value) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name, opt := jsonName(t.Field(i))
		if name == "" || opt {
			continue
		}
		if fv := v.Field(i); fv.IsZero() || fv.Kind() == reflect.String && strings.TrimSpace(fv.String()) == "" {
			return fmt.Errorf("missing required argument %q", name)
		}
	}
	return nil
}

// Registry holds the tools offered to the model.
type Registry struct {
	tools []Tool
	Log   func(c Call, name string, args, result json.RawMessage, ok bool, d time.Duration)
}

func NewRegistry(ts ...Tool) *Registry { return &Registry{tools: ts} }

// Specs describes the tools available right now.
func (r *Registry) Specs() []llm.Tool {
	out := make([]llm.Tool, 0, len(r.tools))
	for i := range r.tools {
		if t := &r.tools[i]; t.available() {
			out = append(out, llm.Tool{Type: "function", Function: llm.FunctionSpec{Name: t.Name, Description: t.Description, Parameters: t.Schema}})
		}
	}
	return out
}

// Result is what a tool call produced, as shown to the model and the UI.
type Result struct {
	OK   bool            `json:"ok"`
	JSON json.RawMessage `json:"result"` // the tool's result, or {"error": ...}
	MS   int             `json:"ms"`
	Say  string          `json:"-"` // speak this verbatim and end the turn
	// Untrusted means the result carries third-party content (a web page).
	Untrusted bool `json:"-"`
}

// Run executes a tool call. Failures (unknown tool, bad arguments, tool
// errors) are returned as {"error": ...} results rather than Go errors, so
// the model sees them and can correct itself or tell the user.
func (r *Registry) Run(ctx context.Context, c Call, name string, args json.RawMessage) Result {
	start := time.Now()
	var (
		out any
		err error
	)
	if t := r.find(name); t == nil {
		err = fmt.Errorf("unknown tool %q", name)
	} else if !t.available() {
		err = fmt.Errorf("%s isn't available right now", name)
	} else {
		out, err = t.run(ctx, c, args)
	}
	res := Result{OK: err == nil, MS: int(time.Since(start).Milliseconds())}
	if err != nil {
		res.JSON, _ = json.Marshal(map[string]string{"error": err.Error()})
	} else if res.JSON, err = json.Marshal(out); err != nil {
		res.OK = false
		res.JSON, _ = json.Marshal(map[string]string{"error": "result not serializable: " + err.Error()})
	} else {
		if sy, ok := out.(Sayer); ok {
			res.Say = sy.Say()
		}
		if u, ok := out.(Untrusted); ok {
			res.Untrusted = u.Untrusted()
		}
	}
	if r.Log != nil {
		r.Log(c, name, args, res.JSON, res.OK, time.Since(start))
	}
	return res
}

// Has reports whether a tool is registered and available right now.
func (r *Registry) Has(name string) bool {
	t := r.find(name)
	return t != nil && t.available()
}

func (r *Registry) find(name string) *Tool {
	for i := range r.tools {
		if r.tools[i].Name == name {
			return &r.tools[i]
		}
	}
	return nil
}
