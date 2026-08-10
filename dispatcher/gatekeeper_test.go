// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package dispatcher

import (
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
)

// TestGateKeeperBlocking verifies the basic block life cycle, so that the pruning tests are working from a known state.
func TestGateKeeperBlocking(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper()
	defer gk.Close()

	const addr = "10.0.0.1"
	c.False(gk.IsAddressStringBlocked(addr))
	gk.BlockAddressString(addr)
	c.True(gk.IsAddressStringBlocked(addr))

	// A block whose time has run out no longer keeps the peer out, even before it has been pruned away
	gk.addresses.Store(addr, time.Now().Add(-time.Minute))
	c.False(gk.IsAddressStringBlocked(addr))
}

// TestPruneRemovesExpiredBlocks verifies that pruning drops the blocks that have run out while leaving the rest alone.
func TestPruneRemovesExpiredBlocks(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper()
	defer gk.Close()

	gk.addresses.Store("10.0.0.1", time.Now().Add(-time.Minute))
	gk.BlockAddressString("10.0.0.2")
	gk.pruneExpired()

	_, stillThere := gk.addresses.Load("10.0.0.1")
	c.False(stillThere, "the expired block should have been pruned")
	c.True(gk.IsAddressStringBlocked("10.0.0.2"))
}

// TestPruneDoesNotUnblockRecentlyReblockedPeer verifies that the removal of an expired block is skipped when the
// address was blocked again after the expired entry was observed. Removing it unconditionally would silently unblock a
// peer that had just been blocked, which is precisely the peer we least want to let back in.
func TestPruneDoesNotUnblockRecentlyReblockedPeer(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper()
	defer gk.Close()

	const addr = "10.0.0.1"
	expired := time.Now().Add(-time.Minute)
	gk.addresses.Store(addr, expired)

	// Pruning has observed the expired entry, but before it can remove it, the peer misbehaves and is blocked again
	gk.BlockAddressString(addr)
	gk.unblockIfExpired(addr, expired)
	c.True(gk.IsAddressStringBlocked(addr), "the fresh block must survive the removal of the expired one")

	// Once nothing re-blocks it, the expired entry is still removed as it should be
	gk.addresses.Store(addr, expired)
	gk.unblockIfExpired(addr, expired)
	_, stillThere := gk.addresses.Load(addr)
	c.False(stillThere)
}

// TestGateKeeperCloseIsIdempotent verifies that a second Close is a no-op rather than a panic, since nothing prevents
// Dispatcher.Stop from being called more than once.
func TestGateKeeperCloseIsIdempotent(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper()
	c.NotPanics(gk.Close)
	c.NotPanics(gk.Close)

	// Concurrent closes must be no less safe than sequential ones. A panic in one of these goroutines can't be
	// recovered by the test, so it takes the whole run down with it, which is failure enough.
	gk2 := NewGateKeeper()
	done := make(chan struct{}, 4)
	for range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			gk2.Close()
		}()
	}
	for range 4 {
		<-done
	}
	c.False(gk2.IsAddressStringBlocked("10.0.0.1"))
}
