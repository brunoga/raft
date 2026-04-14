package grpctransport

import (
	"github.com/brunoga/raft"
	pb "github.com/brunoga/raft/transport/grpctransport/raftpb"
)

// ---- RequestVote -----------------------------------------------------------

func rvReqToProto(r *raft.RequestVoteRequest) *pb.RequestVoteRequest {
	return &pb.RequestVoteRequest{
		Term:         uint64(r.Term),
		CandidateId:  string(r.CandidateID),
		LastLogIndex: uint64(r.LastLogIndex),
		LastLogTerm:  uint64(r.LastLogTerm),
		PreVote:      r.PreVote,
	}
}

func rvReqFromProto(p *pb.RequestVoteRequest) *raft.RequestVoteRequest {
	return &raft.RequestVoteRequest{
		Term:         raft.Term(p.Term),
		CandidateID:  raft.NodeID(p.CandidateId),
		LastLogIndex: raft.Index(p.LastLogIndex),
		LastLogTerm:  raft.Term(p.LastLogTerm),
		PreVote:      p.PreVote,
	}
}

func rvRespToProto(r *raft.RequestVoteResponse) *pb.RequestVoteResponse {
	return &pb.RequestVoteResponse{
		Term:        uint64(r.Term),
		VoteGranted: r.VoteGranted,
	}
}

func rvRespFromProto(p *pb.RequestVoteResponse) *raft.RequestVoteResponse {
	return &raft.RequestVoteResponse{
		Term:        raft.Term(p.Term),
		VoteGranted: p.VoteGranted,
	}
}

// ---- AppendEntries ---------------------------------------------------------

func aeReqToProto(r *raft.AppendEntriesRequest) *pb.AppendEntriesRequest {
	entries := make([]*pb.LogEntry, len(r.Entries))
	for i, e := range r.Entries {
		entries[i] = &pb.LogEntry{
			Index:   uint64(e.Index),
			Term:    uint64(e.Term),
			Command: e.Command,
		}
	}
	return &pb.AppendEntriesRequest{
		Term:         uint64(r.Term),
		LeaderId:     string(r.LeaderID),
		PrevLogIndex: uint64(r.PrevLogIndex),
		PrevLogTerm:  uint64(r.PrevLogTerm),
		Entries:      entries,
		LeaderCommit: uint64(r.LeaderCommit),
		ReadBarrier:  r.ReadBarrier,
	}
}

func aeReqFromProto(p *pb.AppendEntriesRequest) *raft.AppendEntriesRequest {
	entries := make([]raft.LogEntry, len(p.Entries))
	for i, e := range p.Entries {
		entries[i] = raft.LogEntry{
			Index:   raft.Index(e.Index),
			Term:    raft.Term(e.Term),
			Command: e.Command,
		}
	}
	return &raft.AppendEntriesRequest{
		Term:         raft.Term(p.Term),
		LeaderID:     raft.NodeID(p.LeaderId),
		PrevLogIndex: raft.Index(p.PrevLogIndex),
		PrevLogTerm:  raft.Term(p.PrevLogTerm),
		Entries:      entries,
		LeaderCommit: raft.Index(p.LeaderCommit),
		ReadBarrier:  p.ReadBarrier,
	}
}

func aeRespToProto(r *raft.AppendEntriesResponse) *pb.AppendEntriesResponse {
	return &pb.AppendEntriesResponse{
		Term:          uint64(r.Term),
		Success:       r.Success,
		ConflictIndex: uint64(r.ConflictIndex),
		ConflictTerm:  uint64(r.ConflictTerm),
	}
}

func aeRespFromProto(p *pb.AppendEntriesResponse) *raft.AppendEntriesResponse {
	return &raft.AppendEntriesResponse{
		Term:          raft.Term(p.Term),
		Success:       p.Success,
		ConflictIndex: raft.Index(p.ConflictIndex),
		ConflictTerm:  raft.Term(p.ConflictTerm),
	}
}

// ---- InstallSnapshot -------------------------------------------------------

func isReqToProto(r *raft.InstallSnapshotRequest) *pb.InstallSnapshotRequest {
	return &pb.InstallSnapshotRequest{
		Term:              uint64(r.Term),
		LeaderId:          string(r.LeaderID),
		LastIncludedIndex: uint64(r.LastIncludedIndex),
		LastIncludedTerm:  uint64(r.LastIncludedTerm),
		Offset:            r.Offset,
		Data:              r.Data,
		Done:              r.Done,
	}
}

func isReqFromProto(p *pb.InstallSnapshotRequest) *raft.InstallSnapshotRequest {
	return &raft.InstallSnapshotRequest{
		Term:              raft.Term(p.Term),
		LeaderID:          raft.NodeID(p.LeaderId),
		LastIncludedIndex: raft.Index(p.LastIncludedIndex),
		LastIncludedTerm:  raft.Term(p.LastIncludedTerm),
		Offset:            p.Offset,
		Data:              p.Data,
		Done:              p.Done,
	}
}

// ---- ReadIndex -------------------------------------------------------------

func riReqToProto(r *raft.ReadIndexRequest) *pb.ReadIndexRequest {
	return &pb.ReadIndexRequest{
		Term: uint64(r.Term),
	}
}

func riReqFromProto(p *pb.ReadIndexRequest) *raft.ReadIndexRequest {
	return &raft.ReadIndexRequest{
		Term: raft.Term(p.Term),
	}
}

func riRespToProto(r *raft.ReadIndexResponse) *pb.ReadIndexResponse {
	return &pb.ReadIndexResponse{
		Term:  uint64(r.Term),
		Index: uint64(r.Index),
	}
}

func riRespFromProto(p *pb.ReadIndexResponse) *raft.ReadIndexResponse {
	return &raft.ReadIndexResponse{
		Term:  raft.Term(p.Term),
		Index: raft.Index(p.Index),
	}
}

// riRespFromProtoErr converts a proto ReadIndexResponse to either a
// *raft.ReadIndexResponse or a *raft.NotLeaderError. The server encodes a
// not-leader condition by setting leader_id and leaving index as zero rather
// than returning a gRPC error, which would erase the typed error.
func riRespFromProtoErr(p *pb.ReadIndexResponse) (*raft.ReadIndexResponse, error) {
	if p.LeaderId != "" {
		// Server signalled "not leader"; surface as the canonical typed error.
		return nil, &raft.NotLeaderError{Leader: raft.NodeID(p.LeaderId)}
	}
	return riRespFromProto(p), nil
}
