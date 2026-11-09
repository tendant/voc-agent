package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// IsOpen reports whether voc's browser window is running (without starting it).
func (b *Browser) IsOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tab != nil && b.tab.Err() == nil
}

// NavActions are the supported navigation and scrolling actions.
var NavActions = []string{"back", "forward", "reload", "scroll_down", "scroll_up", "top", "bottom"}

type Location struct {
	Action string `json:"action"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	// Scroll position as a percentage of the page, for scroll actions.
	Percent int `json:"percent,omitempty"`
}

// Nav goes back/forward, reloads or scrolls the page.
func (b *Browser) Nav(ctx context.Context, action string) (Location, error) {
	switch action {
	case "back", "forward", "reload":
		js := map[string]string{"back": "history.back()", "forward": "history.forward()", "reload": "location.reload()"}[action]
		before, _ := eval[string](ctx, b, 3*time.Second, `location.href`)
		if _, err := eval[any](ctx, b, 3*time.Second, `(() => { `+js+`; return null; })()`); err != nil {
			return Location{}, err
		}
		// Wait for the navigation to land (or give up quietly if there was
		// nowhere to go).
		for i := 0; i < 20; i++ {
			time.Sleep(150 * time.Millisecond)
			if u, err := eval[string](ctx, b, 2*time.Second, `document.readyState === 'complete' ? location.href : ''`); err == nil && u != "" && (u != before || action == "reload") {
				break
			}
		}
		l, err := eval[Location](ctx, b, 3*time.Second, `({title: document.title, url: location.href})`)
		l.Action = action
		if err == nil && action != "reload" && l.URL == before {
			return l, fmt.Errorf("there is no page to go %s to", action)
		}
		return l, err
	case "scroll_down", "scroll_up", "top", "bottom":
		js := map[string]string{
			"scroll_down": `window.scrollBy({top: innerHeight * 0.8, behavior: 'smooth'})`,
			"scroll_up":   `window.scrollBy({top: -innerHeight * 0.8, behavior: 'smooth'})`,
			"top":         `window.scrollTo({top: 0, behavior: 'smooth'})`,
			"bottom":      `window.scrollTo({top: document.documentElement.scrollHeight, behavior: 'smooth'})`,
		}[action]
		// Smooth scrolling takes a while on long pages: wait until it settles.
		l, err := eval[Location](ctx, b, 4*time.Second, `(async () => { `+js+`;
			const sleep = ms => new Promise(r => setTimeout(r, ms));
			let last = -1, still = 0;
			for (let i = 0; i < 30 && still < 2; i++) { await sleep(100); still = scrollY === last ? still + 1 : 0; last = scrollY; }
			const max = Math.max(1, document.documentElement.scrollHeight - innerHeight);
			return {title: document.title, url: location.href, percent: Math.round(100 * scrollY / max)}; })()`)
		l.Action = action
		return l, err
	}
	return Location{}, fmt.Errorf("unknown navigation action %q", action)
}

type Typed struct {
	Field     string `json:"field"`
	Submitted bool   `json:"submitted"`
	URL       string `json:"url,omitempty"`
}

// ErrSensitiveField is returned instead of typing into password or payment
// fields.
var ErrSensitiveField = errors.New("voc doesn't type into password, payment or other sensitive fields; please do that yourself")

// Type enters text into the field best matching field (by label, placeholder,
// aria-label or name), or the page's search box / first text field when field
// is empty, and presses Enter if submit is set.
func (b *Browser) Type(ctx context.Context, field, text string, submit bool) (Typed, error) {
	type result struct {
		Found     bool   `json:"found"`
		Sensitive bool   `json:"sensitive"`
		Field     string `json:"field"`
	}
	js := `((want, text, submit) => {
		want = want.trim().toLowerCase();
		const visible = el => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0 && getComputedStyle(el).visibility !== 'hidden' && !el.disabled && !el.readOnly; };
		const labelOf = el => {
			const parts = [el.getAttribute('aria-label'), el.placeholder, el.name, el.id, el.title];
			if (el.id) { const l = document.querySelector('label[for="' + CSS.escape(el.id) + '"]'); if (l) parts.push(l.innerText); }
			const wrap = el.closest('label'); if (wrap) parts.push(wrap.innerText);
			return parts.filter(Boolean).join(' ').replace(/\s+/g, ' ').trim();
		};
		const fields = [...document.querySelectorAll('input, textarea, [contenteditable=true], [role=searchbox], [role=textbox], [role=combobox]')]
			.filter(el => visible(el) && !['hidden', 'checkbox', 'radio', 'submit', 'button', 'file', 'image', 'range', 'color'].includes((el.type || '').toLowerCase()));
		let best = null, bestScore = -1;
		for (const el of fields) {
			const label = labelOf(el).toLowerCase();
			let score = 0;
			if (want) {
				if (label === want) score = 4; else if (label.includes(want)) score = 3;
				else if (want.split(/\s+/).some(w => w.length > 2 && label.includes(w))) score = 1; else continue;
			} else {
				score = (el.type === 'search' || el.getAttribute('role') === 'searchbox' || /search|query|\bq\b/.test(label)) ? 2 : 1;
			}
			if (score > bestScore) { best = el; bestScore = score; }
		}
		if (!best) return {found: false};
		const type = (best.type || '').toLowerCase(), ac = (best.autocomplete || '').toLowerCase(), label = labelOf(best);
		if (type === 'password' || ac.startsWith('cc-') || ac.includes('password') || ac === 'one-time-code' ||
			/card|cvv|cvc|expir|iban|routing|account number|ssn|social security|passcode|pin\b/i.test(label))
			return {found: true, sensitive: true, field: label};
		best.focus();
		if (best.isContentEditable) { best.textContent = text; }
		else {
			const proto = best.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
			Object.getOwnPropertyDescriptor(proto, 'value').set.call(best, text); // works with React-style inputs
		}
		best.dispatchEvent(new Event('input', {bubbles: true}));
		best.dispatchEvent(new Event('change', {bubbles: true}));
		if (submit) {
			const opts = {key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true};
			best.dispatchEvent(new KeyboardEvent('keydown', opts));
			best.dispatchEvent(new KeyboardEvent('keypress', opts));
			best.dispatchEvent(new KeyboardEvent('keyup', opts));
			if (best.form) { if (best.form.requestSubmit) best.form.requestSubmit(); else best.form.submit(); }
		}
		return {found: true, field: label || best.tagName.toLowerCase()};
	})(` + jsString(field) + `, ` + jsString(text) + `, ` + fmt.Sprint(submit) + `)`
	res, err := eval[result](ctx, b, 8*time.Second, js)
	switch {
	case err != nil:
		return Typed{}, err
	case !res.Found && field != "":
		return Typed{}, fmt.Errorf("no text field matching %q on this page", field)
	case !res.Found:
		return Typed{}, errors.New("this page has no text field to type into")
	case res.Sensitive:
		return Typed{}, ErrSensitiveField
	}
	t := Typed{Field: res.Field, Submitted: submit}
	if submit {
		time.Sleep(900 * time.Millisecond)
		b.run(ctx, 5*time.Second, func(tab context.Context) error {
			var err error
			t.URL, err = chromedp.Run(tab, chromedp.Location())
			return err
		})
	}
	return t, nil
}

type Result struct {
	N     int    `json:"n"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// OpenResult opens the nth (1-based) result on the page: YouTube videos,
// Google results, or otherwise the nth substantial link in the main content.
func (b *Browser) OpenResult(ctx context.Context, n int) (Result, error) {
	if n < 1 || n > 30 {
		return Result{}, fmt.Errorf("result number %d is out of range", n)
	}
	type found struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Count int    `json:"count"`
	}
	f, err := eval[found](ctx, b, 5*time.Second, `((n) => {
		const visible = el => { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; };
		let links;
		if (location.host.endsWith('youtube.com')) {
			links = [...document.querySelectorAll('ytd-video-renderer a#video-title, ytd-rich-item-renderer a#video-title-link, ytd-compact-video-renderer a')]
				.filter(a => a.href && a.href.includes('/watch?'));
		} else if (/(^|\.)google\./.test(location.host)) {
			links = [...document.querySelectorAll('#search a h3')].map(h => h.closest('a'));
		} else {
			const root = document.querySelector('main, [role=main], article') || document.body;
			links = [...root.querySelectorAll('a[href]')].filter(a => visible(a) && a.innerText.trim().length >= 12 && !a.href.startsWith('javascript:'));
		}
		const seen = new Set();
		links = links.filter(a => a && !seen.has(a.href) && seen.add(a.href));
		const a = links[n - 1];
		if (!a) return {count: links.length};
		return {title: (a.title || a.innerText).trim().replace(/\s+/g, ' ').slice(0, 200), url: a.href, count: links.length};
	})(`+fmt.Sprint(n)+`)`)
	if err != nil {
		return Result{}, err
	}
	if f.URL == "" {
		if f.Count == 0 {
			return Result{}, errors.New("I couldn't find any results on this page")
		}
		return Result{}, fmt.Errorf("there are only %d results on this page", f.Count)
	}
	if !strings.HasPrefix(f.URL, "http://") && !strings.HasPrefix(f.URL, "https://") {
		return Result{}, fmt.Errorf("result %d isn't a web link", n)
	}
	if err := b.Navigate(ctx, f.URL); err != nil {
		return Result{}, err
	}
	return Result{N: n, Title: f.Title, URL: f.URL}, nil
}

// Where reports the open page's title and URL, without starting the browser.
func (b *Browser) Where(ctx context.Context) (title, url string, ok bool) {
	b.mu.Lock()
	tab := b.tab
	b.mu.Unlock()
	if tab == nil || tab.Err() != nil {
		return "", "", false
	}
	tctx, cancel := context.WithTimeout(tab, 300*time.Millisecond)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	type where struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	w, err := chromedp.Run(tctx, chromedp.Evaluate[where](`({title: document.title, url: location.href})`))
	if err != nil || w.URL == "" || w.URL == "about:blank" {
		return "", "", false
	}
	return w.Title, w.URL, true
}
