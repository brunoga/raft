package raft

import (
	"testing"
	"testing/synctest"
)

func TestStateString(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tests := []struct {
			state State
			want  string
		}{
			{Follower, "Follower"},
			{Candidate, "Candidate"},
			{Leader, "Leader"},
			{State(99), "Unknown"},
		}
		for _, tt := range tests {
			if got := tt.state.String(); got != tt.want {
				t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
			}
		}
	})
}
