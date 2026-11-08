package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tendant/voc-agent/internal/store"
)

func TestSchemaAndValidation(t *testing.T) {
	type args struct {
		Q string `json:"q" desc:"query"`
		N int    `json:"n,omitempty"`
	}
	tool := Typed("t", "d", func(_ context.Context, _ Call, a args) (any, error) { return a.Q + strings.Repeat("!", a.N), nil })
	var s map[string]any
	json.Unmarshal(tool.Schema, &s)
	if req := s["required"].([]any); len(req) != 1 || req[0] != "q" {
		t.Fatalf("schema: %s", tool.Schema)
	}
	r := NewRegistry(tool)
	cases := map[string]struct {
		args string
		ok   bool
		want string
	}{
		"ok":             {`{"q":"hi","n":2}`, true, `"hi!!"`},
		"string-encoded": {`"{\"q\":\"hi\"}"`, true, `"hi"`},
		"missing":        {`{}`, false, "missing required argument"},
		"blank":          {`{"q":"  "}`, false, "missing required argument"},
		"unknown field":  {`{"q":"x","zz":1}`, false, "unknown field"},
		"bad json":       {`{"q":`, false, "invalid arguments"},
	}
	for name, c := range cases {
		res := r.Run(context.Background(), Call{}, "t", json.RawMessage(c.args))
		if res.OK != c.ok || !strings.Contains(string(res.JSON), c.want) {
			t.Errorf("%s: %+v %s", name, res, res.JSON)
		}
	}
	if res := r.Run(context.Background(), Call{}, "nope", nil); res.OK || !strings.Contains(string(res.JSON), "unknown tool") {
		t.Errorf("unknown tool: %s", res.JSON)
	}
}

func TestPersonalTools(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "deploy.md"), []byte("# Deployment notes\nRoll out the API with a canary."), 0o644)
	st.SyncNotes(ctx, dir)

	now := time.Date(2026, 11, 8, 15, 0, 0, 0, time.Local)
	woke := 0
	r := NewRegistry(Personal(st, func() time.Time { return now }, func() { woke++ })...)

	res := r.Run(ctx, Call{TurnKey: "t1"}, "search_notes", json.RawMessage(`{"query":"deployment"}`))
	if !res.OK || !strings.Contains(string(res.JSON), `"path":"deploy.md"`) {
		t.Fatalf("search: %s", res.JSON)
	}

	args := json.RawMessage(`{"text":"Review the deployment notes","when":"tomorrow","source":"deploy.md"}`)
	res = r.Run(ctx, Call{TurnKey: "t1"}, "create_reminder", args)
	if !res.OK || !strings.Contains(string(res.JSON), `"status":"created"`) || !strings.Contains(string(res.JSON), `"due":"tomorrow at 9:00 AM"`) ||
		!strings.Contains(string(res.JSON), `"source":"deploy.md"`) {
		t.Fatalf("create: %s", res.JSON)
	}
	res = r.Run(ctx, Call{TurnKey: "t3"}, "create_reminder", json.RawMessage(`{"text":"Stretch","when":"in 5 minutes","source":"Deployment notes"}`))
	if strings.Contains(string(res.JSON), `"source"`) {
		t.Fatalf("bogus source kept: %s", res.JSON)
	}
	// Same turn retries (model repeats the call) → no duplicate.
	res = r.Run(ctx, Call{TurnKey: "t1"}, "create_reminder", args)
	if !strings.Contains(string(res.JSON), `"status":"already_exists"`) {
		t.Fatalf("retry: %s", res.JSON)
	}
	if rs, _ := st.PendingReminders(ctx); len(rs) != 2 {
		t.Fatalf("want 2 reminders (deploy, deduplicated, plus stretch), got %d", len(rs))
	}
	if woke != 3 {
		t.Fatalf("scheduler woken %d times", woke)
	}

	res = r.Run(ctx, Call{TurnKey: "t2"}, "create_reminder", json.RawMessage(`{"text":"x","when":"someday"}`))
	if res.OK || !strings.Contains(string(res.JSON), "couldn't understand") {
		t.Fatalf("bad when: %s", res.JSON)
	}

	res = r.Run(ctx, Call{}, "list_reminders", nil)
	if !res.OK || !strings.Contains(string(res.JSON), `"count":2`) {
		t.Fatalf("list: %s", res.JSON)
	}
}
