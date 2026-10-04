package ring

import (
	"sync"
	"testing"
)

func TestPushPopOrder(t *testing.T) {
	r := New[int](5)
	if r.Cap() != 8 {
		t.Fatalf("cap = %d, want 8", r.Cap())
	}
	for i := 0; i < 8; i++ {
		if !r.Push(i) {
			t.Fatalf("push %d failed", i)
		}
	}
	if r.Push(99) {
		t.Fatal("push into full ring succeeded")
	}
	for i := 0; i < 8; i++ {
		v, ok := r.Pop()
		if !ok || v != i {
			t.Fatalf("pop = %d,%v want %d", v, ok, i)
		}
	}
	if _, ok := r.Pop(); ok {
		t.Fatal("pop from empty ring succeeded")
	}
}

func TestConcurrentSPSC(t *testing.T) {
	const n = 1_000_000
	r := New[int](1024)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; {
			if r.Push(i) {
				i++
			}
		}
	}()
	buf := make([]int, 256)
	next := 0
	for next < n {
		k := r.Drain(buf)
		for _, v := range buf[:k] {
			if v != next {
				t.Fatalf("got %d want %d", v, next)
			}
			next++
		}
	}
	wg.Wait()
}

func BenchmarkPushPop(b *testing.B) {
	r := New[int](1024)
	done := make(chan struct{})
	go func() {
		buf := make([]int, 256)
		got := 0
		for got < b.N {
			got += r.Drain(buf)
		}
		close(done)
	}()
	b.ResetTimer()
	for i := 0; i < b.N; {
		if r.Push(i) {
			i++
		}
	}
	<-done
}
