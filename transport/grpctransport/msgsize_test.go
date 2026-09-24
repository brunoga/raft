package grpctransport_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/internal/memnet"
	"github.com/brunoga/raft/v2/transport/grpctransport"
)

// recordingHandler captures the last request seen by each Handle* method so a
// test can assert that the payload survived the round trip intact.
type recordingHandler struct {
	snapshots chan *raft.InstallSnapshotRequest
	appends   chan *raft.AppendEntriesRequest
	timeouts  chan *raft.TimeoutNowRequest
	votes     chan *raft.RequestVoteRequest

	// appendResp, when non-nil, is returned by HandleAppendEntries.
	appendResp *raft.AppendEntriesResponse
	// appendErr, when non-nil, is returned by HandleAppendEntries.
	appendErr error
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{
		snapshots: make(chan *raft.InstallSnapshotRequest, 8),
		appends:   make(chan *raft.AppendEntriesRequest, 8),
		timeouts:  make(chan *raft.TimeoutNowRequest, 8),
		votes:     make(chan *raft.RequestVoteRequest, 8),
	}
}

func (h *recordingHandler) HandleRequestVote(_ context.Context, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	select {
	case h.votes <- req:
	default:
	}
	return &raft.RequestVoteResponse{Term: req.Term}, nil
}

func (h *recordingHandler) HandleAppendEntries(_ context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	select {
	case h.appends <- req:
	default:
	}
	if h.appendErr != nil {
		return nil, h.appendErr
	}
	if h.appendResp != nil {
		return h.appendResp, nil
	}
	return &raft.AppendEntriesResponse{Term: req.Term, Success: true}, nil
}

func (h *recordingHandler) HandleInstallSnapshot(_ context.Context, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	select {
	case h.snapshots <- req:
	default:
	}
	return &raft.InstallSnapshotResponse{Term: req.Term}, nil
}

func (h *recordingHandler) HandleTimeoutNow(_ context.Context, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	select {
	case h.timeouts <- req:
	default:
	}
	return &raft.TimeoutNowResponse{Term: req.Term}, nil
}

func (h *recordingHandler) HandleReadIndex(_ context.Context, req *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	return &raft.ReadIndexResponse{Term: req.Term}, nil
}

// pairedTransports returns a (client, server) transport pair wired so that the
// client can reach the server under the peer name "srv". Both are closed when
// the test finishes.
func pairedTransports(t *testing.T, opts ...grpctransport.Option) (client, server *grpctransport.GRPCTransport) {
	t.Helper()
	opts = append([]grpctransport.Option{grpctransport.WithInsecure()}, opts...)
	srv, err := grpctransport.Listen("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	cli, err := grpctransport.Listen("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	cli.AddPeer("srv", srv.Addr())
	return cli, srv
}

// pairedMemTransports is pairedTransports with no sockets, so the caller can
// run inside a synctest bubble: a goroutine parked in Accept on a real socket
// is not durably blocked, and one idle listener stops the bubble's clock.
//
// gRPC itself is unchanged -- the same server, the same HTTP/2, the same
// keepalive timers -- only the connection underneath it is a pipe.
func pairedMemTransports(t *testing.T, opts ...grpctransport.Option) (client, server *grpctransport.GRPCTransport) {
	t.Helper()
	nw := memnet.NewNetwork()
	const srvAddr, cliAddr = "srv:7000", "cli:7000"

	base := []grpctransport.Option{
		grpctransport.WithInsecure(),
		grpctransport.WithDialOptions(grpc.WithContextDialer(nw.DialTarget)),
	}

	srv, err := grpctransport.Listen(srvAddr,
		append(append(base, grpctransport.WithListener(nw.Listen(srvAddr))), opts...)...)
	if err != nil {
		t.Fatalf("Listen server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	cli, err := grpctransport.Listen(cliAddr,
		append(append(base, grpctransport.WithListener(nw.Listen(cliAddr))), opts...)...)
	if err != nil {
		t.Fatalf("Listen client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// "passthrough" so gRPC hands the address to the dialer as written rather
	// than asking DNS about a host called "srv".
	cli.AddPeer("srv", "passthrough:///"+srvAddr)
	return cli, srv
}

// TestInstallSnapshot_FullChunkFitsMessageLimit sends a snapshot chunk of
// exactly the size the core Raft implementation uses by default. Such a chunk
// marshals to slightly more than its payload size once the surrounding proto
// fields are added, so a transport that leaves gRPC's 4 MiB default message
// limit in place can never deliver one: every InstallSnapshot for a snapshot
// at or above the chunk size fails with ResourceExhausted and the follower
// never catches up.
func TestInstallSnapshot_FullChunkFitsMessageLimit(t *testing.T) {
	chunkSize := raft.DefaultConfig().SnapshotChunkSize
	if chunkSize <= 0 {
		t.Fatalf("unexpected default SnapshotChunkSize %d", chunkSize)
	}

	cli, srv := pairedTransports(t)

	h := newRecordingHandler()
	srv.Register("srv", h)

	data := make([]byte, chunkSize)
	for i := range data {
		data[i] = byte(i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := cli.InstallSnapshot(ctx, "srv", &raft.InstallSnapshotRequest{
		Term:              7,
		LeaderID:          "leader",
		LastIncludedIndex: 100,
		LastIncludedTerm:  7,
		Data:              data,
		Done:              true,
	})
	if err != nil {
		t.Fatalf("InstallSnapshot with a %d-byte chunk: %v", chunkSize, err)
	}
	if resp.Term != 7 {
		t.Errorf("Term = %d, want 7", resp.Term)
	}

	select {
	case got := <-h.snapshots:
		if len(got.Data) != chunkSize {
			t.Fatalf("receiver got %d bytes, want %d", len(got.Data), chunkSize)
		}
		for i := range got.Data {
			if got.Data[i] != data[i] {
				t.Fatalf("payload corrupted at offset %d", i)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never saw the snapshot chunk")
	}
}

// TestAppendEntries_LargeBatchFitsMessageLimit replicates a batch whose total
// command payload comfortably exceeds gRPC's 4 MiB default message limit. The
// core caps AppendEntries by entry count, not by bytes, so a handful of large
// commands is enough to produce a message this size in normal operation.
func TestAppendEntries_LargeBatchFitsMessageLimit(t *testing.T) {
	cli, srv := pairedTransports(t)

	h := newRecordingHandler()
	srv.Register("srv", h)

	const (
		numEntries = 8
		entrySize  = 1 << 20 // 1 MiB each => 8 MiB total
	)
	entries := make([]raft.LogEntry, numEntries)
	for i := range entries {
		cmd := make([]byte, entrySize)
		cmd[0] = byte(i)
		entries[i] = raft.LogEntry{Index: raft.Index(i + 1), Term: 3, Command: cmd}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term:     3,
		LeaderID: "leader",
		Entries:  entries,
	})
	if err != nil {
		t.Fatalf("AppendEntries with a %d MiB batch: %v", numEntries*entrySize>>20, err)
	}
	if !resp.Success {
		t.Errorf("Success = false, want true")
	}

	select {
	case got := <-h.appends:
		if len(got.Entries) != numEntries {
			t.Fatalf("receiver got %d entries, want %d", len(got.Entries), numEntries)
		}
		for i, e := range got.Entries {
			if len(e.Command) != entrySize || e.Command[0] != byte(i) {
				t.Fatalf("entry %d corrupted", i)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never saw the batch")
	}
}

// TestAppendEntries_OversizedMessageIsDistinguishable verifies that a message
// that genuinely exceeds the configured limit fails with an error the core can
// recognise, rather than an opaque transport failure indistinguishable from a
// dropped packet. Without this the leader retries the same oversized batch
// forever and the follower is wedged.
func TestAppendEntries_OversizedMessageIsDistinguishable(t *testing.T) {
	// A deliberately tiny limit so a modest payload overflows it.
	cli, srv := pairedTransports(t, grpctransport.WithMaxMessageSize(64*1024))

	srv.Register("srv", newRecordingHandler())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := cli.AppendEntries(ctx, "srv", &raft.AppendEntriesRequest{
		Term:     3,
		LeaderID: "leader",
		Entries: []raft.LogEntry{
			{Index: 1, Term: 3, Command: make([]byte, 256*1024)},
		},
	})
	if err == nil {
		t.Fatal("expected an error for an oversized AppendEntries, got nil")
	}
	if !errors.Is(err, grpctransport.ErrMessageTooLarge) {
		t.Fatalf("error %v does not match ErrMessageTooLarge", err)
	}
}

// TestWithMaxMessageSize_RaisesLimit verifies the option raises the limit on
// both the server and the client: a payload larger than the built-in default
// must succeed when the option allows it.
func TestWithMaxMessageSize_RaisesLimit(t *testing.T) {
	const limit = 96 << 20
	cli, srv := pairedTransports(t, grpctransport.WithMaxMessageSize(limit))

	h := newRecordingHandler()
	srv.Register("srv", h)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 72 MiB is over the 64 MiB built-in default but under the configured limit.
	_, err := cli.InstallSnapshot(ctx, "srv", &raft.InstallSnapshotRequest{
		Term:     1,
		LeaderID: "leader",
		Data:     make([]byte, 72<<20),
		Done:     true,
	})
	if err != nil {
		t.Fatalf("InstallSnapshot under the configured limit: %v", err)
	}
}

// TestMaxMessageBytes_ReportsTheConfiguredLimit asserts that the transport
// tells the node how large a message it can carry.
//
// The node uses this to refuse a proposal it could never replicate. A transport
// that does not answer leaves the node with no limit at all, which is how an
// oversized command reaches the log and jams it.
func TestMaxMessageBytes_ReportsTheConfiguredLimit(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		tr, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() })

		if got := tr.MaxMessageBytes(); got != grpctransport.DefaultMaxMessageSize {
			t.Errorf("MaxMessageBytes() = %d, want the default %d",
				got, grpctransport.DefaultMaxMessageSize)
		}
	})

	t.Run("configured", func(t *testing.T) {
		const limit = 8 << 20
		tr, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithMaxMessageSize(limit), grpctransport.WithInsecure())
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() })

		if got := tr.MaxMessageBytes(); got != limit {
			t.Errorf("MaxMessageBytes() = %d, want %d", got, limit)
		}
	})

	t.Run("satisfies the interface", func(t *testing.T) {
		tr, err := grpctransport.Listen("127.0.0.1:0", grpctransport.WithInsecure())
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() })

		var _ raft.MessageSizeLimiter = tr
	})
}
