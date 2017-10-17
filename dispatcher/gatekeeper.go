package dispatcher

import (
	"net"
	"sync"
	"time"
)

const blockDuration = 15 * time.Minute

// GateKeeper controls whether peers with a given address may connect with us.
type GateKeeper struct {
	addresses sync.Map
	done      chan bool
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
	expires, ok := r.addresses.Load(addr)
	return ok && !expires.(time.Time).Before(time.Now())
}

func (r *GateKeeper) prune() {
	for {
		select {
		case <-time.After(blockDuration):
			r.addresses.Range(func(addr interface{}, expires interface{}) bool {
				if expires.(time.Time).Before(time.Now()) {
					r.addresses.Delete(addr)
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
