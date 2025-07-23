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
	handlers            sync.Map
	lastExternalIPCheck time.Time // protected by lock
	externalIP          net.IP    // protected by lock
	internalPort        uint32
	externalPort        uint32
	lock                sync.Mutex
}

// NewDispatcher creates a new dispatcher and starts listening for
// connections.
func NewDispatcher(options ...func(*Dispatcher) error) (*Dispatcher, error) {
	d := &Dispatcher{
		InRate:     rate.New(math.MaxInt32, time.Second),
		OutRate:    rate.New(math.MaxInt32, time.Second),
		logger:     slog.Default(),
		gatekeeper: NewGateKeeper(),
	}
	var err error
	for _, option := range options {
		if err = option(d); err != nil {
			return nil, err
		}
	}
	if d.internalPort == 0 {
		if d.listener, err = net.Listen("tcp", ":0"); err != nil { //nolint:gosec // We intentionally want all network interfaces
			return nil, errs.Wrap(err)
		}
		_, portStr, lerr := net.SplitHostPort(d.listener.Addr().String())
		if lerr != nil {
			lerr = errs.Wrap(err)
			if err = d.listener.Close(); err != nil {
				lerr = errs.Append(lerr, err)
			}
			return nil, lerr
		}
		port, lerr := strconv.Atoi(portStr)
		if lerr != nil {
			lerr = errs.Wrap(err)
			if err = d.listener.Close(); err != nil {
				lerr = errs.Append(lerr, err)
			}
			return nil, lerr
		}
		d.internalPort = uint32(port)
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
			return nil, errs.Newf("unable to listen on any port in the range %d to %d", d.internalPort, d.externalPort)
		}
	}
	d.externalPort = d.internalPort
	go d.listen()
	return d, nil
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

// ExternalIP returns our external IP address.
func (d *Dispatcher) ExternalIP() net.IP {
	d.lock.Lock()
	defer d.lock.Unlock()
	if time.Since(d.lastExternalIPCheck) < time.Hour && d.externalIP != nil {
		return d.externalIP
	}
	d.externalIP = xnet.ExternalIPAddress(context.Background(), 5*time.Second)
	d.lastExternalIPCheck = time.Now()
	return d.externalIP
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
