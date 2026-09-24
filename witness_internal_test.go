package raft

import (
	"testing"
	"testing/synctest"
)

// TestPeerFlags_RoundTrip pins the packed role byte, that the witness bit
// survives every encoding a peer travels in, and that the legacy byte values
// still mean what they meant.
func TestPeerFlags_RoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, p := range []PeerConfig{
			{ID: "a"}, {ID: "a", Voter: true}, {ID: "a", Voter: true, Witness: true},
		} {
			v, w := peerRoles(peerFlags(p))
			if v != p.Voter || w != p.Witness {
				t.Fatalf("flags for %+v decode as voter=%v witness=%v", p, v, w)
			}
			_, got, ok := decodeConfigEntry(encodeConfigEntry(configOpAdd, p))
			if !ok || got != p {
				t.Fatalf("config entry for %+v decodes as %+v ok=%v", p, got, ok)
			}
			list, _, ok := decodePeerList(appendPeerList(nil, []PeerConfig{p}))
			if !ok || len(list) != 1 || list[0] != p {
				t.Fatalf("peer list for %+v decodes as %+v ok=%v", p, list, ok)
			}
		}
		if v, w := peerRoles(0); v || w {
			t.Fatal("legacy 0 is not a non-voter")
		}
		if v, w := peerRoles(1); !v || w {
			t.Fatal("legacy 1 is not a plain voter")
		}
	})
}

// TestStripForWitness pins that commands go and config entries stay.
func TestStripForWitness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := []LogEntry{
			{Index: 1, Term: 1, Command: []byte("k=v")},
			{Index: 2, Term: 1, Command: encodeConfigEntry(configOpAdd, PeerConfig{ID: "x", Voter: true})},
			{Index: 3, Term: 2, Command: encodeDedupCmd("c", 1, []byte("k=v"))},
		}
		out := stripForWitness(in)
		if len(out) != 3 || out[0].Index != 1 || out[2].Term != 2 {
			t.Fatalf("shape lost: %+v", out)
		}
		if out[0].Command != nil || out[2].Command != nil {
			t.Fatalf("commands kept: %+v", out)
		}
		if !isConfigEntry(out[1].Command) {
			t.Fatal("config entry stripped")
		}
		if in[0].Command == nil {
			t.Fatal("input modified")
		}
	})
}
