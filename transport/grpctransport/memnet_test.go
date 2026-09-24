package grpctransport_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/internal/memnet"
	"github.com/brunoga/raft/v2/transport/grpctransport"
)

// echoHandler answers AppendEntries and nothing else.
type echoHandler struct{ raft.Handler }

func (echoHandler) HandleAppendEntries(_ context.Context, req *raft.AppendEntriesRequest) (
	*raft.AppendEntriesResponse, error,
) {
	return &raft.AppendEntriesResponse{Term: req.Term, Success: true}, nil
}

// TestListener_OverMemnetInABubble is the whole point of WithListener: a real
// gRPC transport, with no socket anywhere, running inside a synctest bubble.
// A real listener parked in Accept would stop the bubble's clock and this
// would hang instead of passing.
func TestListener_OverMemnetInABubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nw := memnet.NewNetwork()
		const a, b = "n1:7000", "n2:7000"

		dial := grpctransport.WithDialOptions(grpc.WithContextDialer(nw.DialTarget))
		trA, err := grpctransport.Listen(a,
			grpctransport.WithListener(nw.Listen(a)), grpctransport.WithInsecure(), dial)
		if err != nil {
			t.Fatalf("Listen(%s): %v", a, err)
		}
		defer func() { _ = trA.Close() }()

		trB, err := grpctransport.Listen(b,
			grpctransport.WithListener(nw.Listen(b)), grpctransport.WithInsecure(), dial)
		if err != nil {
			t.Fatalf("Listen(%s): %v", b, err)
		}
		defer func() { _ = trB.Close() }()

		trB.Register("n2", echoHandler{})
		// "passthrough" so gRPC hands the address to the dialer as written
		// instead of asking DNS about a host called "n2".
		trA.AddPeer("n2", "passthrough:///"+b)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := trA.AppendEntries(ctx, "n2", &raft.AppendEntriesRequest{Term: 7})
		if err != nil {
			t.Fatalf("AppendEntries over memnet: %v", err)
		}
		if !resp.Success || resp.Term != 7 {
			t.Errorf("response was %+v, want success at term 7", resp)
		}

		// And the clock still moves with both transports idle, which is what
		// a real listener would have prevented.
		start := time.Now()
		time.Sleep(time.Minute)
		if elapsed := time.Since(start); elapsed != time.Minute {
			t.Errorf("fake clock advanced %v, want %v", elapsed, time.Minute)
		}
	})
}
