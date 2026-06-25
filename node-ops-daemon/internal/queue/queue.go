// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package queue implements an in-memory pending-approval queue for daemon
// actions that require human review before execution.
package queue

import (
	"sync"
	"time"
)

// Status represents the lifecycle state of a queued item.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
)

// Item is a single action awaiting approval.
type Item struct {
	RequestID string
	Action    string
	Params    string // JSON-encoded
	Status    Status
	CreatedAt time.Time
	Reason    string // set on rejection
}

// Queue is a concurrency-safe in-memory store for pending items.
type Queue struct {
	mu    sync.Mutex
	items map[string]*Item
}

// New creates an empty Queue.
func New() *Queue {
	return &Queue{items: make(map[string]*Item)}
}

// Enqueue adds item to the queue with StatusPending. If an item with the
// same RequestID already exists it is overwritten.
func (q *Queue) Enqueue(item Item) {
	q.mu.Lock()
	defer q.mu.Unlock()
	item.Status = StatusPending
	q.items[item.RequestID] = &item
}

// Approve marks the item as approved. Returns the item and true on success,
// or nil and false if not found or already decided.
func (q *Queue) Approve(requestID string) (*Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	item, ok := q.items[requestID]
	if !ok || item.Status != StatusPending {
		return nil, false
	}
	item.Status = StatusApproved
	return item, true
}

// Reject marks the item as rejected with the given reason. Returns the item
// and true on success, or nil and false if not found or already decided.
func (q *Queue) Reject(requestID, reason string) (*Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	item, ok := q.items[requestID]
	if !ok || item.Status != StatusPending {
		return nil, false
	}
	item.Status = StatusRejected
	item.Reason = reason
	return item, true
}

// ListPending returns a snapshot of all items still in StatusPending.
func (q *Queue) ListPending() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Item
	for _, item := range q.items {
		if item.Status == StatusPending {
			out = append(out, *item)
		}
	}
	return out
}

// Get returns the item with the given RequestID and a found flag.
func (q *Queue) Get(requestID string) (*Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	item, ok := q.items[requestID]
	if !ok {
		return nil, false
	}
	cp := *item
	return &cp, true
}
