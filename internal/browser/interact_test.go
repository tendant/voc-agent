package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func interactServer(t *testing.T) string {
	mux := http.NewServeMux()
	long := strings.Repeat("<p>Filler paragraph for scrolling.</p>", 300)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><title>Recipes</title><main>
<form action="/search"><input type="search" name="q" placeholder="Search recipes"><button>Go</button></form>
<label for="zip">Zip code</label><input id="zip" name="zip">
<input type="password" name="pw" placeholder="Password">
<input name="cardnumber" placeholder="Card number">
<ul><li><a href="/r/1">Lemon pasta with parmesan</a></li><li><a href="/r/2">Tomato soup in twenty minutes</a></li><li><a href="/r/3">Garlic bread for beginners</a></li></ul>
%s</main>`, long)
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><title>Results for %s</title><main>results</main>`, r.URL.Query().Get("q"))
	})
	mux.HandleFunc("/r/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<!doctype html><title>Recipe %s</title><main>recipe</main>`, strings.TrimPrefix(r.URL.Path, "/r/"))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestTypeAndSubmit(t *testing.T) {
	b, _ := testBrowser(t)
	url := interactServer(t)
	ctx := context.Background()
	b.Navigate(ctx, url)
	got, err := b.Type(ctx, "", "tomato soup", true) // default: the search box
	if err != nil || !got.Submitted || !strings.Contains(got.URL, "/search?q=tomato+soup") {
		t.Fatalf("search: %+v %v", got, err)
	}
	b.Navigate(ctx, url)
	if got, err := b.Type(ctx, "zip code", "94110", false); err != nil || !strings.Contains(strings.ToLower(got.Field), "zip") {
		t.Fatalf("zip: %+v %v", got, err)
	}
	if v, _ := eval[string](ctx, b, 2e9, `document.getElementById('zip').value`); v != "94110" {
		t.Fatalf("zip value %q", v)
	}
	for _, f := range []string{"password", "card number"} {
		if _, err := b.Type(ctx, f, "secret", false); !errors.Is(err, ErrSensitiveField) {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := b.Type(ctx, "favorite color", "blue", false); err == nil {
		t.Error("missing field should error")
	}
}

func TestOpenResultAndNav(t *testing.T) {
	b, _ := testBrowser(t)
	url := interactServer(t)
	ctx := context.Background()
	b.Navigate(ctx, url)
	r, err := b.OpenResult(ctx, 2)
	if err != nil || r.Title != "Tomato soup in twenty minutes" {
		t.Fatalf("result 2: %+v %v", r, err)
	}
	l, err := b.Nav(ctx, "back")
	if err != nil || l.Title != "Recipes" {
		t.Fatalf("back: %+v %v", l, err)
	}
	if l, err := b.Nav(ctx, "forward"); err != nil || l.Title != "Recipe 2" {
		t.Fatalf("forward: %+v %v", l, err)
	}
	b.Nav(ctx, "back")
	if _, err := b.OpenResult(ctx, 9); err == nil || !strings.Contains(err.Error(), "only 3") {
		t.Fatalf("result 9: %v", err)
	}
	if l, _ := b.Nav(ctx, "scroll_down"); l.Percent <= 0 {
		t.Fatalf("scroll_down: %+v", l)
	}
	if l, _ := b.Nav(ctx, "bottom"); l.Percent < 95 {
		t.Fatalf("bottom: %+v", l)
	}
	if l, _ := b.Nav(ctx, "top"); l.Percent != 0 {
		t.Fatalf("top: %+v", l)
	}
	// Fresh browser: nowhere to go back to.
	b2, _ := testBrowser(t)
	b2.Navigate(ctx, url)
	if _, err := b2.Nav(ctx, "back"); err == nil {
		t.Fatal("back with no history should say so")
	}
}
