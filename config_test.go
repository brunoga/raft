package raft

import (
	"context"
	"io"
	"testing"
	"time"
)

// stubStorage, stubStateMachine, stubTransport are minimal no-op
// implementations used only in config validation tests.

type stubStorage struct{}

func (s *stubStorage) SaveHardState(_ context.Context, _ HardState) error { return nil }
func (s *stubStorage) LoadHardState(_ context.Context) (HardState, error) { return HardState{}, nil }
func (s *stubStorage) AppendLogEntries(_ context.Context, _ []LogEntry) error {
	return nil
}
func (s *stubStorage) GetLogEntry(_ context.Context, _ Index) (LogEntry, error) {
	return LogEntry{}, nil
}
func (s *stubStorage) GetLogEntries(_ context.Context, _, _ Index) ([]LogEntry, error) {
	return nil, nil
}
func (s *stubStorage) FirstIndex() (Index, error)                      { return 0, nil }
func (s *stubStorage) LastIndex() (Index, error)                       { return 0, nil }
func (s *stubStorage) TruncateSuffix(_ context.Context, _ Index) error { return nil }
func (s *stubStorage) TruncatePrefix(_ context.Context, _ Index) error { return nil }
func (s *stubStorage) SaveSnapshot(_ context.Context, _ SnapshotMeta, _ io.Reader) error {
	return nil
}
func (s *stubStorage) LoadSnapshot(_ context.Context) (SnapshotMeta, io.ReadCloser, error) {
	return SnapshotMeta{}, nil, ErrNoSnapshot
}
func (s *stubStorage) Close() error { return nil }

type stubStateMachine struct{}

func (s *stubStateMachine) Apply(_ context.Context, _ LogEntry) ([]byte, error) { return nil, nil }
func (s *stubStateMachine) Snapshot(_ context.Context, _ io.Writer) error       { return nil }
func (s *stubStateMachine) Restore(_ context.Context, _ SnapshotMeta, _ io.Reader) error {
	return nil
}

type stubTransport struct{}

func (s *stubTransport) RequestVote(_ context.Context, _ NodeID, _ *RequestVoteRequest) (*RequestVoteResponse, error) {
	return nil, nil
}
func (s *stubTransport) AppendEntries(_ context.Context, _ NodeID, _ *AppendEntriesRequest) (*AppendEntriesResponse, error) {
	return nil, nil
}
func (s *stubTransport) InstallSnapshot(_ context.Context, _ NodeID, _ *InstallSnapshotRequest) (*InstallSnapshotResponse, error) {
	return nil, nil
}
func (s *stubTransport) TimeoutNow(_ context.Context, _ NodeID, _ *TimeoutNowRequest) (*TimeoutNowResponse, error) {
	return nil, nil
}
func (s *stubTransport) ReadIndex(_ context.Context, _ NodeID, _ *ReadIndexRequest) (*ReadIndexResponse, error) {
	return nil, nil
}
func (s *stubTransport) Register(NodeID, Handler) {}
func (s *stubTransport) Unregister(NodeID)        {}
func (s *stubTransport) Close() error             { return nil }

func validConfig() Config {
	c := DefaultConfig()
	c.ID = "node1"
	c.Storage = &stubStorage{}
	c.StateMachine = &stubStateMachine{}
	c.Transport = &stubTransport{}
	return c
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(c *Config) {}, false},
		{"missing ID", func(c *Config) { c.ID = "" }, true},
		{"missing storage", func(c *Config) { c.Storage = nil }, true},
		{"missing state machine", func(c *Config) { c.StateMachine = nil }, true},
		{"missing transport", func(c *Config) { c.Transport = nil }, true},
		{"invalid heartbeat", func(c *Config) { c.HeartbeatInterval = 0 }, true},
		{"invalid election min", func(c *Config) { c.ElectionTimeoutMin = 0 }, true},
		{"invalid election max", func(c *Config) { c.ElectionTimeoutMax = 5 * time.Millisecond }, true},
		{"tick > heartbeat", func(c *Config) {
			c.TickInterval = 100 * time.Millisecond
			c.HeartbeatInterval = 50 * time.Millisecond
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidate_RejectsEqualElectionBounds pins that an election timeout with no
// randomised spread is refused.
//
// Every follower with the same timeout stands for election at the same moment
// and splits the vote, then does it again on the next attempt. A cluster can
// stay leaderless for a long time that way, with no single component looking
// broken.
func TestValidate_RejectsEqualElectionBounds(t *testing.T) {
	c := validConfig()
	c.ElectionTimeoutMin = 150 * time.Millisecond
	c.ElectionTimeoutMax = 150 * time.Millisecond

	if err := c.Validate(); err == nil {
		t.Error("Validate accepted equal election bounds, which leave no spread between followers")
	}
}

// TestValidate_RejectsNegativeSizes covers the fields where a negative value is
// meaningless and would otherwise be silently reinterpreted.
func TestValidate_RejectsNegativeSizes(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(*Config)
	}{
		{"client table size", func(c *Config) { c.MaxClientTableSize = -1 }},
		{"snapshot chunk size", func(c *Config) { c.SnapshotChunkSize = -1 }},
		{"rpc timeout", func(c *Config) { c.RPCTimeout = -time.Second }},
		{"tick interval", func(c *Config) { c.TickInterval = -time.Second }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.break_(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate accepted a negative %s", tt.name)
			}
		})
	}
}
