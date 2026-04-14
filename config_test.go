package raft

import (
	"context"
	"testing"
	"time"
)

// stubStorage, stubStateMachine, stubTransport are minimal no-op
// implementations used only in config validation tests.

type stubStorage struct{}

func (s *stubStorage) SaveHardState(_ context.Context, _ HardState) error     { return nil }
func (s *stubStorage) LoadHardState(_ context.Context) (HardState, error)     { return HardState{}, nil }
func (s *stubStorage) AppendLogEntries(_ context.Context, _ []LogEntry) error { return nil }
func (s *stubStorage) GetLogEntry(_ context.Context, _ Index) (LogEntry, error) {
	return LogEntry{}, nil
}
func (s *stubStorage) GetLogEntries(_ context.Context, _, _ Index) ([]LogEntry, error) {
	return nil, nil
}
func (s *stubStorage) FirstIndex(_ context.Context) (Index, error)                    { return 0, nil }
func (s *stubStorage) LastIndex(_ context.Context) (Index, error)                     { return 0, nil }
func (s *stubStorage) TruncateSuffix(_ context.Context, _ Index) error                { return nil }
func (s *stubStorage) TruncatePrefix(_ context.Context, _ Index) error                { return nil }
func (s *stubStorage) SaveSnapshot(_ context.Context, _ SnapshotMeta, _ []byte) error { return nil }
func (s *stubStorage) LoadSnapshot(_ context.Context) (SnapshotMeta, []byte, error) {
	return SnapshotMeta{}, nil, ErrNoSnapshot
}
func (s *stubStorage) Close() error { return nil }

type stubStateMachine struct{}

func (s *stubStateMachine) Apply(_ context.Context, _ LogEntry) ([]byte, error) { return nil, nil }
func (s *stubStateMachine) Snapshot(_ context.Context) ([]byte, error)          { return nil, nil }
func (s *stubStateMachine) Restore(_ context.Context, _ SnapshotMeta, _ []byte) error {
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
func (s *stubTransport) Close() error             { return nil }

func validConfig() Config {
	c := DefaultConfig()
	c.ID = "node1"
	c.Storage = &stubStorage{}
	c.StateMachine = &stubStateMachine{}
	c.Transport = &stubTransport{}
	return c
}

func TestDefaultConfigIsValid(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("DefaultConfig() should be valid: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"missing ID", func(c *Config) { c.ID = "" }, true},
		{"missing Storage", func(c *Config) { c.Storage = nil }, true},
		{"missing StateMachine", func(c *Config) { c.StateMachine = nil }, true},
		{"missing Transport", func(c *Config) { c.Transport = nil }, true},
		{"zero ElectionTimeoutMin", func(c *Config) { c.ElectionTimeoutMin = 0 }, true},
		{"zero ElectionTimeoutMax", func(c *Config) { c.ElectionTimeoutMax = 0 }, true},
		{"min > max", func(c *Config) {
			c.ElectionTimeoutMin = 300 * time.Millisecond
			c.ElectionTimeoutMax = 150 * time.Millisecond
		}, true},
		{"zero HeartbeatInterval", func(c *Config) { c.HeartbeatInterval = 0 }, true},
		{"timeout too small vs heartbeat", func(c *Config) {
			c.HeartbeatInterval = 100 * time.Millisecond
			c.ElectionTimeoutMin = 150 * time.Millisecond
			c.ElectionTimeoutMax = 300 * time.Millisecond
		}, true},
		{"zero MaxLogEntriesPerRPC", func(c *Config) { c.MaxLogEntriesPerRPC = 0 }, true},
		{"zero MaxInflightRPCs", func(c *Config) { c.MaxInflightRPCs = 0 }, true},
		{"TickInterval larger than HeartbeatInterval", func(c *Config) {
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
