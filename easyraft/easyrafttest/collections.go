package easyrafttest

import (
	"github.com/brunoga/raft/v2/easyraft"
)

// Collections is one typed collection as seen from every node of a cluster.
//
// A collection handle belongs to one store, so testing replication means
// holding one handle per node and keeping them in step with the cluster. This
// holds the set and tracks which node is leader, so a test writes through
// [Collections.Leader] and reads through [Collections.Node] without
// bookkeeping of its own.
type Collections[T any] struct {
	cluster *Cluster
	name    string
	all     []*easyraft.Collection[T]
}

// AddCollection adds the same named collection to every node of the cluster.
//
// Call it before [Cluster.Start] when mutations or change handlers have to be
// registered, because both must exist on every replica before any entry can
// reach them. Adding a collection itself is safe at any time.
func AddCollection[T any](c *Cluster, name string) *Collections[T] {
	c.t.Helper()
	cs := &Collections[T]{cluster: c, name: name}
	for _, s := range c.Stores {
		cs.all = append(cs.all, easyraft.AddCollection[T](s, name))
	}
	return cs
}

// Leader returns the collection on the node that is currently leader, waiting
// for one if the cluster is between leaders. Writes go here.
func (cs *Collections[T]) Leader() *easyraft.Collection[T] {
	cs.cluster.t.Helper()
	cs.cluster.WaitLeader()
	return cs.all[cs.cluster.LeaderIndex()]
}

// Node returns the collection on node i. Reads from a follower go here.
func (cs *Collections[T]) Node(i int) *easyraft.Collection[T] {
	cs.cluster.t.Helper()
	cs.cluster.Node(i) // bounds check and its error message
	return cs.all[i]
}

// All returns the collection on every node, in node order, including any that
// are stopped. Use it to assert that every replica agrees.
func (cs *Collections[T]) All() []*easyraft.Collection[T] {
	return cs.all
}

// RegisterMutation registers the same mutation on every node. A mutation must
// exist on every replica before any entry can call it, so this has to happen
// before [Cluster.Start].
func (cs *Collections[T]) RegisterMutation(name string, fn func(current *T, args []byte) (*T, []byte, error)) {
	cs.cluster.t.Helper()
	for _, c := range cs.all {
		c.RegisterMutation(name, fn)
	}
}

// Rebind points this set's handle for node i at that node's current store,
// which is what a [Cluster.RestartNode] leaves to be done: the restarted node
// is a new store, and the old handle writes to a store that has stopped.
func (cs *Collections[T]) Rebind(i int) *easyraft.Collection[T] {
	cs.cluster.t.Helper()
	cs.all[i] = easyraft.AddCollection[T](cs.cluster.Node(i), cs.name)
	return cs.all[i]
}
