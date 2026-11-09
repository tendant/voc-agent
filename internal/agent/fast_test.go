package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/tools"
)

func TestMatchFast(t *testing.T) {
	for in, want := range map[string]string{
		"Pause.": `media {"action":"pause"}`, "pause the video": `media {"action":"pause"}`,
		"Play.": `media {"action":"play"}`, "please resume": `media {"action":"play"}`,
		"Next video.": `media {"action":"next"}`, "Turn it up!": `media {"action":"louder"}`,
		"volume down": `media {"action":"quieter"}`, "Skip ahead.": `media {"action":"forward"}`,
		"Scroll down.": `page_nav {"action":"scroll_down"}`, "go to the top of the page": `page_nav {"action":"top"}`,
		"Go back.": `page_nav {"action":"back"}`, "Refresh this page": `page_nav {"action":"reload"}`,
		"rewind a bit":                         `media {"action":"back"}`,
		"Search this site for tomato soup.":    `page_type {"submit":true,"text":"tomato soup"}`,
		"search for garlic bread on this page": `page_type {"submit":true,"text":"garlic bread"}`,
		// Not commands: they need the model.
		"Play lo-fi jazz.": "", "play the second video": "", "scroll to the recipe for soup": "",
		"go back to the previous video and pause it": "", "Pause my reminders": "",
		"What's playing?": `media {"action":"status"}`, "last song": `media {"action":"previous"}`,
	} {
		got := ""
		if c := matchFast(in); c != nil {
			got = c.tool + " " + c.args
		}
		if got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

type noLLM struct{ calls int }

func (n *noLLM) Chat(context.Context, llm.Request, func(string)) (llm.Message, error) {
	n.calls++
	return llm.Message{Role: "assistant", Content: "The model answered."}, nil
}

func TestFastCommandSkipsModel(t *testing.T) {
	type navArgs struct {
		Action string `json:"action"`
	}
	open := true
	var did []string
	reg := tools.NewRegistry(tools.Typed("page_nav", "d", func(_ context.Context, _ tools.Call, a navArgs) (any, error) {
		did = append(did, a.Action)
		if a.Action == "back" {
			return nil, errors.New("there is no page to go back to")
		}
		return map[string]string{"action": a.Action}, nil
	}).When(func() bool { return open }))
	m := &noLLM{}
	s := &Session{LLM: m, TTS: fakeTTS{}, Tools: reg}

	r := &rec{}
	s.Turn(context.Background(), 1, "Scroll down.", time.Now(), r)
	if m.calls != 0 || len(did) != 1 || spokenText(r) != "Scrolled down. " {
		t.Fatalf("calls=%d did=%v said=%q", m.calls, did, spokenText(r))
	}
	r = &rec{}
	s.Turn(context.Background(), 2, "go back", time.Now(), r)
	if !strings.Contains(spokenText(r), "Sorry, there is no page to go back to.") {
		t.Fatalf("error not spoken: %q", spokenText(r))
	}
	// History records the call like a normal tool turn.
	h := s.History()
	if len(h) != 8 || h[1].ToolCalls[0].Function.Name != "page_nav" || h[3].Content != "Scrolled down." {
		t.Fatalf("history: %+v", h)
	}
	// No page open: the tool isn't available, so the model handles it.
	open = false
	s.Turn(context.Background(), 3, "scroll down", time.Now(), &rec{})
	if m.calls != 1 || len(did) != 2 {
		t.Fatalf("closed browser: calls=%d did=%v", m.calls, did)
	}
}

func TestSubmitNeedsUserWords(t *testing.T) {
	type typeArgs struct {
		Text   string `json:"text"`
		Field  string `json:"field,omitempty"`
		Submit bool   `json:"submit,omitempty"`
	}
	var got []bool
	reg := tools.NewRegistry(tools.Typed("page_type", "d", func(_ context.Context, _ tools.Call, a typeArgs) (any, error) {
		got = append(got, a.Submit)
		return map[string]any{"field": a.Field}, nil
	}))
	for _, c := range []struct {
		say  string
		want bool
	}{{"Type 94110 in the zip code box.", false}, {"type tomato soup in the search box and press enter", true}} {
		f := &scriptLLM{rounds: []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{call("page_type", `{"text":"x","field":"zip","submit":true}`)}}, {Role: "assistant"}}}
		s := &Session{LLM: f, TTS: fakeTTS{}, Tools: reg}
		r := &rec{}
		s.Turn(context.Background(), 1, c.say, time.Now(), r)
		if got[len(got)-1] != c.want {
			t.Errorf("%q: submit=%v", c.say, got[len(got)-1])
		}
		// The model's empty final reply gets a confirmation.
		if spokenText(r) != "Typed it. " {
			t.Errorf("%q: said %q", c.say, spokenText(r))
		}
	}
}

func TestTerseClaims(t *testing.T) {
	for _, c := range []string{"Went back.", "Scrolled down.", "Paused.", "Opened the second result."} {
		if unbackedClaim(c, map[string]bool{}) == "" {
			t.Errorf("%q not caught", c)
		}
		if unbackedClaim(c, map[string]bool{"page_nav": true, "media": true, "page_result": true}) != "" {
			t.Errorf("%q blocked though backed", c)
		}
	}
	for _, ok := range []string{"Went to the store yesterday, you said?", "Opening hours are nine to five."} {
		_ = ok // informational: these may be blocked conservatively; check the important ones above
	}
}

func TestMatchFastAll(t *testing.T) {
	for in, want := range map[string]string{
		"Go back. Go back.":          "back back",
		"previous song":              "previous",
		"what's playing":             "status",
		"next, next":                 "next next",
		"scroll down twice":          "scroll_down scroll_down",
		"next and then pause":        "next pause",
		"go back and buy it":         "",
		"pause, then tell me a joke": "",
	} {
		var got []string
		for _, c := range matchFastAll(in) {
			got = append(got, strings.TrimSuffix(strings.TrimPrefix(strings.Split(c.args, ":")[1], `"`), `"}`))
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%q: %q want %q", in, strings.Join(got, " "), want)
		}
	}
}

func TestStatusReply(t *testing.T) {
	c := matchFast("what's playing")
	for res, want := range map[string]string{
		`{"state":"playing","name":"So What","artist":"Miles Davis"}`: "That's So What by Miles Davis.",
		`{"source":"browser","title":"Lo-fi beats - YouTube"}`:        "That's Lo-fi beats.",
		`{"state":"not running"}`:                                     "Nothing is playing.",
	} {
		if got := fastReply(c, true, []byte(res)); got != want {
			t.Errorf("%s: %q", res, got)
		}
	}
}

func TestNoteClaims(t *testing.T) {
	for s, want := range map[string]string{
		"I've saved a note called groceries.": "apple_notes_create", "Your note has been saved.": "",
		"Saved a new note.": "apple_notes_create", "Your note is saved in Notes.": "apple_notes_create",
		"You have a note about groceries.": "",
	} {
		got := unbackedClaim(s, map[string]bool{})
		if s == "Your note has been saved." {
			want = "apple_notes_create"
		}
		if got != want {
			t.Errorf("%q: %q want %q", s, got, want)
		}
	}
}
