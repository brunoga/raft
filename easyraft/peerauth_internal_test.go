package easyraft

import (
	"context"
	"crypto/tls"
	"testing"
)

// TestPeerAuthorizerFor_OnlyWhereItCanWork pins when the default peer
// authorizer is installed.
//
// It binds the node ID an RPC claims to the identity in the certificate that
// carried it, so it needs a certificate that is both present and verified.
// Only tls.RequireAndVerifyClientCert guarantees that; installing it under
// any weaker setting would refuse every inbound RPC, which is a worse
// failure than the one it guards against.
func TestPeerAuthorizerFor_OnlyWhereItCanWork(t *testing.T) {
	cases := map[tls.ClientAuthType]bool{
		tls.NoClientCert:               false,
		tls.RequestClientCert:          false,
		tls.RequireAnyClientCert:       false,
		tls.VerifyClientCertIfGiven:    false,
		tls.RequireAndVerifyClientCert: true,
	}
	for auth, want := range cases {
		t.Run(auth.String(), func(t *testing.T) {
			c := &config{TLS: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: auth}}
			if got := peerAuthorizerFor(c) != nil; got != want {
				t.Errorf("peer authorizer installed = %v, want %v", got, want)
			}
		})
	}
	if peerAuthorizerFor(&config{}) != nil {
		t.Error("a configuration with no TLS at all got a peer authorizer")
	}
}

// TestPeerAuthorizerFor_ExplicitWins pins that WithPeerAuthorizer replaces
// the default, including for a TLS configuration that would get none.
func TestPeerAuthorizerFor_ExplicitWins(t *testing.T) {
	var c config
	WithTLS(&tls.Config{MinVersion: tls.VersionTLS13})(&c)
	WithPeerAuthorizer(func(context.Context, string) error { return nil })(&c)
	if peerAuthorizerFor(&c) == nil {
		t.Fatal("an explicit peer authorizer was not installed")
	}

	var mutual config
	WithTLS(&tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert})(&mutual)
	sentinel := func(context.Context, string) error { return errSentinel }
	WithPeerAuthorizer(sentinel)(&mutual)
	if err := peerAuthorizerFor(&mutual)(context.Background(), "n1"); err != errSentinel {
		t.Error("the default authorizer displaced the explicit one under mutual TLS")
	}
}

// TestTransportOptions_CarriesTheAuthorizer pins that the authorizer reaches
// the transport rather than being computed and dropped.
func TestTransportOptions_CarriesTheAuthorizer(t *testing.T) {
	mutual := &config{TLS: &tls.Config{
		MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert,
	}}
	if got := len(transportOptions(mutual)); got != 2 {
		t.Errorf("mutual TLS produced %d transport options, want the config and the authorizer", got)
	}
	serverOnly := &config{TLS: &tls.Config{MinVersion: tls.VersionTLS13}}
	if got := len(transportOptions(serverOnly)); got != 1 {
		t.Errorf("server-only TLS produced %d transport options, want just the config", got)
	}
	if got := len(transportOptions(&config{})); got != 1 {
		t.Errorf("no TLS produced %d transport options, want just the insecure marker", got)
	}
}

var errSentinel = errTestSentinel{}

type errTestSentinel struct{}

func (errTestSentinel) Error() string { return "sentinel" }
