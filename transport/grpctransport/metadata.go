package grpctransport

import (
	"context"

	"google.golang.org/grpc/metadata"
)

const nodeIDKey = "x-raft-node-id"

// grpcMetadataFromContext extracts the addressed node ID from the incoming
// gRPC metadata. Returns the node ID string and true if found, or ("", false)
// if absent.
//
// The header names the intended recipient, not the sender, so it carries no
// authority and is only ever used to choose between handlers registered on one
// transport. Peer identity is established by TLS and checked by the authorizer
// installed with WithPeerAuthorizer.
func grpcMetadataFromContext(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get(nodeIDKey)
	if len(vals) == 0 {
		return "", false
	}
	return vals[0], true
}

// AppendNodeIDToContext adds the target node ID to the outgoing gRPC metadata
// on ctx. Every outbound RPC is stamped with it, so a transport that hosts
// several nodes can route an inbound request to the one it was addressed to.
//
// It is exported for callers that build their own gRPC clients against a
// GRPCTransport server; the transport's own send methods apply it already.
func AppendNodeIDToContext(ctx context.Context, nodeID string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, nodeIDKey, nodeID)
}
