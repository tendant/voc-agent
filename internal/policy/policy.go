// Package policy decides what may leave the machine for a cloud model. Its
// rules are deterministic hard constraints, not model judgments:
//
//   - local_only content never leaves: it is excluded when the payload is
//     built, and any outbound bytes are scanned for local_only text
//     (CheckOutbound) both before approval and at send time.
//   - personal content is minimized (only notes relevant to the question)
//     and redacted (emails, phone numbers, IPs, secrets, card numbers).
//   - conversation history is not sent.
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/tendant/voc-agent/internal/store"
)

// Source is a piece of local context that might accompany a cloud request.
type Source struct {
	Title, Path, Privacy, Text string
}

// Item describes a source in the decision, for the approval UI and audit.
type Item struct {
	Title      string         `json:"title"`
	Path       string         `json:"path"`
	Privacy    string         `json:"privacy"`
	Why        string         `json:"why,omitempty"`        // for exclusions
	Redactions map[string]int `json:"redactions,omitempty"` // kind → count
}

// Plan is what would be sent, and why.
type Plan struct {
	System   string `json:"-"`
	User     string `json:"-"`
	Included []Item `json:"included"`
	Excluded []Item `json:"excluded"`
	Note     string `json:"note"`
	// Warnings lists words in the outbound text that otherwise appear only in
	// local_only notes: possible paraphrased private details for the user to
	// check before approving.
	Warnings []string `json:"warnings,omitempty"`
}

func (p Plan) JSON() json.RawMessage {
	b, _ := json.Marshal(p)
	return b
}

// CloudSystem is the system prompt sent to the cloud model.
const CloudSystem = `You are helping a private, local voice assistant answer a question it found too hard.
Your answer will be read aloud to the user by text-to-speech: answer in at most four short, plain sentences, with no markdown, lists, or code.
Some details in the context were redacted for privacy and appear as [EMAIL], [PHONE], [IP], [SECRET], [CARD] or [ID]; don't ask for them.`

// maxContextChars bounds how much note text is sent.
const maxContextChars = 3000

// Build assembles the minimal cloud request for question.
func Build(question string, sources []Source) Plan {
	p := Plan{System: CloudSystem, Note: "Conversation history is not sent. Only notes clearly about the question are included."}
	q, qCounts := Redact(strings.TrimSpace(question))
	var terms []string
	for _, t := range store.QueryTerms(q) {
		if len(t) >= 3 { // drop fragments like "io" from addresses
			terms = append(terms, t)
		}
	}
	var ctx strings.Builder
	for _, s := range sources {
		it := Item{Title: s.Title, Path: s.Path, Privacy: s.Privacy}
		switch {
		case s.Privacy == store.LocalOnly:
			it.Why = "local_only: never leaves this device"
			p.Excluded = append(p.Excluded, it)
			continue
		case !Relevant(terms, s.Title, s.Text):
			it.Why = "not relevant enough to send"
			p.Excluded = append(p.Excluded, it)
			continue
		}
		switch s.Privacy {
		case store.Public:
		default: // personal or unknown: redact
			var counts map[string]int
			s.Text, counts = Redact(s.Text)
			it.Redactions = counts
		}
		text := strings.TrimSpace(s.Text)
		if room := maxContextChars - ctx.Len(); room <= 0 {
			it.Why = "context budget reached"
			p.Excluded = append(p.Excluded, it)
			continue
		} else if len(text) > room {
			text = text[:room] + " …"
		}
		fmt.Fprintf(&ctx, "\n--- %s ---\n%s\n", s.Title, text)
		p.Included = append(p.Included, it)
	}
	if len(qCounts) > 0 {
		p.Included = append(p.Included, Item{Title: "question", Privacy: store.Personal, Redactions: qCounts})
	}
	if ctx.Len() > 0 {
		p.User = "Context from the user's notes:" + ctx.String() + "\nQuestion: " + q
	} else {
		p.User = q
	}
	return p
}

// MinCoverage is the share of the question's search terms a note must
// contain to be sent as context when its title doesn't match. Local recall
// can afford loose matches; the cloud only gets notes clearly about the
// question.
const MinCoverage = 0.3

// Relevant decides whether a note is clearly about the question: its title
// matches a term and at least two terms match overall, or it covers
// MinCoverage of the terms. Coverage alone is diluted by wordy questions
// ("…and provide feedback"), and the title is the strongest topic signal.
func Relevant(terms []string, title, body string) bool {
	if len(terms) == 0 {
		return false
	}
	inTitle, inAll := hits(terms, title), hits(terms, title+" "+body)
	return (inTitle >= 1 && inAll >= 2) || float64(inAll)/float64(len(terms)) >= MinCoverage
}

// hits counts terms that prefix-match some word of text.
func hits(terms []string, text string) int {
	words := normWords(text)
	n := 0
	for _, t := range terms {
		for _, w := range words {
			if strings.HasPrefix(w, t) {
				n++
				break
			}
		}
	}
	return n
}

// --- redaction ---

var redactors = []struct {
	kind, label string
	re          *regexp.Regexp
}{
	{"email", "[EMAIL]", regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)},
	{"secret", "[SECRET]", regexp.MustCompile(`\b(?:sk|pk|rk|ghp|gho|github_pat|xox[abposr]|AKIA)[-_A-Za-z0-9]{12,}\b|\b[A-Fa-f0-9]{32,}\b|\b[A-Za-z0-9+/_-]{40,}={0,2}`)},
	{"card", "[CARD]", regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)},
	{"id", "[ID]", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{"ip", "[IP]", regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)},
	{"phone", "[PHONE]", regexp.MustCompile(`(?:\+\d{1,3}[ .-]?)?\(?\b\d{3}\)?[ .-]?\d{3}[ .-]?\d{4}\b`)},
}

// Redact masks personal identifiers and returns counts by kind.
func Redact(s string) (string, map[string]int) {
	counts := map[string]int{}
	for _, r := range redactors {
		s = r.re.ReplaceAllStringFunc(s, func(string) string {
			counts[r.kind]++
			return r.label
		})
	}
	if len(counts) == 0 {
		return s, nil
	}
	return s, counts
}

// --- leak check ---

// shingle is how many consecutive words count as a match. Six words of
// verbatim overlap is very unlikely by chance but catches copied passages.
const shingle = 6

// Fingerprints detects local_only text in outbound data.
type Fingerprints struct {
	shingles map[string]string // normalized word run → note title
	whole    map[string]string // very short notes, matched whole
}

func NewFingerprints(notes []store.RelevantNote) *Fingerprints {
	f := &Fingerprints{shingles: map[string]string{}, whole: map[string]string{}}
	for _, n := range notes {
		words := normWords(n.Body)
		if len(words) < shingle {
			if len(words) >= 2 {
				f.whole[strings.Join(words, " ")] = n.Title
			}
			continue
		}
		for i := 0; i+shingle <= len(words); i++ {
			f.shingles[strings.Join(words[i:i+shingle], " ")] = n.Title
		}
	}
	return f
}

// Violation reports a local_only leak.
type Violation struct {
	Note  string // title of the local_only note
	Match string // the overlapping words
}

func (v *Violation) Error() string {
	return fmt.Sprintf("outbound data contains local-only content from %q (%q)", v.Note, v.Match)
}

// Check scans text for local_only content.
func (f *Fingerprints) Check(text string) *Violation {
	words := normWords(text)
	joined := " " + strings.Join(words, " ") + " "
	for w, title := range f.whole {
		if strings.Contains(joined, " "+w+" ") {
			return &Violation{title, w}
		}
	}
	for i := 0; i+shingle <= len(words); i++ {
		run := strings.Join(words[i:i+shingle], " ")
		if title, ok := f.shingles[run]; ok {
			return &Violation{title, run}
		}
	}
	return nil
}

// CheckOutbound scans every string inside a JSON request body. Scanning the
// decoded strings (not the raw bytes) means JSON escaping can't hide a match.
func (f *Fingerprints) CheckOutbound(body []byte) error {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("outbound body is not JSON: %w", err)
	}
	var strs []string
	collect(v, &strs)
	sort.Strings(strs) // deterministic
	for _, s := range strs {
		if viol := f.Check(s); viol != nil {
			return viol
		}
	}
	return nil
}

func collect(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, x := range t {
			collect(x, out)
		}
	case map[string]any:
		for _, x := range t {
			collect(x, out)
		}
	}
}

func normWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
