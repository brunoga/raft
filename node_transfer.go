package raft

import (
	"context"
	"fmt"
)

// ---- Leadership transfer ----------------------------------------------------

// leadershipTransferMsg carries a TransferLeadership request into the event
// loop.
type leadershipTransferMsg struct {
	target NodeID
	respCh chan<- error
}

// handleTimeoutNow handles a TimeoutNow RPC sent by the current leader to
// trigger an immediate election on this node, skipping the pre-vote round.
func (n *Node) handleTimeoutNow(req *TimeoutNowRequest) (*TimeoutNowResponse, error) {
	if req.Term < n.currentTerm {
		return &TimeoutNowResponse{Term: n.currentTerm}, nil
	}
	if req.Term > n.currentTerm {
		n.becomeFollower(req.Term, "")
	}
	// Skip pre-vote: we were explicitly told to start an election immediately.
	n.becomeCandidate()
	return &TimeoutNowResponse{Term: n.currentTerm}, nil
}

// handleLeadershipTransfer initiates a leadership transfer to msg.target.
// It catches the target up (if needed) and then sends it a TimeoutNow RPC.
func (n *Node) handleLeadershipTransfer(msg leadershipTransferMsg) {
	if n.state != Leader {
		msg.respCh <- &NotLeaderError{Leader: n.leaderID}
		return
	}
	if n.transferTarget != "" {
		msg.respCh <- ErrLeadershipTransferInProgress
		return
	}
	// Validate target is a known voting peer.
	var targetPeer *PeerConfig
	for _, p := range n.cfg.Peers {
		if p.ID == msg.target {
			targetPeer = &p
			break
		}
	}
	if targetPeer == nil {
		msg.respCh <- fmt.Errorf("raft: unknown transfer target %q", msg.target)
		return
	}
	if !targetPeer.Voter {
		msg.respCh <- fmt.Errorf("raft: cannot transfer leadership to non-voting member %q", msg.target)
		return
	}

	n.transferTarget = msg.target
	n.transferElapsed = 0
	msg.respCh <- nil // accepted

	// If the target is already caught up, fire TimeoutNow immediately.
	if n.matchIndex[msg.target] >= n.log.lastLogIndex() {
		n.sendTimeoutNow(msg.target)
		n.transferTarget = ""
	} else {
		// Replicate to accelerate catch-up; TimeoutNow fires in handleAppendResult.
		n.replicateToPeer(msg.target)
	}
}

// sendTimeoutNow sends a TimeoutNow RPC to target in a background goroutine.
// We do not need to track the response: if the target wins an election it will
// broadcast a higher-term heartbeat and this node will step down naturally.
func (n *Node) sendTimeoutNow(target NodeID) {
	req := &TimeoutNowRequest{GroupID: n.cfg.GroupID, Term: n.currentTerm, LeaderID: n.cfg.ID}
	go func(p NodeID, r *TimeoutNowRequest) {
		ctx, cancel := context.WithTimeout(n.stopCtx,
			n.cfg.ElectionTimeoutMin)
		defer cancel()
		finish := n.traceRPC(p, "TimeoutNow")
		_, err := n.cfg.Transport.TimeoutNow(ctx, p, r)
		finish(err)
	}(target, req)
}
