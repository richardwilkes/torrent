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
			r.addresses.Range(func(addr, expires any) bool {
				if t, ok := expires.(time.Time); ok && t.Before(time.Now()) {
					r.addresses.Delete(addr)
					slog.Debug("unblocked peer", "address", addr)
				}
				return true
			})
		case <-r.done:
			return
		}
	}
}

// Close shuts this GateKeeper down.
func (r *GateKeeper) Close() {
	close(r.done)
}
