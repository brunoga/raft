package easyraft

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/brunoga/raft/v2"
)

// TestStatusForError_SeparatesRetryableFromPermanent pins the classification of
// the errors the Raft layer raises.
//
// 500 is the default, and the default is wrong for most of them in one of two
// ways. A client retries a 500, so a permanent error returned as one is
// retried forever: ErrObsoleteSeqNum can never succeed on a retry, because the
// client has to advance its sequence number instead. And a transient error
// returned as 500 reads as "this service is broken" when the honest answer is
// "come back in a moment": ErrWriteBacklogFull means the disk is behind and
// the same request will work shortly.
func TestStatusForError_SeparatesRetryableFromPermanent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		// Permanent: the request will not succeed as sent.
		{"obsolete sequence number", raft.ErrObsoleteSeqNum, http.StatusConflict},
		{"config change in progress", raft.ErrConfigChangeInProgress, http.StatusConflict},
		{"member not caught up", raft.ErrMemberNotCaughtUp, http.StatusConflict},
		{"key exists", ErrKeyExists, http.StatusConflict},
		{"proposal too large", raft.ErrProposalTooLarge, http.StatusRequestEntityTooLarge},
		{"key not found", ErrKeyNotFound, http.StatusNotFound},
		{"not a member", raft.ErrNotMember, http.StatusNotFound},

		// Transient: the node recovers and the same request will work.
		{"write backlog full", raft.ErrWriteBacklogFull, http.StatusServiceUnavailable},
		{"node failed", raft.ErrNodeFailed, http.StatusServiceUnavailable},
		{"lease expired", raft.ErrLeaseExpired, http.StatusServiceUnavailable},
		{"stopped", raft.ErrStopped, http.StatusServiceUnavailable},
		{"manager stopping", raft.ErrManagerStopping, http.StatusServiceUnavailable},

		{"deadline", context.DeadlineExceeded, http.StatusRequestTimeout},
		{"unrecognised", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusForError(tc.err); got != tc.want {
				t.Errorf("statusForError(%v) = %d, want %d", tc.err, got, tc.want)
			}
			// These arrive wrapped: the propose path adds context on the way up.
			wrapped := fmt.Errorf("propose: %w", tc.err)
			if got := statusForError(wrapped); got != tc.want {
				t.Errorf("statusForError(wrapped %v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
