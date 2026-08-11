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
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
)

// TestGateKeeperBlocking verifies the basic block life cycle, so that the pruning tests are working from a known state.
func TestGateKeeperBlocking(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper(nil)
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
	gk := NewGateKeeper(nil)
	defer gk.Close()

	gk.addresses.Store("10.0.0.1", time.Now().Add(-time.Minute))
	gk.BlockAddressString("10.0.0.2")
	gk.pruneExpired()

	_, stillThere := gk.addresses.Load("10.0.0.1")
	c.False(stillThere, "the expired block should have been pruned")
	c.True(gk.IsAddressStringBlocked("10.0.0.2"))
}

// TestSuppressedDialsDoNotBlockTheAddress verifies that an address we couldn't reach is only kept out of the peers we
// dial, rather than being refused when it connects to us. A peer that is firewalled or behind a NAT can't accept a
// connection but is perfectly able to make one, which is the common case in a real swarm, so a failed dial that blocked
// it would cost us a peer that never did anything wrong — in both directions.
func TestSuppressedDialsDoNotBlockTheAddress(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper(nil)
	defer gk.Close()

	const addr = "10.0.0.1"
	c.False(gk.IsDialBlocked(addr))
	gk.SuppressDialsTo(addr)
	c.True(gk.IsDialBlocked(addr))
	c.False(gk.IsAddressStringBlocked(addr), "a peer we couldn't dial must still be free to connect to us")

	// A block, by contrast, applies in both directions
	const blocked = "10.0.0.2"
	gk.BlockAddressString(blocked)
	c.True(gk.IsAddressStringBlocked(blocked))
	c.True(gk.IsDialBlocked(blocked), "an address we won't accept a connection from is not one to dial either")

	// A suppression whose time has run out no longer keeps us from dialing, even before it has been pruned away
	gk.undialable.Store(addr, time.Now().Add(-time.Minute))
	c.False(gk.IsDialBlocked(addr))
}

// TestPruneRemovesExpiredDialSuppressions verifies that the dial suppressions are reclaimed by the same pruning the
// blocks are, rather than accumulating for the life of the process.
func TestPruneRemovesExpiredDialSuppressions(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper(nil)
	defer gk.Close()

	gk.undialable.Store("10.0.0.1", time.Now().Add(-time.Minute))
	gk.SuppressDialsTo("10.0.0.2")
	gk.pruneExpired()

	_, stillThere := gk.undialable.Load("10.0.0.1")
	c.False(stillThere, "the expired dial suppression should have been pruned")
	c.True(gk.IsDialBlocked("10.0.0.2"))
}

// TestPruneDoesNotUnblockRecentlyReblockedPeer verifies that the removal of an expired block is skipped when the
// address was blocked again after the expired entry was observed. Removing it unconditionally would silently unblock a
// peer that had just been blocked, which is precisely the peer we least want to let back in.
func TestPruneDoesNotUnblockRecentlyReblockedPeer(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper(nil)
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

// TestGateKeeperLogsToTheLoggerItWasGiven verifies that the lines saying a peer was blocked and unblocked go to the
// gatekeeper's own logger rather than to the package level slog functions, which write to the process default logger
// no matter what the dispatcher was told to use.
func TestGateKeeperLogsToTheLoggerItWasGiven(t *testing.T) {
	c := check.New(t)
	logger, sink := newTestLogger()
	defaultSink := captureDefaultLogger(t)
	gk := NewGateKeeper(logger)
	defer gk.Close()

	const addr = "10.0.0.1"
	gk.BlockAddressString(addr)
	expired := time.Now().Add(-time.Minute)
	gk.addresses.Store(addr, expired)
	gk.unblockIfExpired(addr, expired)

	logged := sink.contents()
	// The whole message, since "blocked peer" is also a part of "unblocked peer"
	c.Contains(logged, `msg="blocked peer"`)
	c.Contains(logged, `msg="unblocked peer"`)
	c.Contains(logged, addr)
	c.NotContains(defaultSink.contents(), "blocked peer",
		"the gatekeeper's logging must stay off the process default logger")
}

// TestGateKeeperWithoutALoggerUsesTheDefault verifies that a gatekeeper handed no logger of its own still logs, rather
// than going silent or dereferencing a nil logger.
func TestGateKeeperWithoutALoggerUsesTheDefault(t *testing.T) {
	c := check.New(t)
	defaultSink := captureDefaultLogger(t)
	gk := NewGateKeeper(nil)
	defer gk.Close()

	gk.BlockAddressString("10.0.0.1")
	c.Contains(defaultSink.contents(), `msg="blocked peer"`)
}

// TestLookupsReclaimExpiredEntries verifies that an entry whose time has run out is taken away by the lookup that finds
// it, rather than being left to a sweep that runs only once every blockDuration and stops altogether at Close.
func TestLookupsReclaimExpiredEntries(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper(nil)
	defer gk.Close()

	const addr = "10.0.0.1"
	expired := time.Now().Add(-time.Minute)
	gk.addresses.Store(addr, expired)
	c.False(gk.IsAddressStringBlocked(addr))
	_, stillThere := gk.addresses.Load(addr)
	c.False(stillThere, "a lookup must reclaim the block it found expired")

	gk.undialable.Store(addr, expired)
	c.False(gk.IsDialBlocked(addr))
	_, stillThere = gk.undialable.Load(addr)
	c.False(stillThere, "a lookup must reclaim the dial suppression it found expired")

	// One that hasn't run out is left exactly where it is
	gk.BlockAddressString(addr)
	c.True(gk.IsAddressStringBlocked(addr))
	_, stillThere = gk.addresses.Load(addr)
	c.True(stillThere, "a lookup must leave a block that is still in force alone")
}

// TestClosedGateKeeperRecordsNothing verifies that a GateKeeper which has been closed stops accumulating entries.
// Pruning is the only thing that removes one on its own and it exits at Close, while Dispatcher.Stop closes the
// gatekeeper and leaves it reachable through Dispatcher.GateKeeper() — which every client holds and calls on each
// failed dial and peer error. Nothing couples a client's lifetime to the dispatcher's, so a dispatcher stopped ahead of
// its clients would otherwise grow a permanent entry per address for as long as those clients kept running.
func TestClosedGateKeeperRecordsNothing(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper(nil)
	gk.BlockAddressString("10.0.0.1")
	gk.SuppressDialsTo("10.0.0.2")
	gk.Close()

	// What it was holding goes with it, since nothing is left to reclaim any of it
	c.Equal(0, entryCount(&gk.addresses), "a closed GateKeeper must not go on holding the blocks it had")
	c.Equal(0, entryCount(&gk.undialable), "a closed GateKeeper must not go on holding the suppressions it had")

	// And nothing recorded afterwards is retained either
	for i := range 100 {
		addr := "10.1.0." + strconv.Itoa(i)
		gk.BlockAddressString(addr)
		gk.SuppressDialsTo(addr)
		c.False(gk.IsAddressStringBlocked(addr))
		c.False(gk.IsDialBlocked(addr))
	}
	c.Equal(0, entryCount(&gk.addresses), "a closed GateKeeper must not accumulate blocks")
	c.Equal(0, entryCount(&gk.undialable), "a closed GateKeeper must not accumulate dial suppressions")
}

// entryCount returns the number of entries in the map, which sync.Map doesn't report on its own.
func entryCount(m *sync.Map) int {
	count := 0
	m.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// TestGateKeeperCloseIsIdempotent verifies that a second Close is a no-op rather than a panic, since nothing prevents
// Dispatcher.Stop from being called more than once.
func TestGateKeeperCloseIsIdempotent(t *testing.T) {
	c := check.New(t)
	gk := NewGateKeeper(nil)
	c.NotPanics(gk.Close)
	c.NotPanics(gk.Close)

	// Concurrent closes must be no less safe than sequential ones. A panic in one of these goroutines can't be
	// recovered by the test, so it takes the whole run down with it, which is failure enough.
	gk2 := NewGateKeeper(nil)
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
