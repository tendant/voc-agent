package policy

import (
	"sort"

	"github.com/tendant/voc-agent/internal/store"
)

// Distinctive words of local_only notes: words of 4+ letters that appear in
// a local_only note but in no other note and aren't common English. Unlike
// fingerprints they catch paraphrase ("avoid running until March"), so they
// are too noisy to block on; the approval dialog shows them as warnings.
type Distinctive map[string]string // word → note title

func NewDistinctive(localOnly, others []store.RelevantNote) Distinctive {
	elsewhere := map[string]bool{}
	for _, n := range others {
		for _, w := range normWords(n.Title + " " + n.Body) {
			elsewhere[w] = true
		}
	}
	d := Distinctive{}
	for _, n := range localOnly {
		for _, w := range normWords(n.Body) {
			if len(w) >= 4 && !elsewhere[w] && !common[w] && !isNumber(w) {
				d[w] = n.Title
			}
		}
	}
	return d
}

// Find returns distinctive words present in text, sorted.
func (d Distinctive) Find(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range normWords(text) {
		if _, ok := d[w]; ok && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

func isNumber(w string) bool {
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// common holds frequent English words of 4+ letters that carry no private
// meaning on their own.
var common = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range []string{
		"about", "above", "after", "again", "against", "also", "always", "another", "back", "because", "been", "before",
		"being", "below", "best", "better", "between", "both", "came", "come", "comes", "could", "days", "does", "doing",
		"done", "down", "during", "each", "even", "every", "fine", "first", "from", "full", "give", "goes", "going", "good",
		"great", "have", "having", "here", "high", "into", "just", "keep", "know", "last", "later", "less", "like", "little",
		"long", "look", "made", "make", "many", "more", "most", "much", "must", "need", "never", "next", "once", "only",
		"other", "over", "part", "really", "right", "same", "should", "show", "since", "some", "soon", "still", "such",
		"take", "than", "that", "their", "them", "then", "there", "these", "they", "thing", "things", "this", "those",
		"though", "through", "time", "times", "today", "together", "too", "under", "until", "upon", "very", "want", "week",
		"weeks", "well", "were", "what", "when", "where", "which", "while", "will", "with", "within", "without", "would",
		"year", "years", "your", "yours", "again", "away", "around", "already", "able", "okay", "sure", "note", "notes",
		"twice", "three", "four", "five", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday",
		"mondays", "tuesdays", "wednesdays", "thursdays", "fridays", "only", "avoid", "if", "comes", "empty", "due",
	} {
		m[w] = true
	}
	return m
}()
