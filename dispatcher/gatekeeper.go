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

const blockDuration = 5 * time.Minute

// GateKeeper controls whether peers with a given address may connect with us.
type GateKeeper struct {
	done      chan bool
	addresses sync.Map
	closeOnce sync.Once
}

// NewGateKeeper creates a new GateKeeper.
func NewGateKeeper() *GateKeeper {
	r := &GateKeeper{done: make(chan bool)}
	go r.prune()
	return r
}

// BlockAddress adds the specified address to the incoming blocked list.
func (r *GateKeeper) BlockAddress(addr net.Addr) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return
	}
	r.BlockAddressString(host)
}

// BlockAddressString adds the specified address to the incoming blocked list.
func (r *GateKeeper) BlockAddressString(addr string) {
	r.addresses.Store(addr, time.Now().Add(blockDuration))
	slog.Debug("blocked peer", "address", addr)
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
	if expires, ok := r.addresses.Load(addr); ok {
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

// pruneExpired removes the addresses whose blocks have run out.
func (r *GateKeeper) pruneExpired() {
	r.addresses.Range(func(addr, expires any) bool {
		r.unblockIfExpired(addr, expires)
		return true
	})
}

// unblockIfExpired removes the address, but only if the expiry we were handed has passed and is still the one on
// record. Removing it unconditionally would silently unblock an address that a concurrent block replaced with a fresh
// expiry after we observed the old one.
func (r *GateKeeper) unblockIfExpired(addr, expires any) {
	if t, ok := expires.(time.Time); ok && t.Before(time.Now()) {
		if r.addresses.CompareAndDelete(addr, expires) {
			slog.Debug("unblocked peer", "address", addr)
		}
	}
}

// Close shuts this GateKeeper down. Calling it more than once is a no-op.
func (r *GateKeeper) Close() {
	r.closeOnce.Do(func() { close(r.done) })
}
