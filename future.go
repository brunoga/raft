package raft

// result holds either a value or an error.
type result[T any] struct {
	val T
	err error
}

// promise is the write-end of a one-shot result channel.
// Only the Raft node's main goroutine ever holds a promise.
type promise[T any] struct {
	ch chan<- result[T]
}

func (p promise[T]) resolve(val T) {
	p.ch <- result[T]{val: val}
}

func (p promise[T]) reject(err error) {
	var zero T
	p.ch <- result[T]{val: zero, err: err}
}
