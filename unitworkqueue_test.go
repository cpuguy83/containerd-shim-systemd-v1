package main

import (
	"testing"
	"time"
)

func TestUnitWorkQueue(t *testing.T) {
	t.Run("an item added once is returned once then blocks", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.Add("a")

		got, shutdown := q.Get()
		if shutdown || got != "a" {
			t.Fatalf("expected to get \"a\", got %q shutdown=%v", got, shutdown)
		}
		q.Done("a")

		if n := queueLen(q); n != 0 {
			t.Fatalf("expected the queue to be empty after the item was processed, got %d", n)
		}
	})

	t.Run("an item added repeatedly before processing is returned once", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.Add("a")
		q.Add("a")
		q.Add("a")

		got, _ := q.Get()
		if got != "a" {
			t.Fatalf("expected \"a\", got %q", got)
		}
		q.Done("a")

		if n := queueLen(q); n != 0 {
			t.Fatalf("expected duplicate adds to collapse into a single item, queue has %d", n)
		}
	})

	t.Run("an item added while being processed is returned again after Done", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.Add("a")

		got, _ := q.Get() // now processing "a"
		if got != "a" {
			t.Fatalf("expected \"a\", got %q", got)
		}

		q.Add("a") // observed a change mid-reconcile; must not be lost
		q.Done("a")

		got, shutdown := q.Get()
		if shutdown || got != "a" {
			t.Fatalf("expected \"a\" to be re-queued, got %q shutdown=%v", got, shutdown)
		}
		q.Done("a")
	})

	t.Run("distinct items are each returned", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.Add("a")
		q.Add("b")

		seen := map[string]bool{}
		for i := 0; i < 2; i++ {
			got, _ := q.Get()
			seen[got] = true
			q.Done(got)
		}
		if !seen["a"] || !seen["b"] {
			t.Fatalf("expected both a and b, saw %v", seen)
		}
	})

	t.Run("Get blocks until an item is added", func(t *testing.T) {
		q := newUnitWorkQueue()

		got := make(chan string, 1)
		go func() {
			name, _ := q.Get()
			got <- name
		}()

		select {
		case <-got:
			t.Fatal("Get returned before any item was added")
		case <-time.After(20 * time.Millisecond):
		}

		q.Add("a")
		select {
		case name := <-got:
			if name != "a" {
				t.Fatalf("expected \"a\", got %q", name)
			}
		case <-time.After(time.Second):
			t.Fatal("Get did not return after an item was added")
		}
	})

	t.Run("ShutDown unblocks a waiting Get", func(t *testing.T) {
		q := newUnitWorkQueue()

		done := make(chan bool, 1)
		go func() {
			_, shutdown := q.Get()
			done <- shutdown
		}()

		q.ShutDown()
		select {
		case shutdown := <-done:
			if !shutdown {
				t.Fatal("expected Get to report shutdown")
			}
		case <-time.After(time.Second):
			t.Fatal("ShutDown did not unblock a waiting Get")
		}
	})

	t.Run("ShutDown drains already-queued items before reporting shutdown", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.Add("a")
		q.ShutDown()

		got, shutdown := q.Get()
		if shutdown || got != "a" {
			t.Fatalf("expected to drain \"a\" before shutdown, got %q shutdown=%v", got, shutdown)
		}
		q.Done("a")

		_, shutdown = q.Get()
		if !shutdown {
			t.Fatal("expected shutdown once the queue drained")
		}
	})

	t.Run("Add after ShutDown is dropped", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.ShutDown()
		q.Add("a")

		_, shutdown := q.Get()
		if !shutdown {
			t.Fatal("expected shutdown; Add after ShutDown must not enqueue")
		}
	})

	t.Run("ShutDown is idempotent", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.ShutDown()
		q.ShutDown() // must not panic on a second close
	})
}

func TestUnitWorkQueueRetry(t *testing.T) {
	t.Run("AddAfter enqueues once the delay elapses", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.AddAfter("a", 10*time.Millisecond)

		if queueLen(q) != 0 {
			t.Fatal("expected AddAfter not to enqueue immediately")
		}

		got := make(chan string, 1)
		go func() { name, _ := q.Get(); got <- name }()
		select {
		case name := <-got:
			if name != "a" {
				t.Fatalf("expected \"a\", got %q", name)
			}
		case <-time.After(time.Second):
			t.Fatal("AddAfter never enqueued the item")
		}
	})

	t.Run("Retry re-enqueues up to the cap then gives up", func(t *testing.T) {
		q := newUnitWorkQueue()
		const maxRetries = 3

		drain := func() { // consume and complete one processing cycle
			name, _ := q.Get()
			q.Done(name)
		}

		for i := 0; i < maxRetries; i++ {
			if !q.Retry("a", maxRetries, time.Millisecond, 5*time.Millisecond) {
				t.Fatalf("retry %d should have been scheduled", i)
			}
			drain() // wait for the scheduled add and clear it
		}

		if q.Retry("a", maxRetries, time.Millisecond, 5*time.Millisecond) {
			t.Fatal("expected Retry to give up once the cap is reached")
		}
	})

	t.Run("Forget resets the retry count", func(t *testing.T) {
		q := newUnitWorkQueue()
		const maxRetries = 1

		if !q.Retry("a", maxRetries, time.Millisecond, time.Millisecond) {
			t.Fatal("first retry should schedule")
		}
		name, _ := q.Get()
		q.Done(name)

		if q.Retry("a", maxRetries, time.Millisecond, time.Millisecond) {
			t.Fatal("expected the cap to be reached before Forget")
		}
		q.Forget("a")
		if !q.Retry("a", maxRetries, time.Millisecond, time.Millisecond) {
			t.Fatal("expected Retry to schedule again after Forget reset the count")
		}
	})

	t.Run("ShutDown abandons a pending AddAfter", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.AddAfter("a", time.Hour) // would never fire on its own
		q.ShutDown()

		// The timer goroutine must observe shutdown and exit rather than enqueue.
		_, shutdown := q.Get()
		if !shutdown {
			t.Fatal("expected shutdown without the pending AddAfter enqueuing")
		}
	})

	t.Run("Retry after ShutDown does not schedule", func(t *testing.T) {
		q := newUnitWorkQueue()
		q.ShutDown()
		if q.Retry("a", 5, time.Millisecond, time.Millisecond) {
			t.Fatal("expected Retry to be a no-op after ShutDown")
		}
	})
}

func TestBackoff(t *testing.T) {
	base := 100 * time.Millisecond
	max := time.Second

	t.Run("the delay doubles with each attempt", func(t *testing.T) {
		cases := map[int]time.Duration{
			0: 100 * time.Millisecond,
			1: 200 * time.Millisecond,
			2: 400 * time.Millisecond,
			3: 800 * time.Millisecond,
		}
		for attempts, want := range cases {
			if got := backoff(attempts, base, max); got != want {
				t.Fatalf("backoff(%d) = %s, want %s", attempts, got, want)
			}
		}
	})

	t.Run("the delay is capped at max", func(t *testing.T) {
		if got := backoff(10, base, max); got != max {
			t.Fatalf("expected a large attempt count to be capped at %s, got %s", max, got)
		}
	})

	t.Run("a huge attempt count cannot overflow", func(t *testing.T) {
		if got := backoff(1000, base, max); got != max {
			t.Fatalf("expected an overflowing attempt count to be capped at %s, got %s", max, got)
		}
	})
}

// queueLen returns the number of immediately-dequeuable items. It is a
// white-box peek so emptiness can be asserted without a blocking Get.
func queueLen(q *unitWorkQueue) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}
