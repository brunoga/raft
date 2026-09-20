package grpctransport

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/brunoga/raft/transport/grpctransport/internal/raftpb"
)

// ---- Wire compatibility -----------------------------------------------------
//
// Two nodes of a Raft cluster are upgraded one at a time, so every release has
// to talk to the one before it. Protobuf makes most of that automatic: a field
// added with a fresh number is ignored by a peer that does not know it, and a
// field a peer stops sending reads as its zero value.
//
// What protobuf does not protect is a field *number* changing meaning. Renumber
// a field, reuse the number of one that was deleted, or change a field's type
// in place, and both sides still parse the message -- into different values.
// A leader_commit read as a prev_log_index does not fail a request; it commits
// the wrong entries. There is no error to see, in a mixed-version window that
// might last hours.
//
// These tests pin the wire contract so that such a change fails here, where it
// costs a code review, rather than in a rolling upgrade.
//
// If one of them fails, the question is not how to update the expectation. It
// is whether the change is compatible:
//
//   - Adding a field with a number never used before: compatible. Add it to the
//     table below.
//   - Deleting a field: compatible only if its number is never reused. Mark it
//     `reserved` in raft.proto and remove it from the table.
//   - Renumbering, reusing, or retyping a field: NOT compatible. Old and new
//     nodes will disagree silently. Add a new field instead.
//   - Adding an RPC: compatible only for a peer that has it. One that does not
//     answers Unimplemented, which the caller has to handle.
//   - Renaming or removing an RPC: NOT compatible. gRPC routes on the method
//     name.

// wireField is one field's position in the wire format: what a peer of any
// version will read at that number.
type wireField struct {
	number int32
	kind   protoreflect.Kind
	// repeated is separate from kind because it changes the framing, and
	// flipping it on an existing field is as breaking as retyping it.
	repeated bool
}

// wireContract maps each message's full name to the fields it promises. Field
// names are keys for legibility only: the wire carries numbers, and renaming a
// field in the .proto changes nothing a peer can see.
var wireContract = map[string]map[string]wireField{
	"raft.RequestVoteRequest": {
		"group_id":       {1, protoreflect.Uint64Kind, false},
		"term":           {2, protoreflect.Uint64Kind, false},
		"candidate_id":   {3, protoreflect.StringKind, false},
		"last_log_index": {4, protoreflect.Uint64Kind, false},
		"last_log_term":  {5, protoreflect.Uint64Kind, false},
		"pre_vote":       {6, protoreflect.BoolKind, false},
	},
	"raft.RequestVoteResponse": {
		"term":         {1, protoreflect.Uint64Kind, false},
		"vote_granted": {2, protoreflect.BoolKind, false},
	},
	"raft.LogEntry": {
		"index":   {1, protoreflect.Uint64Kind, false},
		"term":    {2, protoreflect.Uint64Kind, false},
		"command": {3, protoreflect.BytesKind, false},
	},
	"raft.AppendEntriesRequest": {
		"group_id":       {1, protoreflect.Uint64Kind, false},
		"term":           {2, protoreflect.Uint64Kind, false},
		"leader_id":      {3, protoreflect.StringKind, false},
		"prev_log_index": {4, protoreflect.Uint64Kind, false},
		"prev_log_term":  {5, protoreflect.Uint64Kind, false},
		"entries":        {6, protoreflect.MessageKind, true},
		"leader_commit":  {7, protoreflect.Uint64Kind, false},
		"read_barrier":   {8, protoreflect.Uint64Kind, false},
	},
	"raft.AppendEntriesResponse": {
		"term":           {1, protoreflect.Uint64Kind, false},
		"success":        {2, protoreflect.BoolKind, false},
		"conflict_index": {3, protoreflect.Uint64Kind, false},
		"conflict_term":  {4, protoreflect.Uint64Kind, false},
	},
	"raft.InstallSnapshotRequest": {
		"group_id":            {1, protoreflect.Uint64Kind, false},
		"term":                {2, protoreflect.Uint64Kind, false},
		"leader_id":           {3, protoreflect.StringKind, false},
		"last_included_index": {4, protoreflect.Uint64Kind, false},
		"last_included_term":  {5, protoreflect.Uint64Kind, false},
		"offset":              {6, protoreflect.Int64Kind, false},
		"data":                {7, protoreflect.BytesKind, false},
		"done":                {8, protoreflect.BoolKind, false},
	},
	"raft.InstallSnapshotResponse": {
		"term": {1, protoreflect.Uint64Kind, false},
	},
	"raft.TimeoutNowRequest": {
		"group_id":  {1, protoreflect.Uint64Kind, false},
		"term":      {2, protoreflect.Uint64Kind, false},
		"leader_id": {3, protoreflect.StringKind, false},
	},
	"raft.TimeoutNowResponse": {
		"term": {1, protoreflect.Uint64Kind, false},
	},
	"raft.ReadIndexRequest": {
		"group_id": {1, protoreflect.Uint64Kind, false},
		"term":     {2, protoreflect.Uint64Kind, false},
	},
	"raft.ReadIndexResponse": {
		"term":      {1, protoreflect.Uint64Kind, false},
		"index":     {2, protoreflect.Uint64Kind, false},
		"leader_id": {3, protoreflect.StringKind, false},
	},
	"raft.HeartbeatEntry": {
		"group_id":       {1, protoreflect.Uint64Kind, false},
		"term":           {2, protoreflect.Uint64Kind, false},
		"leader_id":      {3, protoreflect.StringKind, false},
		"prev_log_index": {4, protoreflect.Uint64Kind, false},
		"prev_log_term":  {5, protoreflect.Uint64Kind, false},
		"leader_commit":  {6, protoreflect.Uint64Kind, false},
		"read_barrier":   {7, protoreflect.Uint64Kind, false},
	},
	"raft.HeartbeatResult": {
		"group_id":       {1, protoreflect.Uint64Kind, false},
		"term":           {2, protoreflect.Uint64Kind, false},
		"success":        {3, protoreflect.BoolKind, false},
		"conflict_index": {4, protoreflect.Uint64Kind, false},
		"conflict_term":  {5, protoreflect.Uint64Kind, false},
		"error_code":     {6, protoreflect.Uint32Kind, false},
		"error_message":  {7, protoreflect.StringKind, false},
	},
	"raft.BatchedHeartbeatRequest": {
		"entries": {1, protoreflect.MessageKind, true},
	},
	"raft.BatchedHeartbeatResponse": {
		"results": {1, protoreflect.MessageKind, true},
	},
}

// TestWireCompatibility_FieldNumbers checks every message in raft.proto against
// the contract above: the same fields, at the same numbers, with the same types
// and the same framing.
//
// A renumbered or reused field is the failure this exists for. Both versions
// parse the message successfully and disagree about what it says, with no error
// anywhere -- a leader_commit read as a prev_log_index commits the wrong
// entries rather than failing a request.
func TestWireCompatibility_FieldNumbers(t *testing.T) {
	msgs := pb.File_raft_proto.Messages()
	seen := make(map[string]bool, msgs.Len())

	for i := range msgs.Len() {
		md := msgs.Get(i)
		name := string(md.FullName())
		seen[name] = true

		want, ok := wireContract[name]
		if !ok {
			t.Errorf("message %s is on the wire but not in wireContract: add it, "+
				"so that a later change to its field numbers is caught here", name)
			continue
		}

		got := make(map[string]wireField, md.Fields().Len())
		for j := range md.Fields().Len() {
			fd := md.Fields().Get(j)
			got[string(fd.Name())] = wireField{
				number:   int32(fd.Number()),
				kind:     fd.Kind(),
				repeated: fd.IsList(),
			}
		}

		for fieldName, w := range want {
			g, present := got[fieldName]
			if !present {
				t.Errorf("%s.%s (number %d) is gone: deleting a field is only safe if its "+
					"number is never reused -- mark it reserved in raft.proto and drop it here",
					name, fieldName, w.number)
				continue
			}
			if g != w {
				t.Errorf("%s.%s changed on the wire: %s, want %s. "+
					"Old and new nodes will read the same bytes as different values; "+
					"add a new field instead",
					name, fieldName, describeField(g), describeField(w))
			}
		}
		for fieldName, g := range got {
			if _, present := want[fieldName]; !present {
				if reused := numberUsedBy(want, g.number); reused != "" {
					t.Errorf("%s.%s takes number %d, which the contract assigns to %s: "+
						"reusing a field number makes two versions disagree silently",
						name, fieldName, g.number, reused)
					continue
				}
				t.Errorf("%s.%s (number %d) is new: adding a field is compatible, "+
					"so record it in wireContract", name, fieldName, g.number)
			}
		}
	}

	for name := range wireContract {
		if !seen[name] {
			t.Errorf("message %s is in wireContract but no longer in raft.proto: "+
				"a peer still sending it will get no answer", name)
		}
	}
}

// TestWireCompatibility_ServiceMethods pins the gRPC method set. gRPC routes on
// the full method name, so renaming one makes every call from an older peer
// fail with Unimplemented, and changing a method's request or response type
// makes it fail to parse.
func TestWireCompatibility_ServiceMethods(t *testing.T) {
	want := map[string][2]string{
		"RequestVote":     {"raft.RequestVoteRequest", "raft.RequestVoteResponse"},
		"AppendEntries":   {"raft.AppendEntriesRequest", "raft.AppendEntriesResponse"},
		"InstallSnapshot": {"raft.InstallSnapshotRequest", "raft.InstallSnapshotResponse"},
		"TimeoutNow":      {"raft.TimeoutNowRequest", "raft.TimeoutNowResponse"},
		"ReadIndex":       {"raft.ReadIndexRequest", "raft.ReadIndexResponse"},
		"BatchHeartbeats": {"raft.BatchedHeartbeatRequest", "raft.BatchedHeartbeatResponse"},
	}

	svcs := pb.File_raft_proto.Services()
	if svcs.Len() != 1 {
		t.Fatalf("raft.proto declares %d services, want 1", svcs.Len())
	}
	svc := svcs.Get(0)
	if got := string(svc.FullName()); got != "raft.RaftService" {
		t.Errorf("service name = %s, want raft.RaftService: gRPC routes on it, so every "+
			"older peer would fail with Unimplemented", got)
	}

	got := make(map[string][2]string, svc.Methods().Len())
	for i := range svc.Methods().Len() {
		m := svc.Methods().Get(i)
		got[string(m.Name())] = [2]string{
			string(m.Input().FullName()),
			string(m.Output().FullName()),
		}
	}

	for name, types := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("RPC %s is gone: an older peer still calls it and will get Unimplemented", name)
			continue
		}
		if g != types {
			t.Errorf("RPC %s: %s -> %s, want %s -> %s; an older peer sends and expects the old types",
				name, g[0], g[1], types[0], types[1])
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("RPC %s is new. That is compatible only with a peer that has it: a peer "+
				"that does not answers Unimplemented, and the caller has to handle that rather "+
				"than treat it as a failed RPC. Record it here once it does", name)
		}
	}
}

// describeField renders a wireField the way the .proto would.
func describeField(f wireField) string {
	var b strings.Builder
	if f.repeated {
		b.WriteString("repeated ")
	}
	fmt.Fprintf(&b, "%s = %d", f.kind, f.number)
	return b.String()
}

// numberUsedBy reports which contracted field owns number, or "" if none does.
func numberUsedBy(contract map[string]wireField, number int32) string {
	names := make([]string, 0, len(contract))
	for name, f := range contract {
		if f.number == number {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
