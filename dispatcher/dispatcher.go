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
)

// ConnectionHandler defines the interface for handling torrent connections.
type ConnectionHandler interface {
	HandleConnection(conn net.Conn, log *slog.Logger, extensions ProtocolExtensions, infoHash tfs.InfoHash, sendHandshake bool)
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
	internalPort        uint32
	externalPort        uint32
	externalIPProbing   bool // protected by lock
	lock                sync.Mutex
}

// NewDispatcher creates a new dispatcher and starts listening for
// connections.
func NewDispatcher(options ...func(*Dispatcher) error) (*Dispatcher, error) {
	d := &Dispatcher{
		InRate:           rate.New(math.MaxInt32, time.Second),
		OutRate:          rate.New(math.MaxInt32, time.Second),
		logger:           slog.Default(),
		gatekeeper:       NewGateKeeper(),
		lookupExternalIP: xnet.ExternalIPAddress,
	}
	var err error
	for _, option := range options {
		if err = option(d); err != nil {
			return nil, d.abandon(err)
		}
	}
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
			return nil, d.abandon(errs.Newf("unable to listen on any port in the range %d to %d", d.internalPort,
				d.externalPort))
		}
	}
	d.externalPort = d.internalPort
	go d.listen()
	return d, nil
}

// abandon releases the resources a partially constructed Dispatcher acquired and returns the non-nil error that caused
// the construction to fail, with any errors encountered during the cleanup appended to it.
func (d *Dispatcher) abandon(err error) error {
	d.gatekeeper.Close()
	d.InRate.Close()
	d.OutRate.Close()
	if d.listener != nil {
		if closeErr := d.listener.Close(); closeErr != nil {
			return errs.Append(err, closeErr)
		}
	}
	return err
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

// Stop accepting connections and shutdown.
func (d *Dispatcher) Stop() {
	d.gatekeeper.Close()
	if err := d.listener.Close(); err != nil {
		errs.LogTo(d.logger, err)
	}
}

// Register a connection handler with this dispatcher.
func (d *Dispatcher) Register(infoHash tfs.InfoHash, handler ConnectionHandler) {
	d.handlers.Store(infoHash, handler)
}

// Deregister a connection handler from this dispatcher.
func (d *Dispatcher) Deregister(infoHash tfs.InfoHash) {
	d.handlers.Delete(infoHash)
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
	d.logger.Info("listening", "port", d.InternalPort(), "external_ip", d.ExternalIP(), "external_port", d.ExternalPort())
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			d.logger.Info("stopped listening", "port", d.InternalPort())
			return
		}
		go d.dispatch(conn)
	}
}

func (d *Dispatcher) dispatch(conn net.Conn) {
	logger := d.logger.With("remote_addr", conn.RemoteAddr().String())
	defer xio.CloseIgnoringErrors(conn)
	if d.gatekeeper.IsAddressBlocked(conn.RemoteAddr()) {
		return
	}
	extensions, infoHash, err := ReceiveTorrentHandshake(conn)
	if err != nil {
		if tio.ShouldLogIOError(err) {
			errs.LogTo(logger, err)
		}
		return
	}
	if handler, ok := d.handlers.Load(infoHash); ok {
		if connHandler, ok2 := handler.(ConnectionHandler); ok2 {
			connHandler.HandleConnection(conn, logger, extensions, infoHash, true)
		}
	}
}
