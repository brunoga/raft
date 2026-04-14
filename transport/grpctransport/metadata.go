package grpctransport

import (
	"context"

	"google.golang.org/grpc/metadata"
)

const nodeIDKey = "x-raft-node-id"

// grpcMetadataFromContext extracts the node ID from the incoming gRPC metadata.
// Returns the node ID string and true if found, or ("", false) if absent.
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

// AppendNodeIDToContext adds the node ID to the outgoing gRPC metadata on ctx.
// Call this on the client side when one transport serves multiple nodes.
func AppendNodeIDToContext(ctx context.Context, nodeID string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, nodeIDKey, nodeID)
}
