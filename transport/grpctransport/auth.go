package grpctransport

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// PeerAuthorizer decides whether the peer on the other end of ctx may act as
// claimedNodeID. It is called once per inbound request — and once per entry of
// a batched heartbeat — with the node ID the request claims to originate from:
// LeaderID for AppendEntries, InstallSnapshot and TimeoutNow, CandidateID for
// RequestVote.
//
// Returning nil accepts the request. Returning an error rejects it; the caller
// sees codes.PermissionDenied and, on the far side, an error wrapping
// ErrUnauthorizedPeer.
//
// Implementations must be safe for concurrent use and must not block: they run
// on the RPC's own goroutine, ahead of the Raft handler.
type PeerAuthorizer func(ctx context.Context, claimedNodeID string) error

// MTLSPeerAuthorizer returns a PeerAuthorizer that accepts a request only when
// the claimed node ID matches an identity in the peer's verified TLS
// certificate. It is the intended companion to WithTLSConfig configured for
// mutual TLS (ClientAuth: tls.RequireAndVerifyClientCert).
//
// By default the claimed node ID must equal the certificate's Common Name, one
// of its DNS SANs, or one of its URI SANs. Pass a non-nil identities function
// to derive the acceptable node IDs differently — for example to strip a
// domain suffix, or to read them from a custom extension:
//
//	grpctransport.MTLSPeerAuthorizer(func(c *x509.Certificate) []string {
//	    return []string{strings.TrimSuffix(c.Subject.CommonName, ".raft.example")}
//	})
//
// Without TLS there is no verified certificate and every request is rejected,
// which is the safe outcome: an authorizer configured on a plaintext listener
// would otherwise authorize nothing at all while appearing to.
func MTLSPeerAuthorizer(identities func(*x509.Certificate) []string) PeerAuthorizer {
	if identities == nil {
		identities = defaultCertIdentities
	}
	return func(ctx context.Context, claimedNodeID string) error {
		cert, err := verifiedPeerCertificate(ctx)
		if err != nil {
			return err
		}
		for _, id := range identities(cert) {
			if id != "" && id == claimedNodeID {
				return nil
			}
		}
		return fmt.Errorf("certificate %q does not authorize node ID %q",
			cert.Subject.CommonName, claimedNodeID)
	}
}

// defaultCertIdentities returns the node IDs a certificate vouches for: its
// Common Name, its DNS SANs, and its URI SANs.
func defaultCertIdentities(cert *x509.Certificate) []string {
	ids := make([]string, 0, 1+len(cert.DNSNames)+len(cert.URIs))
	if cn := cert.Subject.CommonName; cn != "" {
		ids = append(ids, cn)
	}
	ids = append(ids, cert.DNSNames...)
	for _, u := range cert.URIs {
		ids = append(ids, u.String())
	}
	return ids
}

// verifiedPeerCertificate returns the leaf certificate the TLS handshake
// verified for the calling peer.
func verifiedPeerCertificate(ctx context.Context) (*x509.Certificate, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("no peer information on the request context")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, fmt.Errorf("peer %v is not authenticated with TLS", p.Addr)
	}
	// VerifiedChains is populated only when the certificate was actually
	// verified against the configured CAs; PeerCertificates alone proves
	// nothing, since any self-signed certificate ends up there.
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return nil, fmt.Errorf("peer %v presented no verified certificate chain", p.Addr)
	}
	return tlsInfo.State.VerifiedChains[0][0], nil
}

// authorize validates and authorizes the node ID an inbound request claims to
// come from. rpc names the RPC for the error message; claimed is the LeaderID
// or CandidateID carried by the request.
//
// Validation that a claimed ID is well-formed runs whether or not an
// authorizer is installed, because an over-long ID is malformed under any
// configuration. The empty-ID check is part of strict validation (implied by
// WithPeerAuthorizer) so that a bare transport driven directly by tests with
// partially-filled requests keeps working.
func (t *GRPCTransport) authorize(ctx context.Context, rpc, claimed string) error {
	if len(claimed) > maxNodeIDLen {
		return status.Errorf(codes.InvalidArgument,
			"%s: node ID is %d bytes, exceeding the %d byte limit", rpc, len(claimed), maxNodeIDLen)
	}
	if t.strictValidation && claimed == "" {
		return status.Errorf(codes.InvalidArgument, "%s: request carries no node ID", rpc)
	}
	if t.peerAuth == nil {
		return nil
	}
	if err := t.peerAuth(ctx, claimed); err != nil {
		t.logger.LogAttrs(ctx, slog.LevelWarn,
			"rejected an unauthorized request",
			slog.String("rpc", rpc),
			slog.String("claimed_node_id", claimed),
			slog.Any("err", err),
		)
		return status.Errorf(codes.PermissionDenied,
			"%s: peer is not authorized as %q: %v", rpc, claimed, err)
	}
	return nil
}
