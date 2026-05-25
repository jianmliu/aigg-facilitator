package main

import (
	"context"
	"sync"
	"testing"
)

func TestNonceManagerSerializesConcurrentClaims(t *testing.T) {
	manager := newNonceManager()
	var fetches int
	fetch := func(context.Context) (uint64, error) {
		fetches++
		return 42, nil
	}

	const workers = 16
	nonces := make(chan uint64, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nonce, err := manager.Next(context.Background(), fetch)
			if err != nil {
				t.Errorf("Next returned error: %v", err)
				return
			}
			nonces <- nonce
		}()
	}
	wg.Wait()
	close(nonces)

	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
	seen := make(map[uint64]bool, workers)
	for nonce := range nonces {
		if seen[nonce] {
			t.Fatalf("duplicate nonce claimed: %d", nonce)
		}
		seen[nonce] = true
	}
	for nonce := uint64(42); nonce < 42+workers; nonce++ {
		if !seen[nonce] {
			t.Fatalf("missing nonce %d", nonce)
		}
	}
}

func TestNonceManagerCanResetAfterSendFailure(t *testing.T) {
	manager := newNonceManager()
	fetches := 0
	fetch := func(context.Context) (uint64, error) {
		fetches++
		if fetches == 1 {
			return 7, nil
		}
		return 99, nil
	}

	first, err := manager.Next(context.Background(), fetch)
	if err != nil {
		t.Fatalf("first Next returned error: %v", err)
	}
	manager.Reset()
	second, err := manager.Next(context.Background(), fetch)
	if err != nil {
		t.Fatalf("second Next returned error: %v", err)
	}

	if first != 7 || second != 99 {
		t.Fatalf("nonces = %d, %d; want 7, 99", first, second)
	}
	if fetches != 2 {
		t.Fatalf("fetches = %d, want 2", fetches)
	}
}
