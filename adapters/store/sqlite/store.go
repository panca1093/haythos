package sqlite

import (
	"context"
	"errors"

	"github.com/panca/haythos/core/task"
)

var ErrNotImplemented = errors.New("not implemented")

type Store struct{}

func Open(path string) (*Store, error) { return nil, ErrNotImplemented }

func (s *Store) Close() error { return ErrNotImplemented }

func (s *Store) Append(ctx context.Context, taskID string, typ task.EventType, payload any) (task.Event, error) {
	return task.Event{}, ErrNotImplemented
}

func (s *Store) Load(ctx context.Context, taskID string) ([]task.Event, error) {
	return nil, ErrNotImplemented
}

func (s *Store) Since(ctx context.Context, cursor int64) ([]task.Event, error) {
	return nil, ErrNotImplemented
}

func (s *Store) TasksIn(ctx context.Context, state task.State) ([]string, error) {
	return nil, ErrNotImplemented
}

func (s *Store) Get(ctx context.Context, idemKey string) (task.LedgerEntry, bool, error) {
	return task.LedgerEntry{}, false, ErrNotImplemented
}

func (s *Store) Put(ctx context.Context, e task.LedgerEntry) error { return ErrNotImplemented }

func (s *Store) InState(ctx context.Context, st task.LedgerState) ([]task.LedgerEntry, error) {
	return nil, ErrNotImplemented
}
