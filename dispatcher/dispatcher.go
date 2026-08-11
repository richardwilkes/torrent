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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
	"github.com/richardwilkes/toolbox/v2/rate"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/toolbox/v2/xnet"
	"github.com/richardwilkes/toolbox/v2/xos"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/richardwilkes/torrent/tio"
)

const (
	// externalIPTimeout is the maximum amount of time to allow each site consulted while determining our external IP
	// address to respond.
	externalIPTimeout = 5 * time.Second
	// externalIPCacheDuration is how long a successful external IP lookup is reused before being refreshed.
	externalIPCacheDuration = time.Hour
	// externalIPFailureCacheDuration is how long a failed external IP lookup is remembered before being retried.
	// Failures are cached for much less time than successes so that a transient outage doesn't hide our address for an
	// hour, while still preventing every caller from triggering its own network probe while the failure persists.
	externalIPFailureCacheDuration = time.Minute
	// minAcceptRetryDelay and maxAcceptRetryDelay bound how long the accept loop pauses after a failure that isn't the
	// listener being closed. Backing off keeps a condition that persists, such as having run out of file descriptors,
	// from turning into a tight loop, while still recovering from it promptly once it clears.
	minAcceptRetryDelay = 5 * time.Millisecond
	maxAcceptRetryDelay = time.Second
	// maxPendingHandshakes is the number of accepted connections that may be working through their handshake at the
	// same time. Each one holds a socket and a goroutine for roughly 15 to 25 seconds — the three sequential reads of
	// ReceiveTorrentHandshake, plus the handshake write and peer ID read the handler makes, every one of them under
	// its own HandshakeDeadline — before any peer count limit applies to it, so without a bound here a flood of
	// connections that never say anything runs the process out of file descriptors, which breaks outbound dials and
	// disk I/O along with the accept loop. The slot is therefore held for all of that, not just the part of the
	// handshake the dispatcher reads itself, which would leave the rest counted against no limit at all.
	maxPendingHandshakes = 128
	// maxPendingHandshakesPerAddress is how many of those slots any one remote address may hold at once. The total on
	// its own bounds only the file descriptors, which leaves every slot available to whoever asks first: a single host
	// trickling handshake bytes holds each connection for the seconds above without ever having to name a torrent we
	// serve, so at a few connections a second it keeps the whole bound occupied and every legitimate inbound peer is
	// refused for as long as it cares to keep it up. Outbound dials still work, but nothing can reach us. Sharing an
	// address is ordinary — a swarm commonly has several peers behind one NAT — so this is a share of the bound rather
	// than a block: the address goes on being served, just not to the exclusion of everyone else. Blocking it through
	// the gatekeeper would take those NAT-mates down with the offender, and a handshake that is merely slow is not by
	// itself evidence of one.
	maxPendingHandshakesPerAddress = 8
)

// ConnectionHandler defines the interface for handling torrent connections.
type ConnectionHandler interface {
	// HandleConnection services a connection whose incoming handshake has already been read. handshakeDone, which is
	// nil when the caller isn't holding a pending handshake slot for the connection, must be called as soon as the
	// handler has finished the rest of the handshake — the handshake write and the peer ID read — and always before
	// the peer session, which the remote may hold open indefinitely, is started.
	HandleConnection(conn net.Conn, log *slog.Logger, extensions ProtocolExtensions, infoHash tfs.InfoHash,
		sendHandshake bool, handshakeDone func())
}

// Dispatcher holds a dispatcher for bit torrent connections.
type Dispatcher struct {
	InRate              rate.Limiter
	OutRate             rate.Limiter
	listener            net.Listener
	logger              *slog.Logger
	gatekeeper          *GateKeeper
	lookupExternalIP    func(ctx context.Context, timeout time.Duration) net.IP
	handlers            sync.Map
	lastExternalIPCheck time.Time // protected by lock
	externalIP          net.IP    // protected by lock
	// pendingHandshakes accounts for the accepted connections that have yet to finish handshaking, both in total and
	// per remote address. It carries a lock of its own rather than sharing the dispatcher's, since the accept loop
	// takes a slot for every connection and each dispatch goroutine gives one back on its way out, neither of which
	// has any other reason to wait behind the external IP lookup's bookkeeping.
	pendingHandshakes pendingHandshakes
	internalPort      uint32
	externalPort      uint32
	externalIPProbing bool // protected by lock
	lock              sync.Mutex
}

// pendingHandshakes is the accounting behind maxPendingHandshakes and maxPendingHandshakesPerAddress.
type pendingHandshakes struct {
	byAddress map[string]int
	total     int
	lock      sync.Mutex
}

// acquire takes a pending handshake slot for a connection from the given address, returning whether there was one to
// take. A slot that is taken must be given back with release.
//
// The counts the decision was made from come back with it, both of them read under the one hold of the lock, so that a
// caller reporting a refusal describes the state that actually caused it. Sampling them again afterwards would take the
// lock twice more per refused connection — on the single accept goroutine, while up to maxPendingHandshakes dispatch
// goroutines are contending for the same lock to give their slots back — and still report numbers that need not be the
// ones the refusal was decided from.
func (p *pendingHandshakes) acquire(addr string) (acquired bool, total, forAddress int) {
	p.lock.Lock()
	defer p.lock.Unlock()
	total, forAddress = p.total, p.byAddress[addr]
	if total >= maxPendingHandshakes || forAddress >= maxPendingHandshakesPerAddress {
		return false, total, forAddress
	}
	if p.byAddress == nil {
		p.byAddress = make(map[string]int)
	}
	p.byAddress[addr]++
	p.total++
	return true, p.total, p.byAddress[addr]
}

// release gives back a slot taken for a connection from the given address. The last one an address holds takes its
// entry with it, so that the addresses that have come and gone don't accumulate for the life of the process.
func (p *pendingHandshakes) release(addr string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	switch count := p.byAddress[addr]; {
	case count > 1:
		p.byAddress[addr] = count - 1
	case count == 1:
		delete(p.byAddress, addr)
	default:
		return
	}
	p.total--
}

// count returns how many connections are working through their handshake right now.
func (p *pendingHandshakes) count() int {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.total
}

// countFor returns how many connections from the given address are working through their handshake right now.
func (p *pendingHandshakes) countFor(addr string) int {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.byAddress[addr]
}

// handshakeSlotKey returns what an address's pending handshake slots are counted against, which is the host alone: a
// remote is free to make each of its connections from a port of its own, so counting the two together would leave the
// per-address bound meaning nothing at all. An address that can't be split is counted whole rather than pooled with
// every other unparsable one.
func handshakeSlotKey(addr net.Addr) string {
	full := addr.String()
	host, _, err := net.SplitHostPort(full)
	if err != nil {
		return full
	}
	return host
}

// NewDispatcher creates a new dispatcher and starts listening for
// connections.
func NewDispatcher(options ...func(*Dispatcher) error) (*Dispatcher, error) {
	d := &Dispatcher{
		InRate:           rate.New(math.MaxInt32, time.Second),
		OutRate:          rate.New(math.MaxInt32, time.Second),
		logger:           slog.Default(),
		lookupExternalIP: xnet.ExternalIPAddress,
	}
	var err error
	for _, option := range options {
		if err = option(d); err != nil {
			return nil, d.abandon(err)
		}
	}
	// The gatekeeper is created once the options have been applied, so that it logs wherever LogTo said the
	// dispatcher's logging should go rather than to whatever the logger happened to be beforehand
	d.gatekeeper = NewGateKeeper(d.logger)
	if d.internalPort == 0 {
		if d.listener, err = net.Listen("tcp", ":0"); err != nil { //nolint:gosec // We intentionally want all network interfaces
			return nil, d.abandon(errs.Wrap(err))
		}
		var port uint32
		if port, err = portFromAddr(d.listener.Addr().String()); err != nil {
			return nil, d.abandon(err)
		}
		d.internalPort = port
	} else {
		success := false
		for port := d.internalPort; port <= d.externalPort; port++ {
			if d.listener, err = net.Listen("tcp", fmt.Sprintf(":%d", port)); err == nil {
				success = true
				d.internalPort = port
				break
			}
		}
		if !success {
			return nil, d.abandon(errs.NewWithCausef(err, "unable to listen on any port in the range %d to %d",
				d.internalPort, d.externalPort))
		}
	}
	d.externalPort = d.internalPort
	go d.listen()
	return d, nil
}

// abandon releases the resources a partially constructed Dispatcher acquired and returns the non-nil error that caused
// the construction to fail, with any errors encountered during the cleanup appended to it.
func (d *Dispatcher) abandon(err error) error {
	// The gatekeeper isn't created until the options have been applied, so an option that failed leaves us without one
	if d.gatekeeper != nil {
		d.gatekeeper.Close()
	}
	d.closeRateLimiters()
	if d.listener != nil {
		if closeErr := d.listener.Close(); closeErr != nil {
			return errs.Append(err, closeErr)
		}
	}
	return err
}

func (d *Dispatcher) closeRateLimiters() {
	d.InRate.Close()
	d.OutRate.Close()
}

// portFromAddr extracts the port from a network address of the form "host:port".
func portFromAddr(addr string) (uint32, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, errs.Wrap(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, errs.Wrap(err)
	}
	if port < 1 || port > math.MaxUint16 {
		return 0, errs.Newf("port %d in address %q is out of range", port, addr)
	}
	return uint32(port), nil
}

// Logger returns the logger being used by this dispatcher.
func (d *Dispatcher) Logger() *slog.Logger {
	return d.logger
}

// GateKeeper returns the GateKeeper being used by this dispatcher.
func (d *Dispatcher) GateKeeper() *GateKeeper {
	return d.gatekeeper
}

// InternalPort returns the internal port that we're listening on.
func (d *Dispatcher) InternalPort() uint32 {
	return d.internalPort
}

// ExternalPort returns the external port that we're listening on.
func (d *Dispatcher) ExternalPort() uint32 {
	return atomic.LoadUint32(&d.externalPort)
}

// Stop accepting connections and shutdown. Stopping a dispatcher that has already been stopped is a no-op.
func (d *Dispatcher) Stop() {
	d.gatekeeper.Close()
	// A second stop finds the listener already closed, which is the expected outcome of a supported no-op rather than
	// a shutdown that went wrong, so, exactly as the accept loop does, net.ErrClosed isn't reported.
	if err := d.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		errs.LogTo(d.logger, err)
	}
	d.closeRateLimiters()
}

// Registration identifies what a call to Register put in place, so that the same call to Deregister can take it away
// again and nothing else. It carries the handler rather than the handler being stored directly because a registration
// has to have an identity of its own: handlers need not be comparable — a function type is a perfectly ordinary way to
// write one — so they can't be told apart from each other, and a pointer always can.
type Registration struct {
	handler  ConnectionHandler
	infoHash tfs.InfoHash
}

// Register a connection handler with this dispatcher, replacing whatever was registered for the info hash before it.
// The registration returned is what Deregister takes to remove it again.
func (d *Dispatcher) Register(infoHash tfs.InfoHash, handler ConnectionHandler) *Registration {
	reg := &Registration{handler: handler, infoHash: infoHash}
	d.handlers.Store(infoHash, reg)
	return reg
}

// Deregister a connection handler from this dispatcher, but only if the registration is still the current one for its
// info hash. A registration that has since been replaced belongs to whoever replaced it: a torrent that is stopped and
// started again registers its new handler before the old one's shutdown has necessarily finished — Stop returns as
// soon as its timeout expires, whether or not the shutdown is over — so an unconditional removal here would take the
// running client's registration away with it and leave that client receiving no inbound connections at all, silently,
// for the rest of its life. A nil registration, which is what a failed startup has, removes nothing.
func (d *Dispatcher) Deregister(reg *Registration) {
	if reg == nil {
		return
	}
	d.handlers.CompareAndDelete(reg.infoHash, reg)
}

// ExternalIP returns our external IP address, or nil if it could not be determined. The lookup is performed at most
// once per external IP cache period and never with the lock held, so callers are not serialized behind the network
// probe. If a lookup is already in progress, the last known address (which may be nil) is returned rather than waiting
// for it to complete.
func (d *Dispatcher) ExternalIP() net.IP {
	d.lock.Lock()
	if ip, ok := d.cachedExternalIP(); ok {
		d.lock.Unlock()
		return ip
	}
	if d.externalIPProbing {
		last := d.externalIP
		d.lock.Unlock()
		return last
	}
	d.externalIPProbing = true
	lookup := d.lookupExternalIP
	d.lock.Unlock()

	ip := lookup(context.Background(), externalIPTimeout)

	d.lock.Lock()
	d.externalIP = ip
	d.lastExternalIPCheck = time.Now()
	d.externalIPProbing = false
	d.lock.Unlock()
	return ip
}

// cachedExternalIP returns the last looked up external IP address and whether it is still current. Failed lookups are
// cached, too, just for a much shorter period than successful ones. d.lock must be held when calling this.
func (d *Dispatcher) cachedExternalIP() (net.IP, bool) {
	if d.lastExternalIPCheck.IsZero() {
		return nil, false
	}
	duration := externalIPFailureCacheDuration
	if d.externalIP != nil {
		duration = externalIPCacheDuration
	}
	return d.externalIP, time.Since(d.lastExternalIPCheck) < duration
}

func (d *Dispatcher) listen() {
	// The startup message is logged from its own goroutine because determining our external IP address consults
	// outside sites, which can take many seconds when the network is unreachable. Waiting for that before entering the
	// accept loop leaves the first inbound connections sitting unanswered in the kernel's backlog, long enough for the
	// peers that made them to time out their handshakes.
	go d.logListening()
	var retryDelay time.Duration
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			// Only the listener being closed means we're done. Everything else, such as having run out of file
			// descriptors, is transient, and giving up on it would leave the listener open with nothing accepting from
			// it, so remotes would go on connecting into a backlog that is never serviced for the life of the process.
			if errors.Is(err, net.ErrClosed) {
				d.logger.Info("stopped listening", "port", d.InternalPort())
				return
			}
			if retryDelay == 0 {
				retryDelay = minAcceptRetryDelay
			} else {
				retryDelay = min(2*retryDelay, maxAcceptRetryDelay)
			}
			errs.LogTo(d.logger, errs.NewWithCause("unable to accept connection", err), "retry_in", retryDelay)
			time.Sleep(retryDelay)
			continue
		}
		retryDelay = 0
		addr := handshakeSlotKey(conn.RemoteAddr())
		acquired, pending, pendingForAddress := d.pendingHandshakes.acquire(addr)
		if !acquired {
			// Either we're already holding as many un-handshaken connections as we're willing to, or this address is,
			// which is the same refusal for a different reason: the first bounds what a flood costs us, the second
			// keeps one host from being the whole of it. Hanging up returns the descriptor immediately and leaves the
			// remote free to try again, which is a great deal better than letting connections that may never say
			// anything accumulate until nothing else can open a file either.
			//
			// The line is built only when it is going to be written, since formatting the remote address allocates and
			// this is the path the bound exists to survive: a flood has the single accept goroutine here for every
			// connection it makes. The counts come from the refusal itself rather than being sampled again, so they
			// are the ones it was decided from.
			if d.logger.Enabled(context.Background(), slog.LevelDebug) {
				d.logger.Debug("refused connection", "remote_addr", conn.RemoteAddr().String(),
					"pending_handshakes", pending, "pending_handshakes_for_address", pendingForAddress)
			}
			xio.CloseIgnoringErrors(conn)
			continue
		}
		go d.dispatch(conn, addr)
	}
}

// logListening reports the address we're accepting connections on.
func (d *Dispatcher) logListening() {
	d.logger.Info("listening", "port", d.InternalPort(), "external_ip", d.ExternalIP(), "external_port", d.ExternalPort())
}

// dispatch works an accepted connection through its handshake and, if it is for a torrent we have a handler for, hands
// it off to that handler. The pending handshake slot the accept loop took for this connection is given back once the
// whole handshake is over — which the handler, being the one that makes the handshake write and the peer ID read that
// finish it, reports by calling the function it is handed — rather than when this returns: the handler goes on to run
// the peer session, which keep-alives sustain for as long as the remote likes, so a slot held until then would turn
// the bound on connections that haven't handshaken into a cap on how many inbound peers we may ever have at once. The
// slot is given back here as well, so that a handler which fails, or which never reports at all, can't strand it.
//
// The address the slot was taken under is passed in rather than derived again from the connection, so that the release
// can't be charged to a different address than the acquisition was, whatever the connection reports by then.
func (d *Dispatcher) dispatch(conn net.Conn, addr string) {
	logger := d.logger.With("remote_addr", conn.RemoteAddr().String())
	defer xio.CloseIgnoringErrors(conn)
	var once sync.Once
	handshakeDone := func() { once.Do(func() { d.pendingHandshakes.release(addr) }) }
	defer handshakeDone()
	handler, extensions, infoHash := d.handshake(conn, logger)
	if handler == nil {
		return
	}
	// A panic in the handler is contained to the connection that provoked it. ConnectionHandler is an exported
	// extension point and the handler goes on to run the entire peer session — parsing binary messages the remote
	// composes — on this goroutine, so a panic escaping it takes the process down and every other torrent the
	// dispatcher is serving along with it. The deferred release and close above run on the unwind either way; the
	// process kill is the whole of what this prevents.
	defer xos.PanicRecovery(func(err error) { errs.LogTo(logger, err) })
	handler.HandleConnection(conn, logger, extensions, infoHash, true, handshakeDone)
}

// handshake takes an accepted connection through the handshake exchange and returns the handler registered for the
// torrent the remote asked for, or nil if the connection is not one we'll be servicing.
func (d *Dispatcher) handshake(conn net.Conn, logger *slog.Logger) (ConnectionHandler, ProtocolExtensions, tfs.InfoHash) {
	var extensions ProtocolExtensions
	var infoHash tfs.InfoHash
	if d.gatekeeper.IsAddressBlocked(conn.RemoteAddr()) {
		return nil, extensions, infoHash
	}
	extensions, infoHash, err := ReceiveTorrentHandshake(conn)
	if err != nil {
		if tio.ShouldLogIOError(err) {
			errs.LogTo(logger, err)
		}
		return nil, extensions, infoHash
	}
	if stored, ok := d.handlers.Load(infoHash); ok {
		if reg, ok2 := stored.(*Registration); ok2 {
			return reg.handler, extensions, infoHash
		}
	}
	return nil, extensions, infoHash
}
