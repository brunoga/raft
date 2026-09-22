package grpctransport_test

import (
	"net"
	"testing"

	"github.com/brunoga/raft/v2/transport/grpctransport"
)

// freeAddr returns an address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if closeErr := ln.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return addr
}

// TestClose_ReleasesTheAddressBeforeItReturns pins the guarantee a caller that
// rebinds depends on.
//
// GracefulStop closes the listeners the server is serving on, but Listen hands
// the listener to Serve in a goroutine. A Close arriving before that goroutine
// runs found a server with no listener, and the address stayed taken until
// Serve finally ran and discovered the server was already stopped -- tens of
// milliseconds later, with nothing to wait on. A process restarting a node in
// place saw "address already in use".
func TestClose_ReleasesTheAddressBeforeItReturns(t *testing.T) {
	// Repeated, because the old failure was a race with a goroutine that had
	// not been scheduled yet: one run could win it.
	for i := range 20 {
		addr := freeAddr(t)
		tr, err := grpctransport.Listen(addr, grpctransport.WithInsecure())
		if err != nil {
			t.Fatalf("run %d: Listen: %v", i, err)
		}
		if closeErr := tr.Close(); closeErr != nil {
			t.Fatalf("run %d: Close: %v", i, closeErr)
		}

		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("run %d: the address was still taken when Close returned: %v", i, err)
		}
		if closeErr := ln.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
}

// TestClose_IsIdempotent keeps the second close from reporting the listener it
// no longer has.
func TestClose_IsIdempotent(t *testing.T) {
	tr, err := grpctransport.Listen(freeAddr(t), grpctransport.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if closeErr := tr.Close(); closeErr != nil {
			t.Errorf("Close %d: %v", i+1, closeErr)
		}
	}
}
