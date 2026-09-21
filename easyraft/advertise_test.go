package easyraft_test

import (
	"strings"
	"testing"

	"github.com/brunoga/raft/easyraft"
)

// TestJoin_RefusesAnAddressPeersCannotDial is the failure that made three of
// the repository's own example clusters unable to start.
//
// A joining node hands the cluster the address it wants to be dialled back on.
// Passing the bind address straight through means passing ":7002", which names
// no host: the far end rejects it with 400, the joiner retries for thirty
// seconds, and the operator sees "join timed out" with nothing pointing at the
// address they typed.
//
// Refusing at construction puts the error in front of them while they are
// still looking at the command.
func TestJoin_RefusesAnAddressPeersCannotDial(t *testing.T) {
	cases := []struct {
		name string
		addr string
	}{
		{"port only", ":7002"},
		{"wildcard v4", "0.0.0.0:7002"},
		{"wildcard v6", "[::]:7002"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := easyraft.NewStore(
				easyraft.WithID("n2"),
				easyraft.WithRaftAddr(tc.addr),
				easyraft.WithDataDir(t.TempDir()),
				easyraft.WithJoinAddr("127.0.0.1:8001"),
				easyraft.WithInsecureTransportAcknowledged(),
			)
			if err == nil {
				t.Fatalf("NewStore accepted %q as an address to advertise while joining", tc.addr)
			}
			if !strings.Contains(err.Error(), "WithAdvertiseRaftAddr") {
				t.Errorf("error = %v; it should name the option that fixes it", err)
			}
		})
	}
}

// TestJoin_AcceptsARoutableAddress checks the fix is not simply "refuse
// everything": a bind address that already names a reachable host needs no
// extra configuration, which is the common case for a local cluster.
func TestJoin_AcceptsARoutableAddress(t *testing.T) {
	s, err := easyraft.NewStore(
		easyraft.WithID("n2"),
		easyraft.WithRaftAddr("127.0.0.1:0"),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithJoinAddr("127.0.0.1:8001"),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewStore rejected a routable bind address: %v", err)
	}
	_ = s.Stop()
}

// TestJoin_AdvertiseOverridesAnUnroutableBind covers the deployment the bind
// address cannot express: a node listening on every interface, which has to be
// told what to call itself because "0.0.0.0:7002" is not an answer.
func TestJoin_AdvertiseOverridesAnUnroutableBind(t *testing.T) {
	s, err := easyraft.NewStore(
		easyraft.WithID("n2"),
		easyraft.WithRaftAddr("0.0.0.0:0"),
		easyraft.WithAdvertiseRaftAddr("node2.internal:7002"),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithJoinAddr("127.0.0.1:8001"),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewStore rejected a wildcard bind with an explicit advertise address: %v", err)
	}
	_ = s.Stop()
}

// TestNoJoin_DoesNotRequireARoutableAddress checks that a node which is not
// joining anything is left alone. A single-node cluster, or one configured
// with a static peer list, never tells a peer where to find it through this
// path, and demanding an address it does not need would break working setups.
func TestNoJoin_DoesNotRequireARoutableAddress(t *testing.T) {
	s, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(":0"),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatalf("NewStore rejected a port-only bind address with no join configured: %v", err)
	}
	_ = s.Stop()
}
