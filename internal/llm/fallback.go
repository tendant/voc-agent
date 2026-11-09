package llm

import "context"

// Fallback uses Primary and, if it fails (network, refusal, guard), retries
// the same request on Secondary, so goal mode keeps working offline.
type Fallback struct {
	Primary, Secondary Chat
	// OnFallback is told why Primary failed; may be nil.
	OnFallback func(err error)
}

func (f Fallback) Chat(ctx context.Context, req Request, onDelta func(string)) (Message, error) {
	m, err := f.Primary.Chat(ctx, req, onDelta)
	if err == nil || ctx.Err() != nil {
		return m, err
	}
	if f.OnFallback != nil {
		f.OnFallback(err)
	}
	return f.Secondary.Chat(ctx, req, onDelta)
}
