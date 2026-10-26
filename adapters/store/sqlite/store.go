package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/panca/haythos/core/task"
)

var migrations = []string{
	`CREATE TABLE events (
		cursor  INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL,
		seq     INTEGER NOT NULL,
		type    TEXT NOT NULL,
		payload BLOB NOT NULL,
		ts      TEXT NOT NULL,
		UNIQUE (task_id, seq)
	);
	CREATE INDEX events_type ON events (type, task_id);
	CREATE TABLE ledger (
		idem_key    TEXT PRIMARY KEY,
		task_id     TEXT NOT NULL,
		tool        TEXT NOT NULL,
		target      TEXT NOT NULL,
		args_hash   TEXT NOT NULL,
		approval_id TEXT NOT NULL,
		state       TEXT NOT NULL,
		result      TEXT NOT NULL,
		updated_at  TEXT NOT NULL
	);
	CREATE INDEX ledger_state ON ledger (state);`,
}

const (
	tsFormat     = time.RFC3339Nano
	selectEvents = `SELECT cursor, task_id, seq, type, payload, ts FROM events `
	selectLedger = `SELECT idem_key, task_id, tool, target, args_hash, approval_id, state, result, updated_at FROM ledger `
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set schema version %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *Store) Append(ctx context.Context, taskID string, typ task.EventType, payload any) (task.Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return task.Event{}, fmt.Errorf("encode %s payload: %w", typ, err)
	}
	ts := time.Now().UTC()
	var seq int
	var cursor int64
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return task.Event{}, fmt.Errorf("begin append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE task_id = ?`, taskID).Scan(&seq); err != nil {
		return task.Event{}, fmt.Errorf("next seq: %w", err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO events (task_id, seq, type, payload, ts) VALUES (?, ?, ?, ?, ?)`,
		taskID, seq, string(typ), b, ts.Format(tsFormat))
	if err != nil {
		return task.Event{}, fmt.Errorf("insert event: %w", err)
	}
	if cursor, err = res.LastInsertId(); err != nil {
		return task.Event{}, fmt.Errorf("event cursor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return task.Event{}, fmt.Errorf("commit event: %w", err)
	}
	return task.Event{Cursor: cursor, TaskID: taskID, Seq: seq, Type: typ, Payload: b, TS: ts}, nil
}

func (s *Store) Load(ctx context.Context, taskID string) ([]task.Event, error) {
	return s.events(ctx, selectEvents+`WHERE task_id = ? ORDER BY seq`, taskID)
}

func (s *Store) Since(ctx context.Context, cursor int64) ([]task.Event, error) {
	return s.events(ctx, selectEvents+`WHERE cursor > ? ORDER BY cursor`, cursor)
}

func (s *Store) events(ctx context.Context, query string, arg any) ([]task.Event, error) {
	rows, err := s.db.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []task.Event
	for rows.Next() {
		var e task.Event
		var typ, ts string
		if err := rows.Scan(&e.Cursor, &e.TaskID, &e.Seq, &typ, &e.Payload, &ts); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Type = task.EventType(typ)
		if e.TS, err = time.Parse(tsFormat, ts); err != nil {
			return nil, fmt.Errorf("parse event ts: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return out, nil
}

// TasksIn lists tasks whose latest task.state event is state.
func (s *Store) TasksIn(ctx context.Context, state task.State) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.task_id FROM events e
		WHERE e.type = ? AND json_extract(e.payload, '$.state') = ?
		  AND e.seq = (SELECT MAX(seq) FROM events WHERE task_id = e.task_id AND type = ?)
		ORDER BY e.task_id`, string(task.EvState), string(state), string(task.EvState))
	if err != nil {
		return nil, fmt.Errorf("query tasks in %s: %w", state, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan task id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return ids, nil
}

func (s *Store) Get(ctx context.Context, idemKey string) (task.LedgerEntry, bool, error) {
	rows, err := s.ledgerRows(ctx, selectLedger+`WHERE idem_key = ?`, idemKey)
	if err != nil || len(rows) == 0 {
		return task.LedgerEntry{}, false, err
	}
	return rows[0], true, nil
}

func (s *Store) Put(ctx context.Context, e task.LedgerEntry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ledger (idem_key, task_id, tool, target, args_hash, approval_id, state, result, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (idem_key) DO UPDATE SET
			approval_id = excluded.approval_id, state = excluded.state,
			result = excluded.result, updated_at = excluded.updated_at`,
		e.IdemKey, e.TaskID, e.Tool, e.Target, e.ArgsHash, e.ApprovalID, string(e.State), e.Result, e.UpdatedAt.UTC().Format(tsFormat))
	if err != nil {
		return fmt.Errorf("upsert ledger %s: %w", e.IdemKey, err)
	}
	return nil
}

func (s *Store) InState(ctx context.Context, st task.LedgerState) ([]task.LedgerEntry, error) {
	return s.ledgerRows(ctx, selectLedger+`WHERE state = ? ORDER BY updated_at`, string(st))
}

func (s *Store) ledgerRows(ctx context.Context, query string, arg any) ([]task.LedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("query ledger: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []task.LedgerEntry
	for rows.Next() {
		var e task.LedgerEntry
		var st, ts string
		if err := rows.Scan(&e.IdemKey, &e.TaskID, &e.Tool, &e.Target, &e.ArgsHash, &e.ApprovalID, &st, &e.Result, &ts); err != nil {
			return nil, fmt.Errorf("scan ledger: %w", err)
		}
		e.State = task.LedgerState(st)
		if e.UpdatedAt, err = time.Parse(tsFormat, ts); err != nil {
			return nil, fmt.Errorf("parse ledger ts: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ledger: %w", err)
	}
	return out, nil
}
