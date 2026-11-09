package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"github.com/tendant/voc-agent/internal/policy"
	"github.com/tendant/voc-agent/internal/store"
)

// Opener opens URLs and apps on this computer.
type Opener interface {
	OpenURL(ctx context.Context, u string) error
	OpenApp(ctx context.Context, app string) error
}

// SystemOpener uses the OS launcher: `open` on macOS, `xdg-open` on Linux.
type SystemOpener struct {
	Browser string // e.g. "Firefox"; empty means the default browser
	// run executes a command; tests replace it.
	run func(ctx context.Context, name string, args ...string) error
}

func (o *SystemOpener) exec(ctx context.Context, name string, args ...string) error {
	if o.run != nil {
		return o.run(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (o *SystemOpener) OpenURL(ctx context.Context, u string) error {
	switch runtime.GOOS {
	case "darwin":
		if o.Browser != "" {
			return o.exec(ctx, "open", "-a", o.Browser, u)
		}
		return o.exec(ctx, "open", u)
	case "linux":
		return o.exec(ctx, "xdg-open", u)
	}
	return fmt.Errorf("opening URLs is not supported on %s", runtime.GOOS)
}

func (o *SystemOpener) OpenApp(ctx context.Context, app string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("opening apps is only supported on macOS")
	}
	return o.exec(ctx, "open", "-a", app)
}

// DefaultApps can be opened by voice. Terminal, System Settings and other
// apps that can change the system are deliberately absent.
var DefaultApps = []string{
	"Safari", "Firefox", "Google Chrome", "Music", "Spotify", "Podcasts", "Calendar", "Notes",
	"Reminders", "Mail", "Messages", "Maps", "Photos", "Calculator", "Clock", "Weather", "Finder", "Preview",
}

// searchSites maps a site name to its search URL prefix.
var searchSites = map[string]string{
	"youtube":    "https://www.youtube.com/results?search_query=",
	"google":     "https://www.google.com/search?q=",
	"wikipedia":  "https://en.wikipedia.org/w/index.php?search=",
	"maps":       "https://www.google.com/maps/search/",
	"duckduckgo": "https://duckduckgo.com/?q=",
	"github":     "https://github.com/search?q=",
}

// SearchURL builds the search URL for site and query.
func SearchURL(site, query string) (string, error) {
	prefix, ok := searchSites[strings.ToLower(strings.TrimSpace(site))]
	if !ok {
		return "", fmt.Errorf("unknown site %q; use one of %s", site, strings.Join(siteNames(), ", "))
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return "", errors.New("empty search query")
	}
	if strings.HasSuffix(prefix, "/") { // path-style (maps)
		return prefix + url.PathEscape(q), nil
	}
	return prefix + url.QueryEscape(q), nil
}

func siteNames() []string {
	var out []string
	for k := range searchSites {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CheckOpenURL allows only public http(s) web addresses: no file:, custom
// app schemes, javascript:, localhost or raw IPs (which could reach this
// machine's own services or the local network).
func CheckOpenURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if !strings.Contains(s, "://") {
		s = "https://" + s // "github.com/x" → https
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("not a valid URL: %v", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("only http and https links can be opened, not %q", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "":
		return "", errors.New("URL has no host")
	case host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local"):
		return "", errors.New("local addresses can't be opened by voice")
	case net.ParseIP(host) != nil:
		return "", errors.New("IP addresses can't be opened by voice; use a domain name")
	case !strings.Contains(host, "."):
		return "", fmt.Errorf("%q is not a public domain", host)
	case u.User != nil:
		return "", errors.New("URLs with embedded credentials can't be opened")
	}
	return u.String(), nil
}

// Desktop returns tools that act on this computer: web searches, links and
// apps. Every query and URL is checked against local_only notes first, so
// private text can't leave through a browser address bar either.
func Desktop(st *store.Store, o Opener, apps []string) []Tool {
	allowed := map[string]string{} // lower-case → canonical name
	for _, a := range apps {
		allowed[strings.ToLower(a)] = a
	}
	leakCheck := func(ctx context.Context, s string) error {
		if st == nil {
			return nil
		}
		private, err := st.NotesWithPrivacy(ctx, store.LocalOnly)
		if err != nil {
			return err
		}
		if v := policy.NewFingerprints(private).Check(s); v != nil {
			return fmt.Errorf("not opened: it would send text from your local-only note %q to a website", v.Note)
		}
		return nil
	}

	type searchArgs struct {
		Site  string `json:"site" desc:"Where to search" enum:"youtube,google,wikipedia,maps,duckduckgo,github"`
		Query string `json:"query" desc:"What to search for, in the user's words"`
	}
	type urlArgs struct {
		URL string `json:"url" desc:"The web address to open, e.g. https://github.com"`
	}
	type appArgs struct {
		App string `json:"app" desc:"Name of the app to open"`
	}
	appList := make([]string, 0, len(apps))
	appList = append(appList, apps...)
	sort.Strings(appList)

	return []Tool{
		Typed("web_search",
			"Open the browser on this computer with search results, e.g. YouTube videos, Google, Wikipedia, or Maps. Use it when the user asks to search for, find, look up, watch or play something on one of these sites.",
			func(ctx context.Context, _ Call, a searchArgs) (any, error) {
				u, err := SearchURL(a.Site, a.Query)
				if err != nil {
					return nil, err
				}
				if err := leakCheck(ctx, a.Query); err != nil {
					return nil, err
				}
				if err := o.OpenURL(ctx, u); err != nil {
					return nil, err
				}
				return map[string]string{"status": "opened", "site": strings.ToLower(a.Site), "query": a.Query, "url": u}, nil
			}),
		Typed("open_url",
			"Open a web page in the browser on this computer. Only http and https addresses of public websites.",
			func(ctx context.Context, _ Call, a urlArgs) (any, error) {
				u, err := CheckOpenURL(a.URL)
				if err != nil {
					return nil, err
				}
				if err := leakCheck(ctx, u); err != nil {
					return nil, err
				}
				if err := o.OpenURL(ctx, u); err != nil {
					return nil, err
				}
				return map[string]string{"status": "opened", "url": u}, nil
			}),
		Typed("open_app",
			"Open an app on this computer. Allowed apps: "+strings.Join(appList, ", ")+".",
			func(ctx context.Context, _ Call, a appArgs) (any, error) {
				name, ok := allowed[strings.ToLower(strings.TrimSpace(strings.TrimSuffix(a.App, ".app")))]
				if !ok {
					return nil, fmt.Errorf("%q is not in the list of apps voc may open (%s)", a.App, strings.Join(appList, ", "))
				}
				if err := o.OpenApp(ctx, name); err != nil {
					return nil, err
				}
				return map[string]string{"status": "opened", "app": name}, nil
			}),
	}
}
