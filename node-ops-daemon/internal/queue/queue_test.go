// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package queue

import "testing"

func TestApproveActionRequiresMatchingAction(t *testing.T) {
	q := New()
	q.Enqueue(Item{RequestID: "req-1", Action: "execute_rebalance"})

	if _, ok := q.ApproveAction("req-1", "execute_fee_set"); ok {
		t.Fatal("mismatched action should not approve item")
	}
	pending := q.ListPending()
	if len(pending) != 1 {
		t.Fatalf("item should remain pending, got %+v", pending)
	}
	if item, ok := q.ApproveAction("req-1", "execute_rebalance"); !ok ||
		item.Status != StatusApproved {

		t.Fatalf("matching action should approve item, got %+v ok=%v", item, ok)
	}
}

func TestRejectActionRequiresMatchingAction(t *testing.T) {
	q := New()
	q.Enqueue(Item{RequestID: "req-1", Action: "execute_rebalance"})

	if _, ok := q.RejectAction("req-1", "execute_fee_set", "no"); ok {
		t.Fatal("mismatched action should not reject item")
	}
	pending := q.ListPending()
	if len(pending) != 1 {
		t.Fatalf("item should remain pending, got %+v", pending)
	}
	if item, ok := q.RejectAction("req-1", "execute_rebalance", "no"); !ok ||
		item.Status != StatusRejected || item.Reason != "no" {

		t.Fatalf("matching action should reject item, got %+v ok=%v", item, ok)
	}
}
