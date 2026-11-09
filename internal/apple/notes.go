package apple

import (
	"context"
	"strconv"
)

// Notes reads and creates Apple Notes. It never edits or deletes.
type Notes struct{ R Runner }

type NoteHit struct {
	Title    string `json:"title"`
	Folder   string `json:"folder"`
	Modified string `json:"modified"`
	Snippet  string `json:"snippet"`
}

type Note struct {
	Title     string `json:"title"`
	Folder    string `json:"folder"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// MaxNoteText bounds how much of a note goes to the local model.
const MaxNoteText = 6000

const notesSearchJS = `function run(argv) {
	const Notes = Application('Notes');
	const q = argv[0], limit = parseInt(argv[1], 10);
	const find = s => Notes.notes.whose({_or: [{name: {_contains: s}}, {plaintext: {_contains: s}}]})();
	let found = find(q);
	if (found.length === 0) { // fall back to the longest word
		const w = q.split(/\s+/).filter(x => x.length > 3).sort((a, b) => b.length - a.length)[0];
		if (w) found = find(w);
	}
	const hits = found.map(n => ({n, d: n.modificationDate()})).sort((a, b) => b.d - a.d).slice(0, limit);
	return JSON.stringify(hits.map(({n, d}) => {
		const text = n.plaintext();
		const i = Math.max(0, text.toLowerCase().indexOf(q.toLowerCase()));
		return {title: n.name(), folder: n.container().name(), modified: d.toISOString().slice(0, 10),
			snippet: text.slice(Math.max(0, i - 60), i + 160).replace(/\s+/g, ' ').trim()};
	}));
}`

// Search finds notes whose title or text contains query, newest first.
func (n Notes) Search(ctx context.Context, query string, limit int) ([]NoteHit, error) {
	hits, err := runJSON[[]NoteHit](ctx, n.R, notesSearchJS, query, strconv.Itoa(limit))
	if hits == nil {
		hits = []NoteHit{}
	}
	return hits, err
}

const notesReadJS = `function run(argv) {
	const Notes = Application('Notes');
	const t = argv[0], max = parseInt(argv[1], 10);
	let found = Notes.notes.whose({name: t})();
	if (found.length === 0) found = Notes.notes.whose({name: {_contains: t}})();
	if (found.length === 0) throw new Error('no note titled "' + t + '"');
	const n = found.sort((a, b) => b.modificationDate() - a.modificationDate())[0];
	const text = n.plaintext();
	return JSON.stringify({title: n.name(), folder: n.container().name(), text: text.slice(0, max), truncated: text.length > max});
}`

// Read returns the newest note whose title matches.
func (n Notes) Read(ctx context.Context, title string) (Note, error) {
	return runJSON[Note](ctx, n.R, notesReadJS, title, strconv.Itoa(MaxNoteText))
}

const notesCreateJS = `function run(argv) {
	const Notes = Application('Notes');
	const esc = s => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
	const title = argv[0], body = argv[1];
	const html = '<div><h1>' + esc(title) + '</h1></div>' + body.split('\n').map(l => '<div>' + (esc(l) || '<br>') + '</div>').join('');
	const note = Notes.Note({body: html});
	Notes.defaultAccount().defaultFolder().notes.push(note);
	return JSON.stringify({title: note.name(), folder: note.container().name()});
}`

// Create makes a new note in the default folder.
func (n Notes) Create(ctx context.Context, title, body string) (NoteHit, error) {
	return runJSON[NoteHit](ctx, n.R, notesCreateJS, title, body)
}
