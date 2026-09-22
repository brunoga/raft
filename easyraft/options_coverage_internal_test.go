package easyraft

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/discovery"
	"github.com/prometheus/client_golang/prometheus"
)

// TestOptions_CoverEveryConfigField asserts that every field of config can be
// set through an exported option.
//
// config is unexported, so options are the only way in. That is what makes
// adding a field a non-breaking change -- but it also means a field with no
// option is unreachable, and unreachable in a way nothing else notices: the
// code compiles, the field reads as its zero value at run time, and the only
// symptom is a setting that silently does not exist.
//
// Adding a field without an option fails this test.
func TestOptions_CoverEveryConfigField(t *testing.T) {
	opts := []Option{
		WithID("n1"),
		WithRaftAddr("127.0.0.1:7001"),
		WithAdvertiseRaftAddr("node1.internal:7001"),
		WithAdvertiseHTTPAddr("node1.internal:8001"),
		WithHTTPAddr("127.0.0.1:8001"),
		WithDataDir("/tmp/easyraft-options-coverage"),
		WithPeers(map[raft.NodeID]string{"n1": "127.0.0.1:7001"}),
		WithLogger(slog.Default()),
		WithSnapCount(1000),
		WithDiscovery(discovery.Static(nil), time.Second),
		WithDiscoveryAsVoter(),
		WithTLS(&tls.Config{MinVersion: tls.VersionTLS13}),
		WithPrometheus(prometheus.NewRegistry()),
		WithHTTPAuth(func(*http.Request) error { return nil }),
		WithBearerTokenAuth("token"),
		WithInsecureHTTPAcknowledged(),
		WithInsecureTransportAcknowledged(),
		WithPeerAuthorizer(func(context.Context, string) error { return nil }),
		WithHTTPTLS(&tls.Config{MinVersion: tls.VersionTLS13}),
		WithLeaseReads(),
		WithJoinAddr("http://127.0.0.1:8002"),
		WithJoinAsLearner(),
		WithLeaveOnStop(),
		WithHTTPMux(http.NewServeMux()),
		WithRaftTiming(10*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond, 200*time.Millisecond),
	}

	var c config
	for _, opt := range opts {
		opt(&c)
	}

	v := reflect.ValueOf(c)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("config field %s is still its zero value after every option was applied: "+
				"it has no option, and nothing outside this package can set it",
				v.Type().Field(i).Name)
		}
	}
}
