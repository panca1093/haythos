package testfakes

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/panca/haythos/app"
)

type Brain struct {
	mu         sync.Mutex
	Items      []app.BrainItem
	RecallErr  error
	Queries    []string
	Limits     []int
	Remembered []app.BrainItem
	Forgotten  []string
}

func (b *Brain) Recall(_ context.Context, query string, limit int) ([]app.BrainItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Queries = append(b.Queries, query)
	b.Limits = append(b.Limits, limit)
	if b.RecallErr != nil {
		return nil, b.RecallErr
	}
	n := min(limit, len(b.Items))
	return append([]app.BrainItem(nil), b.Items[:n]...), nil
}

func (b *Brain) Remember(_ context.Context, item app.BrainItem) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Remembered = append(b.Remembered, item)
	return fmt.Sprintf("fake-%d", len(b.Remembered)), nil
}

func (b *Brain) Forget(_ context.Context, slug string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if slug == "" {
		return errors.New("fake brain: empty slug")
	}
	b.Forgotten = append(b.Forgotten, slug)
	return nil
}
