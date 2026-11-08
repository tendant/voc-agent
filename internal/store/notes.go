package store

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SyncNotes indexes every *.md / *.txt file under dir: new and changed files
// are (re)indexed, deleted files are removed. It returns how many changed.
func (s *Store) SyncNotes(ctx context.Context, dir string) (changed int, err error) {
	known, err := s.NoteMTimes(ctx)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && os.IsNotExist(err) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != dir {
				return fs.SkipDir
			}
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(p)); ext != ".md" && ext != ".txt" {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		info, err := d.Info()
		if err != nil {
			return err
		}
		mt := info.ModTime().UnixNano()
		if known[rel] == mt {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		changed++
		return s.UpsertNote(ctx, Note{Path: rel, Title: noteTitle(rel, string(b)), Body: string(b), MTime: mt, Privacy: Privacy(rel, string(b))})
	})
	if err != nil {
		return changed, err
	}
	for p := range known {
		if !seen[p] {
			changed++
			if err := s.DeleteNote(ctx, p); err != nil {
				return changed, err
			}
		}
	}
	return changed, nil
}

// WatchNotes re-syncs dir every interval until ctx ends. Polling keeps this
// dependency-free and is cheap for a personal notes folder.
func (s *Store) WatchNotes(ctx context.Context, dir string, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.SyncNotes(ctx, dir); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// noteTitle is the first markdown heading, else the file name.
func noteTitle(rel, body string) string {
	for _, line := range strings.Split(body, "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			return strings.TrimSpace(t)
		}
	}
	base := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	return strings.ReplaceAll(strings.ReplaceAll(base, "-", " "), "_", " ")
}

// Privacy labels.
const (
	Public    = "public"     // may be sent to the cloud as is
	Personal  = "personal"   // may be sent after redaction, with approval
	LocalOnly = "local_only" // never leaves this machine
)

// Privacy decides a note's label: a "privacy:" line in leading front matter
// wins; otherwise notes under private/ are local_only, under public/ are
// public, and everything else is personal.
func Privacy(rel, body string) string {
	if strings.HasPrefix(body, "---") {
		lines := strings.Split(body, "\n")
		for _, l := range lines[1:min(len(lines), 20)] {
			l = strings.TrimSpace(l)
			if l == "---" {
				break
			}
			if v, ok := strings.CutPrefix(l, "privacy:"); ok {
				switch v = strings.Trim(strings.TrimSpace(v), `"'`); v {
				case Public, Personal, LocalOnly:
					return v
				case "private", "local", "local-only":
					return LocalOnly
				}
			}
		}
	}
	switch {
	case strings.HasPrefix(rel, "private/") || strings.Contains(rel, "/private/"):
		return LocalOnly
	case strings.HasPrefix(rel, "public/"):
		return Public
	}
	return Personal
}
