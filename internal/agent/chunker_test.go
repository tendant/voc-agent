package agent

import (
	"reflect"
	"testing"
)

func feed(c *Chunker, deltas ...string) []string {
	var out []string
	for _, d := range deltas {
		out = append(out, c.Push(d)...)
	}
	return append(out, c.Flush()...)
}

func TestChunker(t *testing.T) {
	cases := []struct {
		name   string
		deltas []string
		want   []string
	}{
		{"streamed", []string{"Hello the", "re. How are", " you? Fine"}, []string{"Hello there.", "How are you?", "Fine"}},
		{"decimal", []string{"Pi is 3.14 roughly. ", "Yes."}, []string{"Pi is 3.14 roughly.", "Yes."}},
		{"short merged", []string{"Sure. Your reminder is set. "}, []string{"Sure. Your reminder is set."}},
		{"markdown", []string{"**Tip:** drink `water`.\n- one\n- two\n"}, []string{"Tip: drink water.", "one", "two"}},
		{"quote", []string{`He said "go now." Then left.`}, []string{`He said "go now."`, "Then left."}},
		{"symbols only", []string{"---\n"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := feed(&Chunker{MinLen: 12}, tc.deltas...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
