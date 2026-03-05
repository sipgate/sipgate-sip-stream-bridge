package bridge

import (
	"sync"
	"testing"
)

// Test 1 — NewPortPool(10000, 10003) pre-loads 3 ports (max-min = 3).
func TestPortPool_Acquire_ThreePorts(t *testing.T) {
	pool, err := NewPortPool(10000, 10003)
	if err != nil {
		t.Fatalf("NewPortPool: unexpected error: %v", err)
	}

	acquired := make(map[int]bool)
	for i := 0; i < 3; i++ {
		port, err := pool.Acquire()
		if err != nil {
			t.Fatalf("Acquire() #%d: expected success, got error: %v", i+1, err)
		}
		if port < 10000 || port >= 10003 {
			t.Errorf("Acquire() #%d: port %d out of expected range [10000, 10003)", i+1, port)
		}
		if acquired[port] {
			t.Errorf("Acquire() #%d: port %d returned twice", i+1, port)
		}
		acquired[port] = true
	}

	// Fourth acquire must fail — pool exhausted
	_, err = pool.Acquire()
	if err == nil {
		t.Error("Acquire() fourth call: expected error on exhausted pool, got nil")
	}
}

// Test 2 — Acquire is non-blocking when pool empty.
func TestPortPool_Acquire_NonBlocking(t *testing.T) {
	pool, err := NewPortPool(10000, 10001) // 1 port only
	if err != nil {
		t.Fatalf("NewPortPool: unexpected error: %v", err)
	}

	// First acquire should succeed
	_, err = pool.Acquire()
	if err != nil {
		t.Fatalf("first Acquire(): expected success, got error: %v", err)
	}

	// Second acquire must return immediately with error — no blocking
	done := make(chan error, 1)
	go func() {
		_, err := pool.Acquire()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("second Acquire() on empty pool: expected error, got nil")
		}
	// If this blocks, the test will hang — non-blocking guarantee violated
	}
}

// Test 3 — Release returns port to pool for reuse.
func TestPortPool_Release_ReturnsPortToPool(t *testing.T) {
	pool, err := NewPortPool(10000, 10001) // 1 port only
	if err != nil {
		t.Fatalf("NewPortPool: unexpected error: %v", err)
	}

	port, err := pool.Acquire()
	if err != nil {
		t.Fatalf("first Acquire(): expected success, got error: %v", err)
	}

	pool.Release(port)

	port2, err := pool.Acquire()
	if err != nil {
		t.Fatalf("second Acquire() after Release(): expected success, got error: %v", err)
	}
	if port2 != port {
		t.Errorf("second Acquire(): expected port %d (reused), got %d", port, port2)
	}
}

// Test 4 — NewPortPool with min >= max returns error.
func TestPortPool_InvalidRange_ReturnsError(t *testing.T) {
	_, err := NewPortPool(10000, 10000)
	if err == nil {
		t.Error("NewPortPool(10000, 10000): expected error for min >= max, got nil")
	}

	// min > max also invalid
	_, err = NewPortPool(10005, 10001)
	if err == nil {
		t.Error("NewPortPool(10005, 10001): expected error for min > max, got nil")
	}
}

// Test 5 — Concurrent Acquire from multiple goroutines — no races.
func TestPortPool_ConcurrentAcquire_NoRace(t *testing.T) {
	const numPorts = 10
	pool, err := NewPortPool(10000, 10000+numPorts)
	if err != nil {
		t.Fatalf("NewPortPool: unexpected error: %v", err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		successes int
		failures  int
	)

	for i := 0; i < numPorts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pool.Acquire()
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else {
				failures++
			}
		}()
	}

	wg.Wait()

	if successes != numPorts {
		t.Errorf("concurrent Acquire: expected %d successes, got %d (failures=%d)", numPorts, successes, failures)
	}
	if failures != 0 {
		t.Errorf("concurrent Acquire: expected 0 failures, got %d", failures)
	}
}
