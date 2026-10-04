// Package ring is a bounded single-producer single-consumer queue.
//
// Ingest goroutines (one per feed) each own one Ring and the engine goroutine
// drains them, so the hot path needs no locks: the producer only writes tail,
// the consumer only writes head, and each side reads the other's index with an
// atomic load. Indices live on separate cache lines to avoid false sharing.
package ring

import (
	"sync/atomic"
)

type pad [64]byte

// Ring is an SPSC queue of T with a power-of-two capacity.
type Ring[T any] struct {
	_    pad
	head atomic.Uint64 // next slot to read; written by consumer only
	_    pad
	tail atomic.Uint64 // next slot to write; written by producer only
	_    pad
	// Each side caches the other side's index and only reloads it when the
	// cached value says the ring looks full (producer) or empty (consumer).
	headCache uint64 // producer-local
	_         pad
	tailCache uint64 // consumer-local
	_         pad
	mask      uint64
	buf       []T
}

// New returns a ring holding at least capacity items (rounded up to a power of two).
func New[T any](capacity int) *Ring[T] {
	n := uint64(1)
	for n < uint64(capacity) {
		n <<= 1
	}
	return &Ring[T]{mask: n - 1, buf: make([]T, n)}
}

// Cap is the number of slots.
func (r *Ring[T]) Cap() int { return len(r.buf) }

// Len is a racy snapshot of the number of queued items.
func (r *Ring[T]) Len() int { return int(r.tail.Load() - r.head.Load()) }

// Push enqueues v. It returns false if the ring is full (caller decides whether
// to spin, drop, or count backpressure). Producer side only.
func (r *Ring[T]) Push(v T) bool {
	t := r.tail.Load()
	if t-r.headCache == uint64(len(r.buf)) {
		r.headCache = r.head.Load()
		if t-r.headCache == uint64(len(r.buf)) {
			return false
		}
	}
	r.buf[t&r.mask] = v
	r.tail.Store(t + 1) // release: publishes the slot write
	return true
}

// Pop dequeues one item. Consumer side only.
func (r *Ring[T]) Pop() (T, bool) {
	var zero T
	h := r.head.Load()
	if h == r.tailCache {
		r.tailCache = r.tail.Load()
		if h == r.tailCache {
			return zero, false
		}
	}
	v := r.buf[h&r.mask]
	r.buf[h&r.mask] = zero
	r.head.Store(h + 1)
	return v, true
}

// Drain pops up to len(dst) items into dst and returns how many it popped.
// It reloads tail at most once, so a burst costs one atomic load instead of one per item.
func (r *Ring[T]) Drain(dst []T) int {
	h := r.head.Load()
	if h == r.tailCache {
		r.tailCache = r.tail.Load()
	}
	avail := r.tailCache - h
	n := uint64(len(dst))
	if avail < n {
		n = avail
	}
	for i := uint64(0); i < n; i++ {
		dst[i] = r.buf[(h+i)&r.mask]
	}
	r.head.Store(h + n)
	return int(n)
}
