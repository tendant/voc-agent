package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tendant/voc-agent/internal/voice"
)

// toneWAV is 3 s of quiet 440 Hz, so <audio> has something real to play.
func toneWAV() string {
	const rate = 16000
	samples := make([]float32, 3*rate)
	for i := range samples {
		samples[i] = 0.03 * float32(math.Sin(2*math.Pi*440*float64(i)/rate))
	}
	var w bytes.Buffer
	voice.WriteWAV(&w, voice.Audio{Samples: samples, SampleRate: rate})
	return "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(w.Bytes())
}

func testBrowser(t *testing.T) (*Browser, string) {
	b := New(Config{Headless: true, ProfileDir: t.TempDir()})
	if !b.Available() {
		t.Skip("Google Chrome not installed")
	}
	t.Cleanup(b.Close)
	audio := toneWAV()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><title>Test page</title>
<nav><a href="/next">Next page</a> <a href="/about">About us</a></nav>
<main><h1>Deployment guide</h1><p>Use a canary at five percent.</p>
<audio id="a" src="%s" loop></audio>
<button onclick="document.title='clicked'">Show more</button>
<button>Buy now</button></main>`, audio)
	})
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Second</title><main>Second page body</main>`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return b, ts.URL
}

func TestReadClickRefuse(t *testing.T) {
	b, url := testBrowser(t)
	ctx := context.Background()
	if err := b.Navigate(ctx, url); err != nil {
		t.Fatal(err)
	}
	p, err := b.Read(ctx)
	if err != nil || p.Title != "Test page" || !strings.Contains(p.Text, "canary at five percent") || strings.Contains(p.Text, "About us") {
		t.Fatalf("read: %+v %v (main text only expected)", p, err)
	}
	if _, err := b.Click(ctx, "buy now"); !errors.As(err, new(*ErrRisky)) {
		t.Fatalf("risky click not refused: %v", err)
	}
	if _, err := b.Click(ctx, "no such thing"); err == nil {
		t.Fatal("missing element should error")
	}
	c, err := b.Click(ctx, "next")
	if err != nil || c.Label != "Next page" || !strings.HasSuffix(c.URL, "/next") {
		t.Fatalf("click: %+v %v", c, err)
	}
	if p, _ := b.Read(ctx); !strings.Contains(p.Text, "Second page body") {
		t.Fatalf("after click: %+v", p)
	}
}

func TestMediaAndPlaying(t *testing.T) {
	b, url := testBrowser(t)
	ctx := context.Background()
	b.Navigate(ctx, url)
	if b.Playing(ctx) {
		t.Fatal("nothing should be playing yet")
	}
	st, err := b.Media(ctx, "play")
	if err != nil || !st.Playing {
		t.Fatalf("play: %+v %v", st, err)
	}
	if !b.Playing(ctx) {
		t.Fatal("Playing() should see the audio")
	}
	st, _ = b.Media(ctx, "quieter")
	if st.Volume != 0.8 {
		t.Fatalf("quieter: %+v", st)
	}
	st, _ = b.Media(ctx, "pause")
	if st.Playing || b.Playing(ctx) {
		t.Fatalf("pause: %+v", st)
	}
	if _, err := b.Media(ctx, "next"); err == nil || !strings.Contains(err.Error(), "no next video") {
		t.Fatalf("next without a playlist: %v", err)
	}
	if _, err := b.Media(ctx, "explode"); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestRelaunchAfterWindowClosed(t *testing.T) {
	b, url := testBrowser(t)
	ctx := context.Background()
	b.Navigate(ctx, url)
	b.Close() // like the user closing the window
	if b.Playing(ctx) {
		t.Fatal("closed browser reports playing")
	}
	if err := b.Navigate(ctx, url+"/next"); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if p, _ := b.Read(ctx); p.Title != "Second" {
		t.Fatalf("after relaunch: %+v", p)
	}
}

// TestLiveYouTube needs the internet: VOC_LIVE_WEB=1.
func TestLiveYouTube(t *testing.T) {
	if os.Getenv("VOC_LIVE_WEB") == "" {
		t.Skip("set VOC_LIVE_WEB=1 (uses the internet)")
	}
	b, _ := testBrowser(t)
	ctx := context.Background()
	v, err := b.YouTubePlay(ctx, "lo-fi jazz")
	if err != nil || !strings.Contains(v.URL, "/watch?") {
		t.Fatalf("%+v %v", v, err)
	}
	t.Logf("playing %q %s", v.Title, v.URL)
	for _, step := range []struct {
		action  string
		playing bool
	}{{"pause", false}, {"play", true}, {"quieter", true}, {"pause", false}} {
		st, err := b.Media(ctx, step.action)
		t.Logf("%-8s playing=%v ad=%v vol=%v err=%v (Playing()=%v)", step.action, st.Playing, st.Ad, st.Volume, err, b.Playing(ctx))
		if err != nil || st.Playing != step.playing || b.Playing(ctx) != step.playing {
			t.Errorf("%s: %+v %v", step.action, st, err)
		}
	}
}
