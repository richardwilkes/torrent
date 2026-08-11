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
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	// blockDuration is how long an address that has done something wrong is refused for. A block applies in both
	// directions: we won't dial it and we won't accept a connection from it.
	blockDuration = 5 * time.Minute
	// dialSuppressionDuration is how long an address we couldn't reach is left out of the peers we dial.
	dialSuppressionDuration = 5 * time.Minute
)

// GateKeeper controls whether peers with a given address may connect with us.
type GateKeeper struct {
	done   chan bool
	logger *slog.Logger
	// addresses holds the addresses that are blocked outright, in both directions.
	addresses sync.Map
	// undialable holds the addresses an outgoing connection attempt of ours recently failed to reach. These only keep
	// us from dialing them again; the remote is still free to connect to us.
	undialable sync.Map
	closeOnce  sync.Once
}

// NewGateKeeper creates a new GateKeeper that logs to the supplied logger, or to slog.Default() if it is nil. The
// logger is taken here rather than being settable later because the pruning goroutine started below reaches for it
// without any synchronization of its own.
func NewGateKeeper(logger *slog.Logger) *GateKeeper {
	if logger == nil {
		logger = slog.Default()
	}
	r := &GateKeeper{done: make(chan bool), logger: logger}
	go r.prune()
	return r
}

// BlockAddress adds the specified address to the blocked list, which refuses it in both directions.
func (r *GateKeeper) BlockAddress(addr net.Addr) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return
	}
	r.BlockAddressString(host)
}

// BlockAddressString adds the specified address to the blocked list, which refuses it in both directions.
func (r *GateKeeper) BlockAddressString(addr string) {
	r.addresses.Store(addr, time.Now().Add(blockDuration))
	r.logger.Debug("blocked peer", "address", addr)
}

// SuppressDialsTo records that an outgoing connection attempt to the address failed, so that we don't spend the next
// attempts on it as well. This is deliberately not a block: a peer that is firewalled or behind a NAT can't accept
// connections but can happily make them, which is an ordinary thing to be in a swarm, so refusing the connections it
// makes to us because a dial of ours didn't get through would cost us a peer that never did anything wrong — and, in
// a small swarm, potentially every peer there is.
func (r *GateKeeper) SuppressDialsTo(addr string) {
	r.undialable.Store(addr, time.Now().Add(dialSuppressionDuration))
	r.logger.Debug("suppressed dials to peer", "address", addr)
}

// IsAddressBlocked returns true if the address is blocked.
func (r *GateKeeper) IsAddressBlocked(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return true
	}
	return r.IsAddressStringBlocked(host)
}

// IsAddressStringBlocked returns true if the address is blocked.
func (r *GateKeeper) IsAddressStringBlocked(addr string) bool {
	return unexpired(&r.addresses, addr)
}

// IsDialBlocked returns true if we shouldn't make an outgoing connection to the address, either because it is blocked
// outright or because a recent attempt of ours to reach it failed.
func (r *GateKeeper) IsDialBlocked(addr string) bool {
	return r.IsAddressStringBlocked(addr) || unexpired(&r.undialable, addr)
}

// unexpired returns true if the map holds an entry for the address whose expiry hasn't passed yet.
func unexpired(m *sync.Map, addr string) bool {
	if expires, ok := m.Load(addr); ok {
		if t, ok2 := expires.(time.Time); ok2 {
			return time.Now().Before(t)
		}
	}
	return false
}

func (r *GateKeeper) prune() {
	for {
		select {
		case <-time.After(blockDuration):
			r.pruneExpired()
		case <-r.done:
			return
		}
	}
}

// pruneExpired removes the addresses whose blocks and dial suppressions have run out.
func (r *GateKeeper) pruneExpired() {
	r.addresses.Range(func(addr, expires any) bool {
		r.unblockIfExpired(addr, expires)
		return true
	})
	r.undialable.Range(func(addr, expires any) bool {
		r.removeIfExpired(&r.undialable, addr, expires, "resumed dialing peer")
		return true
	})
}

// unblockIfExpired removes the address from the blocked list, but only if the block we were handed has run out and is
// still the one on record.
func (r *GateKeeper) unblockIfExpired(addr, expires any) {
	r.removeIfExpired(&r.addresses, addr, expires, "unblocked peer")
}

// removeIfExpired removes the address from the map, but only if the expiry we were handed has passed and is still the
// one on record. Removing it unconditionally would silently unblock an address that a concurrent block replaced with a
// fresh expiry after we observed the old one.
func (r *GateKeeper) removeIfExpired(m *sync.Map, addr, expires any, msg string) {
	if t, ok := expires.(time.Time); ok && t.Before(time.Now()) {
		if m.CompareAndDelete(addr, expires) {
			r.logger.Debug(msg, "address", addr)
		}
	}
}

// Close shuts this GateKeeper down. Calling it more than once is a no-op.
func (r *GateKeeper) Close() {
	r.closeOnce.Do(func() { close(r.done) })
}
