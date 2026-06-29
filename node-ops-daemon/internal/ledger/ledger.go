// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package ledger provides an append-only SQLite audit log of every action the
// daemon considers or executes. Only INSERTs are issued; rows are never
// updated or deleted.
package ledger

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no CGO required
)

const driverName = "sqlite"

// Entry is a single immutable record in the ledger.
type Entry struct {
	ID        int64
	RequestID string
	Action    string
	Params    string // JSON-encoded params
	Status    string // "accepted" | "pending" | "executed" | "rejected" | "failed" | "ok"
	Reason    string // human-readable explanation (empty for success)
	CreatedAt time.Time
}

// QueryOptions filters audit ledger entries.
type QueryOptions struct {
	RequestID   string
	Action      string
	Status      string
	Limit       int
	Offset      int
	NewestFirst bool
}

// Ledger is a write-only audit log backed by SQLite.
type Ledger struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite ledger at path and ensures the schema is
// present. The caller is responsible for calling Close when finished.
func Open(path string) (*Ledger, error) {
	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := ensureSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger schema: %w", err)
	}
	return &Ledger{db: db}, nil
}

// ensureSchema creates the actions table if it does not already exist.
// This is the only DDL ever issued against the ledger.
func ensureSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS actions (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT    NOT NULL,
			action     TEXT    NOT NULL,
			params     TEXT    NOT NULL DEFAULT '{}',
			status     TEXT    NOT NULL,
			reason     TEXT    NOT NULL DEFAULT '',
			created_at TEXT    NOT NULL
		)`)
	return err
}

// Record appends an Entry to the ledger. It never updates existing rows.
func (l *Ledger) Record(e Entry) error {
	params := e.Params
	if params == "" {
		params = "{}"
	}
	_, err := l.db.Exec(
		`INSERT INTO actions (request_id, action, params, status, reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.RequestID,
		e.Action,
		params,
		e.Status,
		e.Reason,
		e.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("ledger record: %w", err)
	}
	return nil
}

// List returns all entries in insertion order. For testing and diagnostics only.
func (l *Ledger) List() ([]Entry, error) {
	return l.Query(QueryOptions{})
}

// Query returns entries matching opts. Results are ordered by insertion id,
// ascending by default or descending when NewestFirst is set.
func (l *Ledger) Query(opts QueryOptions) ([]Entry, error) {
	query := `SELECT id, request_id, action, params, status, reason, created_at
		 FROM actions`
	var where []string
	var args []any
	if opts.RequestID != "" {
		where = append(where, "request_id = ?")
		args = append(args, opts.RequestID)
	}
	if opts.Action != "" {
		where = append(where, "action = ?")
		args = append(args, opts.Action)
	}
	if opts.Status != "" {
		where = append(where, "status = ?")
		args = append(args, opts.Status)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	order := "ASC"
	if opts.NewestFirst {
		order = "DESC"
	}
	query += " ORDER BY id " + order
	if opts.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, opts.Limit)
		if opts.Offset > 0 {
			query += " OFFSET ?"
			args = append(args, opts.Offset)
		}
	} else if opts.Offset > 0 {
		query += " LIMIT -1 OFFSET ?"
		args = append(args, opts.Offset)
	}

	rows, err := l.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var ts string
		if err := rows.Scan(&e.ID, &e.RequestID, &e.Action, &e.Params,
			&e.Status, &e.Reason, &ts); err != nil {

			return nil, err
		}
		createdAt, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return nil, fmt.Errorf("parse ledger timestamp %q: %w", ts, err)
		}
		e.CreatedAt = createdAt
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Close releases the database handle.
func (l *Ledger) Close() error {
	return l.db.Close()
}
