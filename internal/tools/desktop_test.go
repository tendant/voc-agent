package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tendant/voc-agent/internal/store"
)

type fakeOpener struct{ urls, apps []string }

func (f *fakeOpener) OpenURL(_ context.Context, u string) error {
	f.urls = append(f.urls, u)
	return nil
}
func (f *fakeOpener) OpenApp(_ context.Context, a string) error {
	f.apps = append(f.apps, a)
	return nil
}

func TestSearchURL(t *testing.T) {
	for _, c := range []struct{ site, q, want string }{
		{"youtube", "lo-fi jazz", "https://www.youtube.com/results?search_query=lo-fi+jazz"},
		{"YouTube", "AC/DC & friends", "https://www.youtube.com/results?search_query=AC%2FDC+%26+friends"},
		{"wikipedia", "CAP theorem", "https://en.wikipedia.org/w/index.php?search=CAP+theorem"},
		{"maps", "coffee near me", "https://www.google.com/maps/search/coffee%20near%20me"},
	} {
		if got, err := SearchURL(c.site, c.q); err != nil || got != c.want {
			t.Errorf("%s %q: %s %v", c.site, c.q, got, err)
		}
	}
	if _, err := SearchURL("myspace", "x"); err == nil || !strings.Contains(err.Error(), "youtube") {
		t.Errorf("unknown site: %v", err)
	}
}

func TestCheckOpenURL(t *testing.T) {
	ok := map[string]string{
		"https://github.com/tendant/voc-agent": "https://github.com/tendant/voc-agent",
		"github.com/tendant":                   "https://github.com/tendant",
		"http://example.com/a?b=c":             "http://example.com/a?b=c",
	}
	for in, want := range ok {
		if got, err := CheckOpenURL(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "vscode://open?x", "http://localhost:8080",
		"http://127.0.0.1:11434/api", "http://192.168.1.1", "http://router.local", "https://intranet", "https://user:pw@example.com"} {
		if got, err := CheckOpenURL(bad); err == nil {
			t.Errorf("%q should be rejected, got %q", bad, got)
		}
	}
}

func TestDesktopTools(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(filepath.Join(t.TempDir(), "db"))
	defer st.Close()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "private"), 0o755)
	os.WriteFile(filepath.Join(dir, "private/h.md"), []byte("physical therapy with Dr. Okafor on Mondays and Thursdays"), 0o644)
	st.SyncNotes(ctx, dir)
	f := &fakeOpener{}
	r := NewRegistry(Desktop(st, f, DefaultApps)...)

	res := r.Run(ctx, Call{}, "web_search", json.RawMessage(`{"site":"youtube","query":"lo-fi jazz"}`))
	if !res.OK || len(f.urls) != 1 || f.urls[0] != "https://www.youtube.com/results?search_query=lo-fi+jazz" {
		t.Fatalf("%s %v", res.JSON, f.urls)
	}
	res = r.Run(ctx, Call{}, "web_search", json.RawMessage(`{"site":"google","query":"physical therapy with Dr. Okafor on Mondays"}`))
	if res.OK || !strings.Contains(string(res.JSON), "local-only") || len(f.urls) != 1 {
		t.Fatalf("private query should be blocked: %s", res.JSON)
	}
	res = r.Run(ctx, Call{}, "open_url", json.RawMessage(`{"url":"file:///Users"}`))
	if res.OK || len(f.urls) != 1 {
		t.Fatalf("file URL opened: %s", res.JSON)
	}
	res = r.Run(ctx, Call{}, "open_app", json.RawMessage(`{"app":"calculator"}`))
	if !res.OK || len(f.apps) != 1 || f.apps[0] != "Calculator" {
		t.Fatalf("%s %v", res.JSON, f.apps)
	}
	res = r.Run(ctx, Call{}, "open_app", json.RawMessage(`{"app":"Terminal"}`))
	if res.OK || len(f.apps) != 1 {
		t.Fatalf("Terminal opened: %s", res.JSON)
	}
}

func TestSystemOpenerCommands(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS command shapes")
	}
	var got []string
	o := &SystemOpener{run: func(_ context.Context, name string, args ...string) error {
		got = append(got, name+" "+strings.Join(args, " "))
		return nil
	}}
	o.OpenURL(context.Background(), "https://x.com")
	o.Browser = "Firefox"
	o.OpenURL(context.Background(), "https://x.com")
	o.OpenApp(context.Background(), "Music")
	want := []string{"open https://x.com", "open -a Firefox https://x.com", "open -a Music"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
}
