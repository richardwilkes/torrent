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
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/toolbox/v2/errs"
	"github.com/richardwilkes/toolbox/v2/xio"
	"github.com/richardwilkes/toolbox/v2/xnet"
	"github.com/richardwilkes/torrent/tfs"
)

// goroutineWait is how long a test will wait for goroutines started by a failed constructor to go away. Anything
// approaching this means they are never going away.
const goroutineWait = 5 * time.Second

// TestPortFromAddr verifies that a port can be extracted from a well-formed address and that the actual cause of a
// failure is reported for malformed ones, rather than a nil error that would leave the caller with no indication
// anything went wrong.
func TestPortFromAddr(t *testing.T) {
	c := check.New(t)
	port, err := portFromAddr("127.0.0.1:8080")
	c.NoError(err)
	c.Equal(uint32(8080), port)

	port, err = portFromAddr("[::1]:1")
	c.NoError(err)
	c.Equal(uint32(1), port)

	for _, addr := range []string{
		"127.0.0.1",       // no port at all
		"127.0.0.1:http",  // non-numeric port
		"127.0.0.1:0",     // out of range
		"127.0.0.1:65536", // out of range
		"",
	} {
		port, err = portFromAddr(addr)
		c.HasError(err, "expected an error for address %q", addr)
		c.Equal(uint32(0), port, "expected no port for address %q", addr)
	}
}

// TestNewDispatcherOptionFailureCleansUp verifies that a failing option doesn't leave the gatekeeper's prune goroutine
// running.
func TestNewDispatcherOptionFailureCleansUp(t *testing.T) {
	c := check.New(t)
	before := settledGoroutineCount(t)
	d, err := NewDispatcher(func(_ *Dispatcher) error { return errs.New("option failed") })
	c.HasError(err)
	c.Nil(d)
	waitForGoroutines(t, before)
}

// TestNewDispatcherPortRangeFailureCleansUp verifies that being unable to listen on any port in the requested range
// reports an error and doesn't leave the gatekeeper's prune goroutine running.
func TestNewDispatcherPortRangeFailureCleansUp(t *testing.T) {
	c := check.New(t)
	port, listener := occupiedPort(t)
	defer xio.CloseIgnoringErrors(listener)

	before := settledGoroutineCount(t)
	d, err := NewDispatcher(PortRange(port, port))
	c.HasError(err)
	c.Nil(d)
	waitForGoroutines(t, before)
}

// TestPortRangeFailureReportsWhyItFailed verifies that the reason no port in the range could be listened on is passed
// along, rather than being dropped in favor of a message that says only that none of them worked.
func TestPortRangeFailureReportsWhyItFailed(t *testing.T) {
	c := check.New(t)
	port, listener := occupiedPort(t)
	defer xio.CloseIgnoringErrors(listener)

	_, err := NewDispatcher(PortRange(port, port))
	c.HasError(err)
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		// Fatal, since the checks that follow would dereference a nil pointer and panic, aborting the whole test
		// binary rather than failing this one test
		t.Fatalf("the underlying listen failure must be preserved: %v", err)
	}
	c.Equal("listen", opErr.Op)
	c.Contains(err.Error(), opErr.Error())
}

// occupiedPort returns a port that is already being listened on, along with the listener holding it, which the caller
// is responsible for closing.
func occupiedPort(t *testing.T) (uint32, net.Listener) {
	t.Helper()
	// Bind on all interfaces, since that is what the dispatcher will attempt to do
	listener, err := net.Listen("tcp", ":0") //nolint:gosec // Must match what the dispatcher does to force a conflict
	if err != nil {
		t.Fatal(err)
	}
	port, err := portFromAddr(listener.Addr().String())
	if err != nil {
		xio.CloseIgnoringErrors(listener)
		t.Fatal(err)
	}
	return port, listener
}

// TestStopReleasesResources verifies that stopping a dispatcher leaves nothing of it behind. Each rate limiter owns a
// goroutine and a ticker, so a dispatcher that doesn't close them leaks both on every create and stop cycle.
func TestStopReleasesResources(t *testing.T) {
	c := check.New(t)
	before := settledGoroutineCount(t)
	// Built through the helper, whose failure is fatal: everything below uses the dispatcher, so carrying on from a
	// failed create would dereference nil and panic, taking the whole test binary down instead of failing this test
	d := newTestDispatcher(t)
	d.Stop()
	c.True(d.InRate.Closed(), "the inbound rate limiter must be closed")
	c.True(d.OutRate.Closed(), "the outbound rate limiter must be closed")
	waitForGoroutines(t, before)

	// Stopping a second time must neither panic nor block
	d.Stop()
}

// TestStoppingASecondTimeReportsNoError verifies that the supported no-op of stopping an already stopped dispatcher
// stays silent. The only thing wrong on that pass is that the listener is already closed, which is exactly what was
// asked for, so logging the "use of closed network connection" error the close returns would tell an operator a clean
// shutdown had failed when nothing about it did.
func TestStoppingASecondTimeReportsNoError(t *testing.T) {
	c := check.New(t)
	logger, sink := newTestLogger()
	d := newTestDispatcher(t, LogTo(logger))
	d.Stop()
	d.Stop()
	c.NotContains(sink.contents(), "level=ERROR", "stopping a dispatcher must not report an error")
}

// TestListenerAcceptsWhileTheExternalIPIsBeingDetermined verifies that connections are accepted without waiting on the
// external IP lookup, which consults outside sites and can take many seconds to give up when the network is
// unreachable.
func TestListenerAcceptsWhileTheExternalIPIsBeingDetermined(t *testing.T) {
	c := check.New(t)
	release := make(chan struct{})
	probing := make(chan struct{}, 1)
	d := newTestDispatcher(t, func(one *Dispatcher) error {
		one.lookupExternalIP = func(_ context.Context, _ time.Duration) net.IP {
			probing <- struct{}{}
			<-release
			return nil
		}
		return nil
	})
	// Released before the dispatcher is stopped, since a lookup still parked would keep the stop waiting. Deferred
	// rather than left to a cleanup because every deferred call runs ahead of the cleanup the helper registered.
	defer close(release)

	// The lookup is now under way and will not be completing
	select {
	case <-probing:
	case <-time.After(goroutineWait):
		t.Fatal("the external IP was never looked up")
	}

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(d.InternalPort()))))
	if err != nil {
		// Fatal, since everything that follows, the deferred close included, would use a nil connection and panic
		t.Fatal(err)
	}
	defer xio.CloseIgnoringErrors(conn)

	// A handshake that doesn't start with the protocol identifier is refused and the connection closed, which can only
	// happen if the connection was accepted
	c.NoError(conn.SetDeadline(time.Now().Add(goroutineWait)))
	_, err = conn.Write(make([]byte, len(protocolIdentifier)))
	c.NoError(err)
	_, err = conn.Read(make([]byte, 1))
	c.True(errors.Is(err, io.EOF), "the connection was never accepted: %v", err)
}

// TestAcceptRetriesTransientErrors verifies that the accept loop doesn't treat every failure as a shutdown. Accept
// returns transient errors, such as having run out of file descriptors, and abandoning the loop for one of those would
// leave the listener open with nothing accepting from it, silently losing inbound connectivity for good.
func TestAcceptRetriesTransientErrors(t *testing.T) {
	c := check.New(t)
	local, remote := newAddressedPipe(t)
	listener := &scriptedListener{script: []acceptResult{
		{err: syscall.EMFILE},
		{err: syscall.ENFILE},
		{conn: remote},
	}}
	d := newScriptedDispatcher(listener)
	defer d.gatekeeper.Close()

	// A handler of our own, so that the connection reaching it is what says it was dispatched. Without one, a
	// connection refused before the handshake — which is what the gatekeeper does with an address it can't parse —
	// would be closed just the same, and the test would pass whether dispatch worked or not.
	infoHash := tfs.InfoHash{1, 2, 3}
	dispatched := make(chan tfs.InfoHash, 1)
	d.Register(infoHash, handlerFunc(func(_ net.Conn, _ *slog.Logger, _ ProtocolExtensions, hash tfs.InfoHash,
		sendHandshake bool, _ func(),
	) {
		c.True(sendHandshake, "an inbound connection is still owed a handshake of our own")
		dispatched <- hash
	}))

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		d.listen()
	}()

	// The connection accepted after the transient failures must still be dispatched to its handler. The handshake is
	// written from a goroutine because the handler doesn't read the peer ID that trails it, so the last of it stays
	// unread until the dispatch is over and the connection closed.
	c.NoError(local.SetDeadline(time.Now().Add(goroutineWait)))
	sent := make(chan error, 1)
	go func() { sent <- SendTorrentHandshake(local, ProtocolExtensions{}, infoHash, PeerID{}) }()
	select {
	case hash := <-dispatched:
		c.Equal(infoHash, hash)
	case <-time.After(goroutineWait):
		t.Fatal("the connection accepted after the transient failures was never dispatched")
	}

	// Only the listener being closed, which the script reports once it has been used up, ends the loop
	select {
	case <-stopped:
	case <-time.After(goroutineWait):
		t.Fatal("the accept loop never stopped")
	}
	c.Equal(len(listener.script)+1, listener.accepts(), "every scripted result must have been consumed")
}

// TestPendingHandshakesAreBounded verifies that connections which have been accepted but haven't handshaken yet can't
// pile up without limit. Each one holds a socket and a goroutine for tens of seconds before any peer count limit
// applies to it, so a flood of remotes that connect and then say nothing would otherwise run us out of file
// descriptors, taking outbound dials and disk I/O down with the accept loop.
func TestPendingHandshakesAreBounded(t *testing.T) {
	c := check.New(t)
	const beyondTheLimit = 4
	script := make([]acceptResult, 0, maxPendingHandshakes+beyondTheLimit)
	locals := make([]net.Conn, 0, maxPendingHandshakes+beyondTheLimit)
	for i := range maxPendingHandshakes + beyondTheLimit {
		// Each connection comes from an address of its own, so that what refuses the last of them is the total the
		// accept loop is willing to hold rather than the share any one address is allowed of it
		local, remote := newAddressedPipeFrom(t, i+1)
		locals = append(locals, local)
		script = append(script, acceptResult{conn: remote})
	}
	d := newScriptedDispatcher(&scriptedListener{script: script})
	defer d.gatekeeper.Close()

	// The loop runs to the end of the script, so every decision about every connection has been made by the time it
	// returns. None of the accepted connections can have finished with its slot, since each is parked waiting for a
	// handshake that is never written.
	d.listen()
	c.Equal(maxPendingHandshakes, d.pendingHandshakes.count())

	// Only a handful of the connections that were kept are checked, since proving one is still open costs a read
	// deadline's worth of waiting apiece
	for i := range 3 {
		c.False(hungUpOn(locals[i]), "connection %d must still be working through its handshake", i)
	}
	for i := maxPendingHandshakes; i < len(locals); i++ {
		c.True(hungUpOn(locals[i]), "connection %d was past the limit and must have been refused", i)
	}
}

// TestPendingHandshakesAreBoundedPerAddress verifies that one remote can't hold every pending handshake slot. A slot is
// held for the tens of seconds a connection may take over its handshake, and holding it costs the remote nothing but
// the occasional byte and requires it to know no info hash we serve, so a total bound that any one host may fill
// entirely is a bound a single host can sit on: at a few connections a second it keeps the accept loop refusing every
// legitimate inbound peer for as long as it cares to, and nothing can reach us at all.
func TestPendingHandshakesAreBoundedPerAddress(t *testing.T) {
	c := check.New(t)
	const flooder = 1
	const legitimate = 2
	const beyondTheShare = 3
	script := make([]acceptResult, 0, maxPendingHandshakesPerAddress+beyondTheShare+1)
	locals := make([]net.Conn, 0, cap(script))
	for range maxPendingHandshakesPerAddress + beyondTheShare {
		local, remote := newAddressedPipeFrom(t, flooder)
		locals = append(locals, local)
		script = append(script, acceptResult{conn: remote})
	}
	// One peer that has done nothing wrong, arriving after the flood has taken everything it is going to get
	honest, honestRemote := newAddressedPipeFrom(t, legitimate)
	locals = append(locals, honest)
	script = append(script, acceptResult{conn: honestRemote})

	d := newScriptedDispatcher(&scriptedListener{script: script})
	defer d.gatekeeper.Close()

	// The loop runs to the end of the script, so every decision about every connection has been made by the time it
	// returns. None of the accepted connections can have finished with its slot, since each is parked waiting for a
	// handshake that is never written.
	d.listen()
	c.Equal(maxPendingHandshakesPerAddress+1, d.pendingHandshakes.count(),
		"the flooding address must hold its share and no more, leaving the rest of the bound free")
	c.Equal(maxPendingHandshakesPerAddress, d.pendingHandshakes.countFor(testHostIP(flooder).String()))

	// Only a handful of the connections that were kept are checked, since proving one is still open costs a read
	// deadline's worth of waiting apiece
	for i := range 3 {
		c.False(hungUpOn(locals[i]), "connection %d must still be working through its handshake", i)
	}
	for i := maxPendingHandshakesPerAddress; i < len(locals)-1; i++ {
		c.True(hungUpOn(locals[i]), "connection %d was past the address's share and must have been refused", i)
	}
	c.False(hungUpOn(honest), "a peer at another address must not be refused for what the flooding address is doing")
}

// TestPendingHandshakeSlotsAreCountedByHostAlone verifies that the per-address share is charged to the host rather than
// to the host and port together. A remote picks its own source port and gets a fresh one for every connection, so
// counted together every connection is its own address and the share bounds nothing at all.
func TestPendingHandshakeSlotsAreCountedByHostAlone(t *testing.T) {
	c := check.New(t)
	first := &net.TCPAddr{IP: testHostIP(1), Port: 51413}
	second := &net.TCPAddr{IP: testHostIP(1), Port: 51414}
	c.Equal(handshakeSlotKey(first), handshakeSlotKey(second),
		"connections from one host must share a slot key however they numbered their source ports")
	c.Equal("203.0.113.1", handshakeSlotKey(first))

	// An address that can't be split into a host and a port is counted whole rather than pooled with every other one
	// that couldn't be split either, which would have them share a single address's share between them
	c.Equal("/tmp/peer.sock", handshakeSlotKey(&net.UnixAddr{Name: "/tmp/peer.sock", Net: "unix"}))
}

// TestPendingHandshakeSlotCoversTheHandlersHandshake verifies that the slot an accepted connection takes is still held
// while the handler makes the rest of the handshake — our handshake write and the peer ID read, each with a deadline
// of its own — and is given back the moment the handler reports that exchange over, which is before the peer session
// it goes on to run. Releasing the slot as soon as the dispatcher's own reads finished would leave those seconds
// counted against no limit at all, so a remote that knows an info hash we serve could send the handshake prefix, cycle
// through the bound in milliseconds, and then withhold its peer ID, holding a socket and a goroutine apiece for the
// deadlines that follow until we run out of file descriptors. Holding it until the handler returns would be no good
// either: the session lasts as long as the remote's keep-alives do, so the bound on connections that haven't
// handshaken would become a cap on how many inbound peers we may ever have at once.
func TestPendingHandshakeSlotCoversTheHandlersHandshake(t *testing.T) {
	c := check.New(t)
	local, remote := newAddressedPipe(t)
	d := newScriptedDispatcher(&scriptedListener{script: []acceptResult{{conn: remote}}})
	defer d.gatekeeper.Close()

	infoHash := tfs.InfoHash{1, 2, 3}
	type counts struct{ duringHandshake, duringSession int }
	observed := make(chan counts, 1)
	release := make(chan struct{})
	d.Register(infoHash, handlerFunc(func(_ net.Conn, _ *slog.Logger, _ ProtocolExtensions, _ tfs.InfoHash, _ bool,
		handshakeDone func(),
	) {
		var one counts
		one.duringHandshake = d.pendingHandshakes.count()
		handshakeDone() // Stands in for the handshake write and peer ID read having completed
		one.duringSession = d.pendingHandshakes.count()
		observed <- one
		<-release // Stands in for a peer session the remote holds open with keep-alives
	}))

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		d.listen()
	}()
	c.NoError(local.SetDeadline(time.Now().Add(goroutineWait)))
	// The handshake is written from a goroutine because the handler doesn't read the peer ID that trails it, so the
	// last of it stays unread until the connection is closed
	sent := make(chan error, 1)
	go func() { sent <- SendTorrentHandshake(local, ProtocolExtensions{}, infoHash, PeerID{}) }()

	select {
	case one := <-observed:
		c.Equal(1, one.duringHandshake,
			"the handshake slot must still be held while the handler finishes the handshake")
		c.Equal(0, one.duringSession,
			"the handshake slot must be given back before the peer session is handed the connection")
	case <-time.After(goroutineWait):
		t.Fatal("the connection was never dispatched to its handler")
	}
	close(release)

	select {
	case <-stopped:
	case <-time.After(goroutineWait):
		t.Fatal("the accept loop never stopped")
	}
}

// TestPendingHandshakeSlotIsReleasedByAHandlerThatNeverReports verifies that a handler which returns without ever
// saying its handshake finished — one that failed partway through it, or an implementation that simply doesn't make
// the call — doesn't strand the slot the accept loop took for its connection. Slots lost that way accumulate until the
// bound is used up entirely by connections that are long gone and nothing new can be accepted at all.
func TestPendingHandshakeSlotIsReleasedByAHandlerThatNeverReports(t *testing.T) {
	c := check.New(t)
	local, remote := newAddressedPipe(t)
	d := newScriptedDispatcher(&scriptedListener{script: []acceptResult{{conn: remote}}})
	defer d.gatekeeper.Close()

	infoHash := tfs.InfoHash{1, 2, 3}
	handled := make(chan struct{}, 1)
	d.Register(infoHash, handlerFunc(func(_ net.Conn, _ *slog.Logger, _ ProtocolExtensions, _ tfs.InfoHash, _ bool,
		_ func(),
	) {
		handled <- struct{}{}
	}))

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		d.listen()
	}()
	c.NoError(local.SetDeadline(time.Now().Add(goroutineWait)))
	sent := make(chan error, 1)
	go func() { sent <- SendTorrentHandshake(local, ProtocolExtensions{}, infoHash, PeerID{}) }()

	select {
	case <-handled:
	case <-time.After(goroutineWait):
		t.Fatal("the connection was never dispatched to its handler")
	}

	// The handler has been entered, so the release, which its return triggers, may not have happened yet
	deadline := time.Now().Add(goroutineWait)
	for d.pendingHandshakes.count() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the handshake slot was never given back: %d still pending", d.pendingHandshakes.count())
		}
		time.Sleep(time.Millisecond)
	}

	select {
	case <-stopped:
	case <-time.After(goroutineWait):
		t.Fatal("the accept loop never stopped")
	}
}

// newScriptedDispatcher returns a dispatcher that accepts from the supplied listener, with everything that would
// otherwise reach outside the test stubbed out. The caller is responsible for closing its gatekeeper.
func newScriptedDispatcher(listener net.Listener) *Dispatcher {
	logger := slog.New(slog.DiscardHandler)
	return &Dispatcher{
		listener:         listener,
		logger:           logger,
		gatekeeper:       NewGateKeeper(logger),
		lookupExternalIP: func(_ context.Context, _ time.Duration) net.IP { return nil },
	}
}

// hungUpOn reports whether the far end of the connection has been closed, which is what the accept loop does with a
// connection it refuses. One that is still being worked on leaves the read waiting instead.
func hungUpOn(conn net.Conn) bool {
	// A pipe refuses to set a deadline at all once either of its ends has been closed
	if conn.SetReadDeadline(time.Now().Add(50*time.Millisecond)) != nil {
		return true
	}
	_, err := conn.Read(make([]byte, 1))
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

// newAddressedPipe returns the two ends of an in-memory connection, with the end handed to the dispatcher reporting a
// routable remote address. A net.Pipe's own address is "pipe", which SplitHostPort can't parse, so the gatekeeper's
// fail-closed branch refuses it before any handshake is attempted and none of the dispatch path runs.
func newAddressedPipe(t *testing.T) (local, remote net.Conn) {
	t.Helper()
	return newAddressedPipeFrom(t, 1)
}

// newAddressedPipeFrom does the same for a connection that has to be told apart from the others by where it came from,
// which is what the per-address bound on pending handshakes is decided by. The host is 203.0.113.n, from the range
// reserved for documentation, so nothing a test makes up can be an address that actually exists.
func newAddressedPipeFrom(t *testing.T, host int) (local, remote net.Conn) {
	t.Helper()
	local, other := net.Pipe()
	t.Cleanup(func() {
		xio.CloseIgnoringErrors(local)
		xio.CloseIgnoringErrors(other)
	})
	return local, &addressedConn{Conn: other, addr: &net.TCPAddr{IP: testHostIP(host), Port: 6881}}
}

// testHostIP returns the address newAddressedPipeFrom hands out for the given host, which is also the key the pending
// handshake accounting counts that host's connections against.
func testHostIP(host int) net.IP {
	return net.IPv4(203, 0, 113, byte(host))
}

// addressedConn is a connection reporting a remote address other than the one it actually has, which also ignores the
// read deadline the handshake sets on it. Tests hand this end to the dispatcher and then make assertions about the
// connections it is holding: without this, an overloaded machine could let the handshake deadline elapse partway
// through those assertions, so the connections would be closed and their slots given back while the test was still
// looking at them and it would fail for reasons of its own making. The local end keeps its deadlines, since that is
// what the tests use to bound their own reads and writes.
type addressedConn struct {
	net.Conn
	addr net.Addr
}

func (c *addressedConn) RemoteAddr() net.Addr { return c.addr }

func (c *addressedConn) SetReadDeadline(_ time.Time) error { return nil }

// TestTheDispatcherEndOfAPipeIgnoresReadDeadlines verifies the premise the tests that park handshakes rest on: the end
// of the pipe the dispatcher is given can't have a read of its time out from underneath them.
func TestTheDispatcherEndOfAPipeIgnoresReadDeadlines(t *testing.T) {
	c := check.New(t)
	_, remote := newAddressedPipe(t)
	c.NoError(remote.SetReadDeadline(time.Now().Add(-time.Hour)))
	read := make(chan error, 1)
	go func() {
		_, err := remote.Read(make([]byte, 1))
		read <- err // Buffered, so this is released by the pipe being closed when the test ends
	}()
	select {
	case err := <-read:
		t.Fatalf("the read honored a deadline that had already passed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestDeregisterOnlyRemovesItsOwnRegistration verifies that a handler which has since been replaced doesn't take the
// replacement with it when it is deregistered. Stop returns as soon as its timeout expires, whether or not the shutdown
// it asked for has finished, so a torrent started again after a timed-out stop has its new handler registered while the
// old client's run goroutine is still on its way out — and an unconditional removal there would leave the running
// client receiving no inbound connections at all, silently, for the rest of its life.
func TestDeregisterOnlyRemovesItsOwnRegistration(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	infoHash := tfs.InfoHash{1, 2, 3}
	// A function typed handler, which is not a comparable type at all, so this also covers the registrations being told
	// apart by an identity of their own rather than by comparing the handlers themselves
	noop := handlerFunc(func(_ net.Conn, _ *slog.Logger, _ ProtocolExtensions, _ tfs.InfoHash, _ bool, _ func()) {})

	old := d.Register(infoHash, noop)
	current := d.Register(infoHash, noop)
	d.Deregister(old)
	stored, ok := d.handlers.Load(infoHash)
	c.True(ok, "the replacement registration must survive the removal of the one it replaced")
	reg, isRegistration := stored.(*Registration)
	c.True(isRegistration)
	c.True(reg == current, "the registration left behind must be the replacement")

	// The current registration still removes itself, and removing one that is already gone changes nothing
	d.Deregister(current)
	_, ok = d.handlers.Load(infoHash)
	c.False(ok)
	c.NotPanics(func() { d.Deregister(current) })
	c.NotPanics(func() { d.Deregister(nil) })
}

// handlerFunc adapts a function to the ConnectionHandler interface.
type handlerFunc func(conn net.Conn, log *slog.Logger, extensions ProtocolExtensions, infoHash tfs.InfoHash,
	sendHandshake bool, handshakeDone func())

func (f handlerFunc) HandleConnection(conn net.Conn, log *slog.Logger, extensions ProtocolExtensions,
	infoHash tfs.InfoHash, sendHandshake bool, handshakeDone func(),
) {
	f(conn, log, extensions, infoHash, sendHandshake, handshakeDone)
}

// scriptedListener hands out a canned sequence of accept results, then reports that it has been closed.
type scriptedListener struct {
	script []acceptResult
	next   int
	lock   sync.Mutex
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	l.lock.Lock()
	defer l.lock.Unlock()
	if l.next >= len(l.script) {
		l.next++
		return nil, net.ErrClosed
	}
	one := l.script[l.next]
	l.next++
	return one.conn, one.err
}

func (l *scriptedListener) Close() error { return nil }

func (l *scriptedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// accepts returns the number of times Accept has been called.
func (l *scriptedListener) accepts() int {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.next
}

// newTestDispatcher creates a dispatcher for a test and stops it once the test is over. The external IP lookup is
// always stubbed out, since the real one is started by the listen goroutine as soon as the dispatcher exists: leaving
// it in place makes the test depend on outside sites being reachable and leaves a probe goroutine running for as long
// as those requests take to give up, which is the better part of a minute where the network is restricted.
func newTestDispatcher(t *testing.T, options ...func(*Dispatcher) error) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(append([]func(*Dispatcher) error{FixedExternalIP(nil)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	return d
}

// TestGateKeeperLogsWhereLogToSaid verifies that the logger LogTo supplies governs the gatekeeper's logging as well.
// LogTo exists so that a caller can keep the dispatcher's logging off of the process default logger, which a
// gatekeeper reaching for the package level slog functions would quietly ignore.
func TestGateKeeperLogsWhereLogToSaid(t *testing.T) {
	c := check.New(t)
	logger, sink := newTestLogger()
	defaultSink := captureDefaultLogger(t)
	d := newTestDispatcher(t, LogTo(logger))

	d.GateKeeper().BlockAddressString("10.0.0.1")
	c.Contains(sink.contents(), `msg="blocked peer"`)
	c.NotContains(defaultSink.contents(), "blocked peer",
		"LogTo must keep the dispatcher's logging, the gatekeeper's included, off the process default logger")
}

// newTestLogger returns a logger that records everything written to it, down to the debug level the gatekeeper uses,
// along with the sink holding what it recorded.
func newTestLogger() (*slog.Logger, *logSink) {
	sink := &logSink{}
	return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})), sink
}

// captureDefaultLogger replaces the process default logger with one that records what it is given for the duration of
// the test, so that a test can tell whether anything reached it, and puts the original back afterwards.
func captureDefaultLogger(t *testing.T) *logSink {
	t.Helper()
	logger, sink := newTestLogger()
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}

// logSink collects log output. Writes are serialized because a dispatcher logs from its own goroutines while the test
// that made it is reading what has been logged so far.
type logSink struct {
	buffer strings.Builder
	lock   sync.Mutex
}

func (s *logSink) Write(p []byte) (int, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.buffer.Write(p)
}

// contents returns everything logged so far.
func (s *logSink) contents() string {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.buffer.String()
}

// TestFixedExternalIP verifies that a supplied address is reported as our external one without any lookup being made,
// which is what keeps callers that already know their address, and the tests, off of the outside sites the real lookup
// consults.
func TestFixedExternalIP(t *testing.T) {
	c := check.New(t)
	expected := net.ParseIP("203.0.113.7")
	d := newTestDispatcher(t, FixedExternalIP(expected))
	c.NotEqual(reflect.ValueOf(xnet.ExternalIPAddress).Pointer(), reflect.ValueOf(d.lookupExternalIP).Pointer(),
		"the real external IP lookup must be replaced")
	c.Equal(expected.String(), d.ExternalIP().String())
}

// TestTestDispatchersDoNotLookUpTheRealExternalIP verifies that a dispatcher built for a test can't consult the
// outside sites the real lookup uses, no matter what options the test itself supplies.
func TestTestDispatchersDoNotLookUpTheRealExternalIP(t *testing.T) {
	c := check.New(t)
	realLookup := reflect.ValueOf(xnet.ExternalIPAddress).Pointer()
	d := newTestDispatcher(t)
	c.NotEqual(realLookup, reflect.ValueOf(d.lookupExternalIP).Pointer(), "the real external IP lookup must be replaced")
	c.Nil(d.ExternalIP())

	// An option of the test's own must not put the real lookup back
	d = newTestDispatcher(t, func(_ *Dispatcher) error { return nil })
	c.NotEqual(realLookup, reflect.ValueOf(d.lookupExternalIP).Pointer(), "the real external IP lookup must be replaced")

	// The comparison has to be able to recognize the real lookup, which is what an unstubbed dispatcher is left
	// holding, or the checks above would pass no matter what
	unstubbed := &Dispatcher{lookupExternalIP: xnet.ExternalIPAddress}
	c.Equal(realLookup, reflect.ValueOf(unstubbed.lookupExternalIP).Pointer())
}

// TestExternalIPDoesNotSerializeCallers verifies that a caller arriving while a lookup is in progress isn't blocked
// behind it, since the lookup may take many seconds to complete.
func TestExternalIPDoesNotSerializeCallers(t *testing.T) {
	c := check.New(t)
	var calls atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	d := &Dispatcher{lookupExternalIP: func(_ context.Context, _ time.Duration) net.IP {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return nil
	}}

	probed := make(chan net.IP, 1)
	go func() { probed <- d.ExternalIP() }()
	<-started

	second := make(chan net.IP, 1)
	go func() { second <- d.ExternalIP() }()
	select {
	case <-second:
	case <-time.After(goroutineWait):
		close(release)
		t.Fatal("second caller blocked behind the in-flight external IP lookup")
	}

	close(release)
	<-probed
	c.Equal(int32(1), calls.Load(), "the second caller must not have triggered a second lookup")
}

// TestExternalIPCachesFailures verifies that a failed lookup is remembered for a while, rather than every caller
// triggering another network probe.
func TestExternalIPCachesFailures(t *testing.T) {
	c := check.New(t)
	var calls atomic.Int32
	d := &Dispatcher{lookupExternalIP: func(_ context.Context, _ time.Duration) net.IP {
		calls.Add(1)
		return nil
	}}

	c.Nil(d.ExternalIP())
	c.Nil(d.ExternalIP())
	c.Equal(int32(1), calls.Load(), "a failed lookup must be cached")

	ageExternalIPCheck(d, externalIPFailureCacheDuration+time.Second)
	c.Nil(d.ExternalIP())
	c.Equal(int32(2), calls.Load(), "a failed lookup must be retried once its cache period has passed")
}

// TestExternalIPCachesSuccesses verifies that a successful lookup is cached for the full cache period, not just the
// much shorter period used for failures.
func TestExternalIPCachesSuccesses(t *testing.T) {
	c := check.New(t)
	expected := net.ParseIP("203.0.113.7")
	var calls atomic.Int32
	d := &Dispatcher{lookupExternalIP: func(_ context.Context, _ time.Duration) net.IP {
		calls.Add(1)
		return expected
	}}

	c.Equal(expected.String(), d.ExternalIP().String())
	ageExternalIPCheck(d, externalIPFailureCacheDuration+time.Second)
	c.Equal(expected.String(), d.ExternalIP().String())
	c.Equal(int32(1), calls.Load(), "a successful lookup must outlive the failure cache period")

	ageExternalIPCheck(d, externalIPCacheDuration+time.Second)
	c.Equal(expected.String(), d.ExternalIP().String())
	c.Equal(int32(2), calls.Load(), "a successful lookup must be refreshed once its cache period has passed")
}

// ageExternalIPCheck backdates the last external IP check by the given amount of time.
func ageExternalIPCheck(d *Dispatcher, age time.Duration) {
	d.lock.Lock()
	defer d.lock.Unlock()
	d.lastExternalIPCheck = d.lastExternalIPCheck.Add(-age)
}

// settledGoroutineCount returns the number of running goroutines once it has stopped changing, for use as the baseline
// a leak check measures against. A count taken while goroutines an earlier test abandoned are still winding down —
// which a failed constructor leaves behind, since the gatekeeper's pruner and the rate limiters' tickers are only
// signaled to exit — is inflated by however many of them are left, and a baseline that is too high hides a leak of
// exactly that size: the very thing the check exists to catch.
func settledGoroutineCount(t *testing.T) int {
	t.Helper()
	// Consecutive samples that agree, rather than a single one, since goroutines on their way out go a few at a time
	const settledSamples = 5
	deadline := time.Now().Add(goroutineWait)
	count := runtime.NumGoroutine()
	for agreed := 0; agreed < settledSamples; {
		time.Sleep(10 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == count {
			agreed++
			continue
		}
		count = current
		agreed = 0 // Start the run over, since the count is still moving
		if time.Now().After(deadline) {
			t.Fatalf("the goroutine count never settled; last at %d", count)
		}
	}
	return count
}

// TestSettledGoroutineCountWaitsOutStragglers verifies that the baseline a leak check starts from doesn't count
// goroutines that are already on their way out, since a baseline inflated by them would let a leak of the same size
// through unnoticed.
func TestSettledGoroutineCountWaitsOutStragglers(t *testing.T) {
	c := check.New(t)
	baseline := settledGoroutineCount(t)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { <-release })
	}
	c.True(runtime.NumGoroutine() > baseline, "the stragglers must be running before they are told to finish")

	// Told to finish, but not waited for: a count taken right now includes them
	close(release)
	count := settledGoroutineCount(t)
	wg.Wait()
	c.True(count <= baseline, "the baseline of %d counted goroutines that were on their way out (%d)", baseline, count)
}

// waitForGoroutines waits for the number of running goroutines to drop back to the count that was present before the
// call under test was made.
func waitForGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(goroutineWait)
	for {
		count := runtime.NumGoroutine()
		if count <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count never dropped back to %d; still at %d", before, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
