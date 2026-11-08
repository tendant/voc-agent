package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tendant/voc-agent/internal/store"
)

func TestRedact(t *testing.T) {
	in := `Mail priya@example.com or call (415) 555-0134 / +1 415.555.0199. Router 192.168.1.1.
Key sk-live_abcdefghijklmnop1234, card 4242 4242 4242 4242, SSN 123-45-6789.
Keep: migration 0042, 30 minutes, p95 latency, version 1.2.3.`
	out, counts := Redact(in)
	for _, leak := range []string{"priya@", "555-0134", "555.0199", "192.168", "sk-live", "4242", "123-45"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %q", leak, out)
		}
	}
	for _, keep := range []string{"0042", "30 minutes", "p95", "1.2.3"} {
		if !strings.Contains(out, keep) {
			t.Errorf("over-redacted %q: %q", keep, out)
		}
	}
	want := map[string]int{"email": 1, "phone": 2, "ip": 1, "secret": 1, "card": 1, "id": 1}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("%s: %d want %d (%v)", k, counts[k], v, counts)
		}
	}
}

func TestBuildExcludesLocalOnlyAndRedactsPersonal(t *testing.T) {
	p := Build("Is my canary pasta plan risky? ping me at me@x.io", []Source{
		{Title: "Health", Path: "private/health.md", Privacy: store.LocalOnly, Text: "Blood pressure 128 over 82, taking lisinopril"},
		{Title: "Deploy", Path: "work/deploy.md", Privacy: store.Personal, Text: "Canary first plan. Contact ops@corp.com"},
		{Title: "Recipes", Path: "public/r.md", Privacy: store.Public, Text: "Pasta plan ops@corp.com"},
		{Title: "Groceries", Path: "g.md", Privacy: store.Personal, Text: "Oat milk, eggs, and a risky amount of coffee"},
	})
	if strings.Contains(p.User, "lisinopril") || strings.Contains(p.User, "Health") {
		t.Fatalf("local_only leaked: %q", p.User)
	}
	if !strings.Contains(p.User, "Canary first plan. Contact [EMAIL]") || !strings.Contains(p.User, "ping me at [EMAIL]") {
		t.Fatalf("personal not redacted: %q", p.User)
	}
	if strings.Contains(p.User, "Oat milk") {
		t.Fatalf("weakly related note sent: %q", p.User)
	}
	if !strings.Contains(p.User, "Pasta plan ops@corp.com") {
		t.Fatalf("public text should pass unchanged: %q", p.User)
	}
	if len(p.Excluded) != 2 || p.Excluded[0].Path != "private/health.md" || p.Excluded[1].Why != "not relevant enough to send" || len(p.Included) != 3 {
		t.Fatalf("included=%+v excluded=%+v", p.Included, p.Excluded)
	}
}

func TestFingerprintsCatchCopiedPrivateText(t *testing.T) {
	f := NewFingerprints([]store.RelevantNote{
		{Title: "Health", Body: "# Health\nDr. Patel said my blood pressure was 128 over 82 at the March checkup."},
		{Title: "PIN", Body: "Garage code 7741"},
	})
	body, _ := json.Marshal(map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "content": "Given that my blood pressure was 128 over 82, what exercise is safe?"}},
	})
	err := f.CheckOutbound(body)
	if v, ok := err.(*Violation); !ok || v.Note != "Health" {
		t.Fatalf("expected violation, got %v", err)
	}
	// JSON escaping can't hide it.
	esc := []byte(`{"q":"blood pressure was 128 over 82 at the"}`)
	if f.CheckOutbound(esc) == nil {
		t.Fatal("escaped text slipped through")
	}
	if f.CheckOutbound([]byte(`{"q":"what is the garage code 7741 for"}`)) == nil {
		t.Fatal("short note not matched")
	}
	if err := f.CheckOutbound([]byte(`{"q":"How should I plan a canary deploy?"}`)); err != nil {
		t.Fatalf("false positive: %v", err)
	}
}

func TestDistinctiveFlagsParaphrase(t *testing.T) {
	d := NewDistinctive(
		[]store.RelevantNote{{Title: "Health", Body: "No running until March. Physical therapy with Dr. Okafor, ibuprofen if swelling."}},
		[]store.RelevantNote{{Title: "Deploy", Body: "Run the migration first, then therapy for the database (joke)."}},
	)
	got := d.Find("Plan workouts that avoid running until March; is ibuprofen ok?")
	want := []string{"ibuprofen", "march", "running"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if w := d.Find("Explain the CAP theorem and database therapy"); len(w) != 0 {
		t.Fatalf("false positives: %v", w)
	}
}

func TestRelevant(t *testing.T) {
	terms := func(q string) []string { return store.QueryTerms(q) }
	deploy := [2]string{"Deployment notes — billing service v2", "Run the migration, canary at 5%, roll out the rest in waves; traffic is lowest at 6 PM."}
	for q, want := range map[string]bool{
		"Review the user's billing deployment plan for potential risks and provide feedback.": true,  // wordy but on topic
		"Is a canary at 5% with waves enough for the migration?":                              true,  // body coverage
		"Create a weekly workout plan with low-impact exercises and rest days":                false, // incidental "rest"/"low"
		"Explain the CAP theorem": false,
	} {
		if got := Relevant(terms(q), deploy[0], deploy[1]); got != want {
			t.Errorf("%q: %v", q, got)
		}
	}
}
