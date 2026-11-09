package server

import (
	"regexp"
	"strings"
)

// DefaultWakeWord is said to wake hands-free listening after it sleeps.
// Chosen by testing: SenseVoice heard "Hey Victor" in about 90% of takes,
// while "Hey voc" / "Hey vox" mostly came out as "Hey, O" or "Hey, Fox".
const DefaultWakeWord = "Victor"

var greetings = map[string]bool{"hey": true, "hi": true, "okay": true, "ok": true, "hay": true, "o": true, "a": true, "yo": true}

// wakeMatch reports whether text starts with the wake word ("Hey Victor",
// "Okay Victor, …", "Victor, …", "Heyvictor") and returns the rest, which
// may be a request said in the same breath.
func wakeMatch(word, text string) (rest string, ok bool) {
	name := strings.ToLower(word)
	toks := strings.Fields(strings.NewReplacer(",", " ", ".", " ", "!", " ", "?", " ", ";", " ", ":", " ").Replace(strings.ToLower(text)))
	isName := func(t string) bool {
		if strings.HasPrefix(t, name) {
			return true
		}
		// One slip for names of 5+ letters ("Viktor", "Victer").
		return len(name) >= 5 && len(t) >= len(name)-1 && editDistance(t, name) <= 1
	}
	switch {
	case len(toks) == 0:
		return "", false
	case isName(toks[0]):
		return strings.Join(toks[1:], " "), true
	case len(toks) >= 2 && greetings[toks[0]] && isName(toks[1]):
		return strings.Join(toks[2:], " "), true
	}
	for g := range greetings { // run together: "heyvictor"
		if len(g) > 1 && strings.HasPrefix(toks[0], g) && isName(strings.TrimPrefix(toks[0], g)) {
			return strings.Join(toks[1:], " "), true
		}
	}
	return "", false
}

// goToSleepRe puts hands-free back to sleep without turning it off.
var goToSleepRe = regexp.MustCompile(`^(please )?(go to sleep|sleep now|go back to sleep|that's all|that is all|thanks that's all|that's it for now|goodnight|good night)( for now| thanks| thank you)?$`)

// wakeFind looks for the wake word anywhere in text (used on audio that
// includes the moments before an utterance, where a short "Hey Victor" may
// have been missed by the voice detector) and returns what follows it.
func wakeFind(word, text string) (rest string, ok bool) {
	toks := strings.Fields(strings.NewReplacer(",", " ", ".", " ", "!", " ", "?", " ", ";", " ", ":", " ").Replace(text))
	for i := range toks {
		if r, ok := wakeMatch(word, strings.Join(toks[i:], " ")); ok {
			return r, true
		}
	}
	return "", false
}
