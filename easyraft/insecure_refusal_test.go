package easyraft_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/brunoga/raft/easyraft"
)

// TestNewStore_RefusesPlaintextTransportByDefault pins that a store with no
// WithTLS and no acknowledgement does not start. The transport is the whole
// cluster's trust boundary, so running it open has to be asked for.
func TestNewStore_RefusesPlaintextTransportByDefault(t *testing.T) {
	_, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:0"),
		easyraft.WithDataDir(t.TempDir()),
	)
	if err == nil {
		t.Fatal("NewStore with neither WithTLS nor WithInsecureTransportAcknowledged succeeded")
	}
	if !strings.Contains(err.Error(), "WithInsecureTransportAcknowledged") {
		t.Fatalf("error does not name the way out: %v", err)
	}
}

// TestNewStore_RefusesOpenHTTPAPIByDefault pins that a store which would
// serve the HTTP API with no authorization hook does not start unless the
// exposure is acknowledged, for both ways of serving it.
func TestNewStore_RefusesOpenHTTPAPIByDefault(t *testing.T) {
	for name, serve := range map[string]easyraft.Option{
		"WithHTTPAddr": easyraft.WithHTTPAddr("127.0.0.1:0"),
		"WithHTTPMux":  easyraft.WithHTTPMux(http.NewServeMux()),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := easyraft.NewStore(
				easyraft.WithID("n1"),
				easyraft.WithRaftAddr("127.0.0.1:0"),
				easyraft.WithInsecureTransportAcknowledged(),
				easyraft.WithDataDir(t.TempDir()),
				serve,
			)
			if err == nil {
				t.Fatal("NewStore serving an unauthenticated HTTP API succeeded")
			}
			if !strings.Contains(err.Error(), "WithInsecureHTTPAcknowledged") {
				t.Fatalf("error does not name the way out: %v", err)
			}
		})
	}
}

// TestNewStore_AcknowledgementsAllowIt pins that the two acknowledgements are
// what turn the refusals off, and that a store without an HTTP API needs only
// the transport one.
func TestNewStore_AcknowledgementsAllowIt(t *testing.T) {
	s, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:0"),
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithDataDir(t.TempDir()),
	)
	if err != nil {
		t.Fatalf("NewStore with the transport acknowledged and no HTTP API: %v", err)
	}
	_ = s.Stop()

	s, err = easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:0"),
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithHTTPAddr("127.0.0.1:0"),
		easyraft.WithInsecureHTTPAcknowledged(),
		easyraft.WithDataDir(t.TempDir()),
	)
	if err != nil {
		t.Fatalf("NewStore with both acknowledged: %v", err)
	}
	_ = s.Stop()
}

// TestNewManager_RefusesPlaintextTransportByDefault applies the same rule to
// the multi-group constructor.
func TestNewManager_RefusesPlaintextTransportByDefault(t *testing.T) {
	_, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:0"),
	)
	if err == nil {
		t.Fatal("NewManager with neither WithTLS nor WithInsecureTransportAcknowledged succeeded")
	}
}
