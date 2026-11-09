package agent

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// fastCommand is a spoken command handled without the LLM: an exact phrase
// mapped to one tool call and a fixed spoken confirmation. These are the
// things people say mid-video or mid-page ("pause", "scroll down"), where a
// 1–2 s model round trip feels broken.
type fastCommand struct {
	re   *regexp.Regexp // matches the whole normalized utterance
	tool string
	args string
	ok   string // said on success
	// build, if set, makes args from the regexp's submatches (e.g. the search
	// terms in "search this site for X").
	build func(m []string) string
}

func phrase(alts string) *regexp.Regexp {
	// Whole utterance, optional politeness around it.
	return regexp.MustCompile(`^(?:(?:please|ok|okay|hey voc|voc|can you|could you|would you|can you please|could you please) )?(?:` + alts + `)(?: please| now| thanks| for me)?$`)
}

var fastCommands = []fastCommand{
	{phrase(`pause|pause (?:it|the video|the music|this)|stop (?:it|the video|the music)|hold on`), "media", `{"action":"pause"}`, "Paused.", nil},
	{phrase(`resume|unpause|continue|play|play (?:it|again)|keep playing|resume (?:the )?(?:video|music)`), "media", `{"action":"play"}`, "Playing.", nil},
	{phrase(`next|skip|next (?:video|song|one|track)|skip (?:this|it|this one|this video|this song)`), "media", `{"action":"next"}`, "Next one.", nil},
	{phrase(`mute|mute (?:it|the video|the sound)`), "media", `{"action":"mute"}`, "Muted.", nil},
	{phrase(`unmute|unmute (?:it|the video)|sound on`), "media", `{"action":"unmute"}`, "Unmuted.", nil},
	{phrase(`louder|volume up|turn it up|turn up the volume|turn the volume up`), "media", `{"action":"louder"}`, "Louder.", nil},
	{phrase(`quieter|softer|volume down|turn it down|turn down the volume|turn the volume down`), "media", `{"action":"quieter"}`, "Quieter.", nil},
	{phrase(`restart|start over|start (?:it|the video) (?:over|again)|from the beginning`), "media", `{"action":"restart"}`, "From the beginning.", nil},
	{phrase(`skip ahead|fast forward|forward (?:a bit|30 seconds)|jump ahead`), "media", `{"action":"forward"}`, "Skipped ahead.", nil},
	{phrase(`rewind|back a bit|go back (?:a bit|15 seconds)|rewind (?:a bit|15 seconds)`), "media", `{"action":"back"}`, "Went back a bit.", nil},
	{phrase(`scroll down|scroll|down|page down|scroll (?:down )?more`), "page_nav", `{"action":"scroll_down"}`, "Scrolled down.", nil},
	{phrase(`scroll up|up|page up`), "page_nav", `{"action":"scroll_up"}`, "Scrolled up.", nil},
	{phrase(`(?:go to |scroll to )?(?:the )?top(?: of the page)?`), "page_nav", `{"action":"top"}`, "Top of the page.", nil},
	{phrase(`(?:go to |scroll to )?(?:the )?bottom(?: of the page)?`), "page_nav", `{"action":"bottom"}`, "Bottom of the page.", nil},
	{phrase(`go back|back|previous page|go to the previous page`), "page_nav", `{"action":"back"}`, "Went back.", nil},
	{phrase(`go forward|forward|next page in history`), "page_nav", `{"action":"forward"}`, "Went forward.", nil},
	{phrase(`reload|refresh|reload (?:the|this) page|refresh (?:the|this) page`), "page_nav", `{"action":"reload"}`, "Reloaded.", nil},
	// "search this site for X" types X into the page's search box.
	{regexp.MustCompile(`^(?:(?:please|ok|okay|hey voc|voc) )?search (?:this|the) (?:site|page|website|web site) for (.+?)(?: please)?$|^(?:(?:please|ok|okay) )?search for (.+?) on (?:this|the) (?:site|page|website)$`),
		"page_type", "", "Searching.", func(m []string) string {
			q := m[1]
			if q == "" {
				q = m[2]
			}
			b, _ := json.Marshal(map[string]any{"text": q, "submit": true})
			return string(b)
		}},
}

// matchFast returns the command for a single-command utterance, if it is one.
func matchFast(user string) *fastCommand {
	cs := matchFastAll(user)
	if len(cs) != 1 {
		return nil
	}
	return cs[0]
}

var (
	partSplit = regexp.MustCompile(`(?i)\s*(?:[.!?;,]+|\band then\b|\bthen\b|\band\b)\s*`)
	repeatRe  = regexp.MustCompile(`^(.+?) (twice|two times|three times|thrice|2 times|3 times)$`)
)

// matchFastAll handles utterances made only of instant commands: "go back,
// go back", "next and then pause", "scroll down twice". If any part isn't an
// instant command, it returns nil and the model takes the whole request.
func matchFastAll(user string) []*fastCommand {
	var out []*fastCommand
	for _, part := range partSplit.Split(strings.ToLower(user), -1) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		times := 1
		if m := repeatRe.FindStringSubmatch(part); m != nil {
			part, times = m[1], map[string]int{"twice": 2, "two times": 2, "2 times": 2, "three times": 3, "thrice": 3, "3 times": 3}[m[2]]
		}
		c := matchOne(part)
		if c == nil {
			return nil
		}
		for i := 0; i < times; i++ {
			out = append(out, c)
		}
	}
	if len(out) > 5 {
		return nil // something unusual; let the model handle it
	}
	return out
}

func matchOne(user string) *fastCommand {
	n := strings.ToLower(strings.TrimSpace(user))
	n = strings.TrimFunc(n, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) })
	n = strings.NewReplacer(",", "", ".", "", "!", "", "?", "").Replace(n)
	n = strings.Join(strings.Fields(n), " ")
	for i := range fastCommands {
		c := fastCommands[i]
		if m := c.re.FindStringSubmatch(n); m != nil {
			if c.build != nil {
				c.args = c.build(m)
			}
			return &c
		}
	}
	return nil
}

// fastReply turns a tool result into the spoken reply: the fixed
// confirmation, or the tool's error as a sentence.
func fastReply(c *fastCommand, ok bool, result json.RawMessage) string {
	if ok {
		var r struct {
			Ad bool `json:"ad"`
		}
		json.Unmarshal(result, &r)
		if r.Ad && c.tool == "media" {
			return c.ok + " An ad is playing; the video starts after it."
		}
		return c.ok
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(result, &e)
	msg := strings.TrimSpace(e.Error)
	if msg == "" {
		return "Sorry, that didn't work."
	}
	return "Sorry, " + strings.TrimSuffix(msg, ".") + "."
}
