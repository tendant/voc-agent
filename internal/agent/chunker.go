package agent

import (
	"regexp"
	"strings"
	"unicode"
)

// Chunker splits streamed LLM text into speakable sentences so TTS can start
// on the first sentence while the model is still generating the rest.
type Chunker struct {
	buf strings.Builder
	// MinLen avoids sending tiny fragments ("Sure.") to TTS on their own;
	// they are merged with the following sentence instead.
	MinLen int
}

// Push adds streamed text and returns any sentences that are now complete.
func (c *Chunker) Push(delta string) []string {
	c.buf.WriteString(delta)
	var out []string
	s := c.buf.String()
	start := 0
	for i := 0; i < len(s); i++ {
		if !isBoundary(s, i) {
			continue
		}
		end := i + 1
		// Absorb closing quotes/brackets after the punctuation.
		for end < len(s) && strings.ContainsRune(`"')]”’`, rune(s[end])) {
			end++
		}
		// A boundary needs whitespace after it, so "3.5" and "e.g." mid-word
		// don't split. The end of the buffer is ambiguous: wait for more.
		if end >= len(s) || !unicode.IsSpace(rune(s[end])) && s[i] != '\n' {
			continue
		}
		if seg := strings.TrimSpace(s[start:end]); len(seg) >= c.MinLen || s[i] == '\n' {
			if seg = Speakable(seg); seg != "" {
				out = append(out, seg)
			}
			start = end
		}
	}
	rest := s[start:]
	c.buf.Reset()
	c.buf.WriteString(rest)
	return out
}

// Flush returns whatever text remains at the end of the stream.
func (c *Chunker) Flush() []string {
	seg := Speakable(c.buf.String())
	c.buf.Reset()
	if seg == "" {
		return nil
	}
	return []string{seg}
}

func isBoundary(s string, i int) bool {
	switch s[i] {
	case '.', '!', '?', ';', '\n':
		return true
	}
	return false
}

var (
	mdLink   = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	mdMarks  = regexp.MustCompile("[*_`#>|~]+")
	listMark = regexp.MustCompile(`(?m)^\s*(?:[-•]|\d+[.)])\s+`)
	spaces   = regexp.MustCompile(`\s+`)
)

// Speakable strips markdown and other symbols TTS would read out literally.
func Speakable(s string) string {
	s = mdLink.ReplaceAllString(s, "$1")
	s = listMark.ReplaceAllString(s, "")
	s = mdMarks.ReplaceAllString(s, "")
	s = spaces.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	// Nothing worth saying if there are no letters or digits.
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return s
		}
	}
	return ""
}
