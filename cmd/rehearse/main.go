// rehearse runs the demo script against a running voc server by voice: each
// line is synthesized with Piper, resampled to 16 kHz and streamed in 20 ms
// frames like the browser does. Approval prompts are answered by voice too.
// It prints what the server heard, the tools it used, the reply, and the time
// from end of speech to the first reply audio.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/tendant/voc-agent/internal/agent"
	"github.com/tendant/voc-agent/internal/voice"
)

// Script is the demo, in order. Approve is the spoken answer if the line
// triggers a cloud approval prompt.
var Script = []struct{ Say, Approve string }{
	{"Hi! Give me one quick tip for staying focused.", ""},
	{"Find my deployment notes and remind me to review them tomorrow.", ""},
	{"What's the rollback plan?", ""},
	{"Remind me to stretch in one minute.", ""},
	{"Ask the cloud to review my billing deployment plan for risks.", "Approve."},
	{"Ask the cloud for a weekly workout plan that fits my knee recovery.", "Cancel."},
	{"What reminders do I have?", ""},
}

const notifyTurn = ^uint32(0)

func main() {
	url := flag.String("url", "ws://127.0.0.1:8080/ws", "voc server WebSocket URL")
	models := flag.String("models", "models", "model directory (for Piper)")
	waitReminder := flag.Duration("wait-reminder", 75*time.Second, "how long to wait for the 1-minute reminder to fire (0 to skip)")
	handsFree := flag.Bool("handsfree", false, "use hands-free listening: one continuous mic stream, no push-to-talk")
	flag.Parse()

	tts, err := voice.NewSherpaTTS(voice.ModelPaths{Dir: *models})
	if err != nil {
		log.Fatal(err)
	}
	defer tts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, _, err := websocket.Dial(ctx, *url, nil)
	if err != nil {
		log.Fatalf("connect %s: %v (is the server running?)", *url, err)
	}
	defer c.CloseNow()
	c.SetReadLimit(16 << 20)

	// In hands-free mode the "mic" is a single continuous stream: speech is
	// followed by silence, and the server decides where utterances end.
	silence := voice.FloatToPCM16(make([]float32, voice.SampleRate*12/10)) // 1.2 s
	if *handsFree {
		c.Write(ctx, websocket.MessageText, []byte(`{"type":"listen_start"}`))
		c.Write(ctx, websocket.MessageBinary, silence)
	}
	speak := func(text string) {
		a, err := tts.Synthesize(text)
		if err != nil {
			log.Fatal(err)
		}
		pcm := voice.FloatToPCM16(voice.Resample(a.Samples, a.SampleRate, voice.SampleRate))
		if *handsFree {
			pcm = append(pcm, silence...)
		} else {
			c.Write(ctx, websocket.MessageText, []byte(`{"type":"ptt_start"}`))
		}
		for i := 0; i < len(pcm); i += 640 {
			c.Write(ctx, websocket.MessageBinary, pcm[i:min(i+640, len(pcm))])
		}
		if !*handsFree {
			c.Write(ctx, websocket.MessageText, []byte(`{"type":"ptt_end"}`))
		}
	}

	failures, reprompts := 0, 0
	var firstAudio []time.Duration
	for i, line := range Script {
		fmt.Printf("\n[%d] YOU   %s\n", i+1, line.Say)
		speak(line.Say)
		end := time.Now()
		var turn uint32
		var heard time.Duration
		for done := false; !done; {
			typ, data, err := c.Read(ctx)
			if err != nil {
				log.Fatalf("read: %v", err)
			}
			if typ == websocket.MessageBinary {
				if t := binary.LittleEndian.Uint32(data); t == turn && heard == 0 {
					heard = time.Since(end)
				}
				continue
			}
			var e agent.Event
			json.Unmarshal(data, &e)
			switch e.Type {
			case "turn_start":
				turn = e.Turn
			case "ignored":
				fmt.Printf("    IGNORED %q\n", e.Text)
			case "transcript":
				fmt.Printf("    HEARD %q\n", e.Text)
			case "recall":
				var ns []struct{ Title string }
				json.Unmarshal(e.Result, &ns)
				var ts []string
				for _, n := range ns {
					ts = append(ts, n.Title)
				}
				fmt.Printf("    NOTES %s\n", strings.Join(ts, "; "))
			case "ack":
				fmt.Printf("    ACK   %q\n", e.Text)
			case "tool_call":
				fmt.Printf("    TOOL  %s %s\n", e.Name, e.Args)
			case "guard":
				fmt.Printf("    GUARD %s\n", e.Text)
			case "approval_request":
				var req struct {
					Destination string
					Policy      struct {
						Excluded []struct{ Title, Why string }
						Warnings []string
					}
				}
				json.Unmarshal(e.Result, &req)
				fmt.Printf("    ASKS  approval to send to %s", req.Destination)
				for _, x := range req.Policy.Excluded {
					fmt.Printf("; withheld %q (%s)", x.Title, x.Why)
				}
				if len(req.Policy.Warnings) > 0 {
					fmt.Printf("; warnings %v", req.Policy.Warnings)
				}
				fmt.Printf("\n    YOU   %s\n", line.Approve)
				if line.Approve == "" {
					line.Approve = "Cancel."
				}
				speak(line.Approve)
			case "approval_reprompt":
				// Like a person would: repeat the answer more fully.
				again := map[bool]string{true: "Yes, approve it.", false: "No, cancel it."}[strings.HasPrefix(strings.ToLower(line.Approve), "approve")]
				fmt.Printf("    ASKS  again (misheard)\n    YOU   %s\n", again)
				reprompts++
				speak(again)
			case "approval_resolved":
				fmt.Printf("    %s\n", map[bool]string{true: "APPROVED", false: "CANCELLED"}[e.OK != nil && *e.OK])
			case "error":
				fmt.Printf("    ERROR %s\n", e.Error)
				failures++
			case "assistant_done":
				if e.Turn == turn {
					fmt.Printf("    VOC   %s\n", e.Text)
					if e.Text == "" {
						failures++
					}
				}
			case "timing":
				if e.Turn == turn && e.MS["total"] > 0 {
					fmt.Printf("    TIME  first audio %v after you stopped talking (llm first token %dms, first tts %dms, total %dms)\n",
						heard.Round(10*time.Millisecond), e.MS["llm_ttft"], e.MS["tts_first"], e.MS["total"])
					if heard > 0 {
						firstAudio = append(firstAudio, heard)
					}
					done = true
				}
			case "reminder_fired":
				fmt.Printf("    ⏰    reminder fired: %s\n", e.Text)
			}
		}
	}

	if *waitReminder > 0 {
		fmt.Printf("\nwaiting up to %v for the stretch reminder…\n", *waitReminder)
		wctx, wcancel := context.WithTimeout(ctx, *waitReminder)
		fired := false
		for !fired {
			typ, data, err := c.Read(wctx)
			if err != nil {
				break
			}
			if typ == websocket.MessageText {
				var e agent.Event
				json.Unmarshal(data, &e)
				if e.Type == "reminder_fired" {
					fmt.Printf("⏰ reminder fired at %s: %s\n", time.Now().Format("15:04:05"), e.Text)
					fired = true
				}
			} else if binary.LittleEndian.Uint32(data) == notifyTurn {
				// reminder speech
			}
		}
		wcancel()
		if !fired {
			fmt.Println("reminder did not fire")
			failures++
		}
	}

	var worst, sum time.Duration
	for _, d := range firstAudio {
		sum += d
		worst = max(worst, d)
	}
	if n := len(firstAudio); n > 0 {
		fmt.Printf("\nfirst audio: mean %v, worst %v over %d turns (target < 3s)\n", (sum / time.Duration(n)).Round(10*time.Millisecond), worst.Round(10*time.Millisecond), n)
	}
	if reprompts > 0 {
		fmt.Printf("%d approval answer(s) had to be repeated (misheard)\n", reprompts)
	}
	if failures > 0 {
		fmt.Printf("%d problem(s)\n", failures)
		os.Exit(1)
	}
}
