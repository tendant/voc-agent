package apple

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Music controls the Music app (your library and playlists; Apple's
// scripting doesn't reach the streaming catalog).
type Music struct {
	R Runner

	mu        sync.Mutex
	cachedAt  time.Time
	cachedOn  bool
	cacheTime time.Duration
}

type Track struct {
	State  string `json:"state"` // playing | paused | stopped | not running
	Name   string `json:"name,omitempty"`
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
	Volume int    `json:"volume,omitempty"`
	Kind   string `json:"kind,omitempty"` // for Play: song | playlist | album | artist
	Action string `json:"action,omitempty"`
}

const musicStatusJS = `function run(argv) {
	const M = Application('Music');
	if (!M.running()) return JSON.stringify({state: 'not running'});
	const r = {state: M.playerState(), volume: M.soundVolume()};
	try { const t = M.currentTrack; r.name = t.name(); r.artist = t.artist(); r.album = t.album(); } catch (e) {}
	return JSON.stringify(r);
}`

// Status reports what Music is doing, without launching it.
func (m *Music) Status(ctx context.Context) (Track, error) {
	return runJSON[Track](ctx, m.R, musicStatusJS)
}

// Playing reports whether Music is playing. Cached briefly: hands-free asks
// on every utterance.
func (m *Music) Playing(ctx context.Context) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.cachedAt) < 2*time.Second {
		return m.cachedOn
	}
	t, err := m.Status(ctx)
	m.cachedOn, m.cachedAt = err == nil && t.State == "playing", time.Now()
	return m.cachedOn
}

func (m *Music) forget() {
	m.mu.Lock()
	m.cachedAt = time.Time{}
	m.mu.Unlock()
}

// MusicActions are the supported controls.
var MusicActions = []string{"play", "pause", "next", "previous", "louder", "quieter", "mute", "unmute", "shuffle_on", "shuffle_off", "status"}

const musicControlJS = `function run(argv) {
	const M = Application('Music');
	const a = argv[0];
	if (!M.running() && a !== 'play') return JSON.stringify({state: 'not running', action: a});
	switch (a) {
		case 'play': M.play(); break;
		case 'pause': M.pause(); break;
		case 'next': M.nextTrack(); break;
		case 'previous': M.previousTrack(); break;
		case 'louder': M.soundVolume = Math.min(100, M.soundVolume() + 15); break;
		case 'quieter': M.soundVolume = Math.max(0, M.soundVolume() - 15); break;
		case 'mute': M.mute = true; break;
		case 'unmute': M.mute = false; break;
		case 'shuffle_on': M.shuffleEnabled = true; break;
		case 'shuffle_off': M.shuffleEnabled = false; break;
	}
	delay(0.3);
	const r = {action: a, state: M.playerState(), volume: M.soundVolume()};
	try { const t = M.currentTrack; r.name = t.name(); r.artist = t.artist(); } catch (e) {}
	return JSON.stringify(r);
}`

// Control applies a player action.
func (m *Music) Control(ctx context.Context, action string) (Track, error) {
	ok := false
	for _, a := range MusicActions {
		ok = ok || a == action
	}
	if !ok {
		return Track{}, fmt.Errorf("unknown music action %q", action)
	}
	defer m.forget()
	t, err := runJSON[Track](ctx, m.R, musicControlJS, action)
	if err == nil && t.State == "not running" && action != "status" {
		return t, fmt.Errorf("Music isn't open")
	}
	return t, err
}

const musicPlayJS = `function run(argv) {
	const M = Application('Music');
	const q = argv[0], kind = argv[1];
	const lib = M.libraryPlaylists[0];
	const result = (t, k) => ({kind: k, state: 'playing', name: t.name(), artist: t.artist ? t.artist() : '', album: t.album ? t.album() : ''});
	if (kind === 'playlist' || kind === 'any') {
		const pls = M.userPlaylists.whose({name: {_contains: q}})();
		if (pls.length) { pls[0].play(); return JSON.stringify({kind: 'playlist', state: 'playing', name: pls[0].name()}); }
	}
	const only = {song: 'songs', artist: 'artists', album: 'albums'}[kind];
	const tracks = only ? M.search(lib, {for: q, only}) : M.search(lib, {for: q});
	if (!tracks || tracks.length === 0) throw new Error('nothing in your Music library matches "' + q + '"');
	if (kind === 'album' || kind === 'artist') { M.shuffleEnabled = kind === 'artist'; }
	tracks[0].play();
	return JSON.stringify(result(tracks[0], kind === 'any' ? 'song' : kind));
}`

// Play finds query in the library (a playlist, song, album or artist) and
// plays it.
func (m *Music) Play(ctx context.Context, query, kind string) (Track, error) {
	switch kind {
	case "", "any":
		kind = "any"
	case "song", "playlist", "album", "artist":
	default:
		return Track{}, fmt.Errorf("unknown kind %q", kind)
	}
	defer m.forget()
	return runJSON[Track](ctx, m.R, musicPlayJS, query, kind)
}
