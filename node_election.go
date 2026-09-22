package raft

import "context"

// ---- Election -----------------------------------------------------------------

// handleRequestVote processes one RequestVote request and answers it on
// respCh, which may happen after this function has returned.
//
// A granted vote is a promise that this node will not vote for anybody else in
// this term, and the only thing that can hold the promise across a crash is
// the hard-state write. So the answer waits for it. A pre-vote changes no
// persistent state and is answered at once, which is what keeps a cluster with
// a slow disk able to hold an election at all.
func (n *Node) handleRequestVote(req *RequestVoteRequest, respCh chan rpcResponse) {
	var resp *RequestVoteResponse
	answered := false
	// The response channel holds one value and the caller reads it once.
	// Answering twice would block the event loop on the second send, which is
	// worse than whatever bug caused it.
	reply := func(r *RequestVoteResponse, err error) {
		if respCh == nil || answered {
			return
		}
		answered = true
		respCh <- rpcResponse{resp: r, err: err}
	}
	// replyWhenDurable holds the answer back until this node's term and vote
	// are on disk. Every answer below goes through it except the pre-vote,
	// because every answer states this node's term, and a term it could forget
	// in a crash is one it could make a second and different decision in.
	replyWhenDurable := func(r *RequestVoteResponse) {
		n.afterWrite(n.sendGate(0),
			func() { reply(r, nil) },
			func(err error) { reply(r, err) })
	}

	// Pre-vote: the sender is testing whether it COULD win an election in the
	// next term. We must not update any persistent state; just report whether
	// we would vote.
	//
	// This is the one answer that is not held back, and it is safe precisely
	// because it promises nothing: a pre-vote grant is not recorded anywhere,
	// so there is nothing for a crash to lose. It is also the answer that most
	// needs not to wait. Pre-vote exists to keep a node that cannot win from
	// disrupting a healthy cluster, and it can only do that if a cluster whose
	// disks are busy can still work out who should stand.
	if req.PreVote {
		resp = &RequestVoteResponse{Term: n.currentTerm}
		if !n.cfg.Voter {
			reply(resp, nil)
			return
		}
		// Grant pre-vote if: their next-term would be higher than ours, their log
		// is up-to-date, and we have not heard from a valid leader recently.
		// "Recently" is defined as: we are a follower whose election timer has
		// not yet expired — i.e. we are still receiving heartbeats from a live
		// leader. This prevents a partitioned-then-rejoined node from disrupting
		// a healthy cluster by winning a pre-vote round against nodes that know
		// about an active leader.
		logOK := n.log.isUpToDate(req.LastLogIndex, req.LastLogTerm)
		// A live leader IS the active authority — it must deny pre-votes to
		// prevent a partially-isolated node (one that can send but not receive)
		// from winning a majority pre-vote and forcing the leader to step down.
		// A follower that is still hearing from a leader also denies.
		heardFromLeader := n.state == Leader ||
			(n.state == Follower && n.electionElapsed < n.electionTimeout)
		if req.Term > n.currentTerm && logOK && !heardFromLeader {
			resp.VoteGranted = true
		}
		reply(resp, nil)
		return
	}

	// Real vote: step down if we see a higher term.
	if req.Term > n.currentTerm {
		if !n.cfg.Voter {
			n.saveTerm(req.Term, "")
			n.applyFollowerTransition("")
			replyWhenDurable(&RequestVoteResponse{Term: n.currentTerm, VoteGranted: false})
			return
		}
		// Determine the vote decision now, before stepping down, so we can
		// merge the term-update and vote-grant into a single fsync. When the
		// term increases, votedFor resets to "" (new term), so alreadyVoted is
		// always false — only the log-up-to-date check matters.
		logOK := n.log.isUpToDate(req.LastLogIndex, req.LastLogTerm)
		votedFor := NodeID("")
		if logOK {
			votedFor = req.CandidateID
		}
		n.saveTerm(req.Term, votedFor)
		n.applyFollowerTransition("") // resets election timeout unconditionally
		replyWhenDurable(&RequestVoteResponse{Term: n.currentTerm, VoteGranted: logOK})
		return
	}

	resp = &RequestVoteResponse{Term: n.currentTerm}

	// Deny if request is from an older term.
	if req.Term < n.currentTerm {
		replyWhenDurable(resp)
		return
	}

	// Grant vote if we haven't voted yet (or already voted for this candidate)
	// and the candidate's log is at least as up-to-date as ours.
	if !n.cfg.Voter {
		replyWhenDurable(resp)
		return
	}
	alreadyVoted := n.votedFor != "" && n.votedFor != req.CandidateID
	logOK := n.log.isUpToDate(req.LastLogIndex, req.LastLogTerm)
	if !alreadyVoted && logOK {
		n.saveTerm(req.Term, req.CandidateID)
		n.resetElectionTimeout()
		resp.VoteGranted = true
	}
	replyWhenDurable(resp)
}

// triggerElection starts an election. If the caller is a follower, it first
// runs a pre-vote round to avoid disrupting the cluster with unnecessary term
// increments. A candidate that already has its term incremented re-runs the
// real election directly.
func (n *Node) triggerElection() {
	if n.state == Candidate {
		// Already in a real election; just re-broadcast.
		n.becomeCandidate()
	} else {
		n.becomePreCandidate()
	}
}

// broadcastRequestVote sends RequestVote RPCs to all peers in parallel.
// preVote=true is used for the pre-vote extension (section 13): the request
// uses term = currentTerm+1 so the receiver can judge whether it would vote
// in a real election, but neither side actually increments the term yet.
func (n *Node) broadcastRequestVote(preVote bool) {
	term := n.currentTerm
	if preVote {
		term = n.currentTerm + 1
	}
	req := &RequestVoteRequest{
		GroupID:      n.cfg.GroupID,
		Term:         term,
		CandidateID:  n.cfg.ID,
		LastLogIndex: n.log.lastLogIndex(),
		LastLogTerm:  n.log.lastLogTerm(),
		PreVote:      preVote,
	}

	for _, peer := range n.cfg.Peers {
		if !peer.Voter {
			continue
		}
		p := peer.ID
		go func(p NodeID) {
			ctx, cancel := context.WithTimeout(n.stopCtx, n.rpcTimeout())
			defer cancel()

			ctx, finish := n.traceRPC(ctx, p, RPCRequestVote)
			resp, err := n.cfg.Transport.RequestVote(ctx, p, req)
			finish(err)
			if err != nil || resp == nil {
				return
			}

			select {
			case n.rpcCh <- rpcEnvelope{
				req: &voteResult{
					peerID:       p,
					term:         resp.Term,
					voteGranted:  resp.VoteGranted,
					electionTerm: req.Term,
					preVote:      preVote,
				},
			}:
			case <-n.stopCh:
			}
		}(p)
	}
}

// voteResult carries the outcome of a RequestVote RPC back to the event loop.
type voteResult struct {
	peerID       NodeID // which peer sent this response
	term         Term
	voteGranted  bool
	electionTerm Term // term at which the RPC was sent (to detect stale replies)
	preVote      bool // true when this is a pre-vote response
}

// electionWon reports whether the accumulated votes in receivedVoteSet
// constitute a winning majority. During joint consensus (jointOld != nil) both
// C_old and C_new must independently have a majority (Raft §6). Self is always
// counted for C_old; for C_new it is counted only if jointIncludeSelf is true.
func (n *Node) electionWon() bool {
	if n.jointOld == nil {
		// Normal single-config: self + voted peers must reach quorum.
		return n.hasQuorumAck(electionQuorum, n.receivedVoteSet, n.cfg.Peers, true, n.cfg.Voter)
	}
	// Joint consensus: both C_old and C_new must independently have a quorum.
	return n.hasQuorumAck(electionQuorum, n.receivedVoteSet, n.jointOld, true, n.jointSelfVoterOld) &&
		n.hasQuorumAck(electionQuorum, n.receivedVoteSet, n.jointNew, n.jointIncludeSelf, n.jointSelfVoter)
}

func (n *Node) handleVoteResult(r *voteResult) {
	if r.preVote {
		// Pre-vote: only count if we are still a PreCandidate and the reply is
		// for the correct "next term". Never update persistent state.
		if n.state != PreCandidate || r.electionTerm != n.currentTerm+1 {
			return
		}
		if r.term > n.currentTerm {
			n.becomeFollower(r.term, "")
			return
		}
		if r.voteGranted {
			n.receivedVoteSet[r.peerID] = true
			if n.electionWon() {
				n.becomeCandidate()
			}
		}
		return
	}

	// Real vote: step down if we see a higher term.
	if r.term > n.currentTerm {
		n.becomeFollower(r.term, "")
		return
	}
	if n.state != Candidate || r.term < n.currentTerm {
		return
	}
	if r.voteGranted {
		n.receivedVoteSet[r.peerID] = true
		if n.electionWon() {
			n.becomeLeader()
		}
	}
}
