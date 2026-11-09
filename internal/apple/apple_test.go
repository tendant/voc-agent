package apple

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner records calls and returns canned output.
type fakeRunner struct {
	calls []struct {
		script string
		args   []string
	}
	reply string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, script string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, struct {
		script string
		args   []string
	}{script, args})
	return []byte(f.reply), f.err
}

func TestArgsNeverSplicedIntoScripts(t *testing.T) {
	evil := `"); Application('Finder').delete(...) //`
	f := &fakeRunner{reply: `[]`}
	n := Notes{R: f}
	n.Search(context.Background(), evil, 5)
	f.reply = `{"title":"x","folder":"Notes"}`
	n.Create(context.Background(), evil, evil)
	m := &Music{R: f}
	f.reply = `{"kind":"song","state":"playing","name":"x"}`
	m.Play(context.Background(), evil, "any")
	for _, c := range f.calls {
		if strings.Contains(c.script, "Finder") {
			t.Fatal("user text ended up in a script")
		}
		if c.args[0] != evil {
			t.Fatalf("args: %q", c.args)
		}
	}
}

func TestMusicControlValidation(t *testing.T) {
	f := &fakeRunner{reply: `{"state":"not running","action":"pause"}`}
	m := &Music{R: f}
	if _, err := m.Control(context.Background(), "explode"); err == nil || len(f.calls) != 0 {
		t.Fatal("unknown action ran")
	}
	if _, err := m.Control(context.Background(), "pause"); err == nil || !strings.Contains(err.Error(), "isn't open") {
		t.Fatalf("pause while closed: %v", err)
	}
	if _, err := m.Play(context.Background(), "x", "podcast"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestPlayingIsCached(t *testing.T) {
	f := &fakeRunner{reply: `{"state":"playing","name":"So What"}`}
	m := &Music{R: f}
	if !m.Playing(context.Background()) || !m.Playing(context.Background()) || len(f.calls) != 1 {
		t.Fatalf("calls=%d", len(f.calls))
	}
	m.Control(context.Background(), "pause") // invalidates the cache
	f.reply = `{"state":"paused"}`
	if m.Playing(context.Background()) {
		t.Fatal("stale cache after a control")
	}
}

func TestNotAllowedSurfaces(t *testing.T) {
	f := &fakeRunner{err: ErrNotAllowed}
	if _, err := (Notes{R: f}).Search(context.Background(), "x", 3); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("%v", err)
	}
}
