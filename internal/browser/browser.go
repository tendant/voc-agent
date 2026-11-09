// Package browser drives a separate Chrome window that voc controls: its own
// profile (not logged into the user's accounts), launched on first use and
// relaunched if the user closes it. Page work is done with small JavaScript
// snippets over the Chrome DevTools Protocol; nothing is sent anywhere but
// the sites the user asked for.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// DefaultChrome is where Google Chrome lives on macOS.
const DefaultChrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

type Config struct {
	ExecPath   string // Chrome binary; DefaultChrome if empty
	ProfileDir string // separate user data dir; ~/.voc/chrome-profile if empty
	Headless   bool   // tests
}

type Browser struct {
	cfg Config

	mu          sync.Mutex
	allocCancel context.CancelFunc
	tab         context.Context
	tabCancel   context.CancelFunc
}

func New(cfg Config) *Browser {
	if cfg.ExecPath == "" {
		cfg.ExecPath = DefaultChrome
	}
	if cfg.ProfileDir == "" {
		home, _ := os.UserHomeDir()
		cfg.ProfileDir = filepath.Join(home, ".voc", "chrome-profile")
	}
	return &Browser{cfg: cfg}
}

// Available reports whether Chrome is installed.
func (b *Browser) Available() bool {
	_, err := os.Stat(b.cfg.ExecPath)
	return err == nil
}

func (b *Browser) launch() error {
	if err := os.MkdirAll(b.cfg.ProfileDir, 0o700); err != nil {
		return err
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(b.cfg.ExecPath),
		chromedp.UserDataDir(b.cfg.ProfileDir),
		chromedp.Flag("headless", b.cfg.Headless),
		chromedp.Flag("disable-popup-blocking", false),               // keep Chrome's popup blocker
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"), // "play" must work without a click
		chromedp.WindowSize(1280, 900),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	tab, tabCancel := chromedp.NewContext(allocCtx)
	if _, err := chromedp.Run(tab, chromedp.Sleep(0)); err != nil { // starts the browser
		tabCancel()
		allocCancel()
		return fmt.Errorf("could not start Chrome: %w", err)
	}
	b.allocCancel, b.tab, b.tabCancel = allocCancel, tab, tabCancel
	return nil
}

func (b *Browser) closeLocked() {
	if b.tabCancel != nil {
		b.tabCancel()
		b.allocCancel()
	}
	b.tab, b.tabCancel, b.allocCancel = nil, nil, nil
}

// Close shuts the controlled browser down.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked()
}

// run calls f with the controlled tab's context, launching Chrome if needed
// and relaunching once if the user closed the window.
func (b *Browser) run(ctx context.Context, timeout time.Duration, f func(tab context.Context) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if b.tab == nil || b.tab.Err() != nil {
			b.closeLocked()
			if err := b.launch(); err != nil {
				return err
			}
		}
		tctx, cancel := context.WithTimeout(b.tab, timeout)
		stop := context.AfterFunc(ctx, cancel) // caller cancellation (turn interrupted)
		err := f(tctx)
		stop()
		cancel()
		if err == nil || ctx.Err() != nil || attempt > 0 || b.tab.Err() == nil {
			return err
		}
		// The tab or browser died (window closed): start over once.
	}
}

// Navigate opens u in the controlled window.
func (b *Browser) Navigate(ctx context.Context, u string) error {
	return b.run(ctx, 25*time.Second, func(tab context.Context) error {
		return chromedp.Do(tab, chromedp.Navigate(u))
	})
}

// eval runs a JavaScript expression (awaiting a promise result) and decodes
// its JSON value into T.
func eval[T any](ctx context.Context, b *Browser, timeout time.Duration, js string) (T, error) {
	var out T
	err := b.run(ctx, timeout, func(tab context.Context) error {
		var err error
		out, err = chromedp.Run(tab, chromedp.Evaluate[T](js, chromedp.EvalAwaitPromise))
		return err
	})
	return out, err
}

// --- YouTube ---

type Video struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// YouTubePlay searches YouTube and plays the first video result (skipping
// ads, Shorts and playlists).
func (b *Browser) YouTubePlay(ctx context.Context, query string) (Video, error) {
	u := "https://www.youtube.com/results?search_query=" + urlQuery(query)
	if err := b.Navigate(ctx, u); err != nil {
		return Video{}, err
	}
	err := b.run(ctx, 15*time.Second, func(tab context.Context) error {
		return chromedp.Do(tab, chromedp.WaitVisible(chromedp.CSS(`ytd-video-renderer a#video-title`)))
	})
	var v Video
	if err == nil {
		v, err = eval[Video](ctx, b, 5*time.Second, `(() => {
			for (const a of document.querySelectorAll('ytd-video-renderer a#video-title')) {
				if (a.href && a.href.includes('/watch?')) return {title: (a.title || a.textContent).trim(), url: a.href};
			}
			return {title: '', url: ''};
		})()`)
	}
	if err != nil {
		return Video{}, fmt.Errorf("couldn't find a video on YouTube for %q: %w", query, err)
	}
	if v.URL == "" {
		return Video{}, fmt.Errorf("no video results on YouTube for %q", query)
	}
	if err := b.Navigate(ctx, v.URL); err != nil {
		return Video{}, err
	}
	// Autoplay is allowed by flag; nudge it in case the page didn't start.
	b.run(ctx, 10*time.Second, func(tab context.Context) error {
		return chromedp.Do(tab, chromedp.WaitReady(chromedp.CSS(`video`)))
	})
	eval[bool](ctx, b, 5*time.Second, `(async () => { const v = document.querySelector('video'); if (v && v.paused) await v.play().catch(() => {}); return !!v; })()`)
	return v, nil
}

// --- media ---

// MediaActions are the supported media controls.
var MediaActions = []string{"play", "pause", "next", "mute", "unmute", "louder", "quieter", "restart", "forward", "back"}

type MediaState struct {
	Action  string  `json:"action"`
	Playing bool    `json:"playing"`
	Muted   bool    `json:"muted"`
	Volume  float64 `json:"volume"`
	Title   string  `json:"title,omitempty"`
	Ad      bool    `json:"ad,omitempty"` // a YouTube ad is playing; the video starts after it
}

// Media controls the first <video> or <audio> on the page.
func (b *Browser) Media(ctx context.Context, action string) (MediaState, error) {
	known := false
	for _, a := range MediaActions {
		known = known || a == action
	}
	if !known {
		return MediaState{}, fmt.Errorf("unknown media action %q", action)
	}
	js := `(async (action) => {
		const wait = () => new Promise(r => setTimeout(r, 400));
		// YouTube: use its player API; it resumes or swaps a bare <video>
		// (ads), so direct element control doesn't stick.
		const yt = document.getElementById('movie_player');
		// During an ad the API reports (and controls) the main video, not the
		// ad that is actually playing, so ads are handled as a plain element.
		const ad = yt && yt.classList.contains('ad-showing');
		if (yt && typeof yt.pauseVideo === 'function' && !(ad && action !== 'next')) {
			switch (action) {
				case 'play': yt.playVideo(); break;
				case 'pause': yt.pauseVideo(); break;
				case 'next': yt.nextVideo(); break;
				case 'mute': yt.mute(); break;
				case 'unmute': yt.unMute(); break;
				case 'louder': yt.unMute(); yt.setVolume(Math.min(100, yt.getVolume() + 20)); break;
				case 'quieter': yt.setVolume(Math.max(0, yt.getVolume() - 20)); break;
				case 'restart': yt.seekTo(0, true); yt.playVideo(); break;
				case 'forward': yt.seekTo(yt.getCurrentTime() + 30, true); break;
				case 'back': yt.seekTo(Math.max(0, yt.getCurrentTime() - 15), true); break;
			}
			await wait();
			// After play/restart/next YouTube buffers first (state 3); give it
			// a moment to start. 1 = playing, 3 = buffering on the way to it.
			if (['play', 'restart', 'next'].includes(action)) {
				for (let i = 0; i < 10 && yt.getPlayerState() !== 1; i++) await wait();
			}
			const d = yt.getVideoData ? yt.getVideoData() : {};
			const st = yt.getPlayerState();
			return {action, playing: st === 1 || (st === 3 && action !== 'pause'), muted: yt.isMuted(),
				volume: yt.getVolume() / 100, title: (d && d.title) || document.title};
		}
		const m = document.querySelector('video, audio');
		if (!m) return {error: 'nothing to play on this page'};
		switch (action) {
			case 'next': if (yt && yt.nextVideo) { yt.nextVideo(); break; } return {error: 'there is no next video on this page'};
			case 'play': await m.play().catch(() => {}); break;
			case 'pause': m.pause(); break;
			case 'mute': m.muted = true; break;
			case 'unmute': m.muted = false; break;
			case 'louder': m.muted = false; m.volume = Math.min(1, m.volume + 0.2); break;
			case 'quieter': m.volume = Math.max(0, m.volume - 0.2); break;
			case 'restart': m.currentTime = 0; await m.play().catch(() => {}); break;
			case 'forward': m.currentTime = Math.min(m.duration || 1e9, m.currentTime + 30); break;
			case 'back': m.currentTime = Math.max(0, m.currentTime - 15); break;
		}
		await wait();
		return {action, playing: !m.paused, muted: m.muted, volume: Math.round(m.volume * 100) / 100, title: document.title, ad: !!ad};
	})(` + jsString(action) + `)`
	type result struct {
		MediaState
		Error string `json:"error"`
	}
	res, err := eval[result](ctx, b, 10*time.Second, js)
	if err != nil {
		return MediaState{}, err
	}
	if res.Error != "" {
		return MediaState{}, errors.New(res.Error)
	}
	return res.MediaState, nil
}

// Playing reports whether media is playing in the controlled window right
// now. It never launches the browser and answers false on any error.
func (b *Browser) Playing(ctx context.Context) bool {
	b.mu.Lock()
	tab := b.tab
	b.mu.Unlock()
	if tab == nil || tab.Err() != nil {
		return false
	}
	tctx, cancel := context.WithTimeout(tab, 500*time.Millisecond)
	defer cancel()
	playing, err := chromedp.Run(tctx, chromedp.Evaluate[bool](`(() => {
		const yt = document.getElementById('movie_player');
		if (yt && typeof yt.getPlayerState === 'function' && !yt.classList.contains('ad-showing'))
			return yt.getPlayerState() === 1 && !yt.isMuted() && yt.getVolume() > 0;
		return [...document.querySelectorAll('video, audio')].some(m => !m.paused && !m.muted && m.volume > 0);
	})()`))
	return err == nil && playing
}

// --- reading and clicking ---

type Page struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// MaxPageText bounds how much page text goes to the local model.
const MaxPageText = 6000

// Read returns the page's main text.
func (b *Browser) Read(ctx context.Context) (Page, error) {
	p, err := eval[Page](ctx, b, 15*time.Second, `(() => {
		const root = document.querySelector('main, article, [role=main]') || document.body;
		const text = (root ? root.innerText : '').replace(/[ \t]+/g, ' ').replace(/\n\s*\n+/g, '\n').trim();
		return {title: document.title, url: location.href, text};
	})()`)
	if err != nil {
		return Page{}, err
	}
	if len(p.Text) > MaxPageText {
		p.Text, p.Truncated = p.Text[:MaxPageText], true
	}
	return p, nil
}

// Risky matches labels of controls voc refuses to click: anything that
// spends money, sends or publishes something, or deletes data.
var Risky = regexp.MustCompile(`(?i)\b(buy|order|checkout|check out|pay|purchase|add to cart|send|post|publish|submit|delete|remove|subscribe|unsubscribe|sign up|transfer|donate|confirm|book now|reply|tweet|share)\b`)

type Clicked struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// ErrRisky is returned instead of clicking a risky control.
type ErrRisky struct{ Label string }

func (e *ErrRisky) Error() string {
	return fmt.Sprintf("refused to click %q: voc doesn't click buttons that buy, send, post or delete; please do that yourself", e.Label)
}

// Click clicks the visible link or button whose text best matches text.
func (b *Browser) Click(ctx context.Context, text string) (Clicked, error) {
	type result struct {
		Label string `json:"label"`
		Found bool   `json:"found"`
		Risky bool   `json:"risky"`
	}
	js := `((want, riskySrc) => {
		const risky = new RegExp(riskySrc, 'i');
		want = want.trim().toLowerCase();
		const els = document.querySelectorAll('a, button, [role=button], [role=link], [role=tab], [role=menuitem], input[type=button], input[type=submit], summary');
		let best = null, bestScore = 0;
		for (const el of els) {
			const r = el.getBoundingClientRect();
			if (!r.width || !r.height || getComputedStyle(el).visibility === 'hidden') continue;
			const label = (el.innerText || el.getAttribute('aria-label') || el.value || el.title || '').trim().replace(/\s+/g, ' ');
			if (!label) continue;
			const l = label.toLowerCase();
			// Exact > starts with > contains; last resort, the label is inside a
			// longer request ("next recipe buy now" → "Next recipe").
			const score = l === want ? 3 : l.startsWith(want) ? 2 : l.includes(want) ? 1 :
				(l.length >= 4 && want.includes(l) && !risky.test(label)) ? 0.5 + l.length / 1000 : 0;
			if (score > bestScore || (score === bestScore && score >= 1 && best && label.length < best.label.length)) {
				if (score) { best = {el, label}; bestScore = score; }
			}
		}
		if (!best) return {found: false};
		if (risky.test(best.label)) return {found: true, risky: true, label: best.label};
		best.el.scrollIntoView({block: 'center'});
		best.el.click();
		return {found: true, label: best.label};
	})(` + jsString(text) + `, ` + jsString(jsRegex(Risky)) + `)`
	res, err := eval[result](ctx, b, 10*time.Second, js)
	if err != nil {
		return Clicked{}, err
	}
	switch {
	case !res.Found:
		return Clicked{}, fmt.Errorf("no link or button matching %q on this page", text)
	case res.Risky:
		return Clicked{}, &ErrRisky{res.Label}
	}
	time.Sleep(700 * time.Millisecond) // let navigation start
	var u string
	b.run(ctx, 5*time.Second, func(tab context.Context) error {
		var err error
		u, err = chromedp.Run(tab, chromedp.Location())
		return err
	})
	return Clicked{Label: res.Label, URL: u}, nil
}

// --- helpers ---

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// jsRegex converts a Go (?i) regexp to a JS RegExp source.
func jsRegex(re *regexp.Regexp) string { return strings.TrimPrefix(re.String(), "(?i)") }
