package agent

import (
	"context"

	"github.com/tendant/voc-agent/internal/store"
)

// StoreRecall recalls the top two matching notes (up to 2000 characters each).
func StoreRecall(st *store.Store) func(context.Context, string) ([]Recalled, error) {
	return func(ctx context.Context, user string) ([]Recalled, error) {
		ns, err := st.RelevantNotes(ctx, user, 2, 2000)
		if err != nil {
			return nil, err
		}
		out := make([]Recalled, len(ns))
		for i, n := range ns {
			out[i] = Recalled{Path: n.Path, Title: n.Title, Body: n.Body}
		}
		return out, nil
	}
}
