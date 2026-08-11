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
	before := runtime.NumGoroutine()
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

	before := runtime.NumGoroutine()
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
	c.True(errors.As(err, &opErr), "the underlying listen failure must be preserved: %v", err)
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
	before := runtime.NumGoroutine()
	d, err := NewDispatcher(FixedExternalIP(nil))
	c.NoError(err)
	d.Stop()
	c.True(d.InRate.Closed(), "the inbound rate limiter must be closed")
	c.True(d.OutRate.Closed(), "the outbound rate limiter must be closed")
	waitForGoroutines(t, before)

	// Stopping a second time must neither panic nor block
	d.Stop()
}

// TestListenerAcceptsWhileTheExternalIPIsBeingDetermined verifies that connections are accepted without waiting on the
// external IP lookup, which consults outside sites and can take many seconds to give up when the network is
// unreachable.
func TestListenerAcceptsWhileTheExternalIPIsBeingDetermined(t *testing.T) {
	c := check.New(t)
	release := make(chan struct{})
	probing := make(chan struct{}, 1)
	d, err := NewDispatcher(func(one *Dispatcher) error {
		one.lookupExternalIP = func(_ context.Context, _ time.Duration) net.IP {
			probing <- struct{}{}
			<-release
			return nil
		}
		return nil
	})
	c.NoError(err)
	defer d.Stop()
	defer close(release)

	// The lookup is now under way and will not be completing
	select {
	case <-probing:
	case <-time.After(goroutineWait):
		t.Fatal("the external IP was never looked up")
	}

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(d.InternalPort()))))
	c.NoError(err)
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
		sendHandshake bool,
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
	for range maxPendingHandshakes + beyondTheLimit {
		local, remote := newAddressedPipe(t)
		locals = append(locals, local)
		script = append(script, acceptResult{conn: remote})
	}
	d := newScriptedDispatcher(&scriptedListener{script: script})
	defer d.gatekeeper.Close()

	// The loop runs to the end of the script, so every decision about every connection has been made by the time it
	// returns. None of the accepted connections can have finished with its slot, since each is parked waiting for a
	// handshake that is never written.
	d.listen()
	c.Equal(int32(maxPendingHandshakes), d.pendingHandshakes.Load())

	// Only a handful of the connections that were kept are checked, since proving one is still open costs a read
	// deadline's worth of waiting apiece
	for i := range 3 {
		c.False(hungUpOn(locals[i]), "connection %d must still be working through its handshake", i)
	}
	for i := maxPendingHandshakes; i < len(locals); i++ {
		c.True(hungUpOn(locals[i]), "connection %d was past the limit and must have been refused", i)
	}
}

// newScriptedDispatcher returns a dispatcher that accepts from the supplied listener, with everything that would
// otherwise reach outside the test stubbed out. The caller is responsible for closing its gatekeeper.
func newScriptedDispatcher(listener net.Listener) *Dispatcher {
	return &Dispatcher{
		listener:         listener,
		logger:           slog.New(slog.DiscardHandler),
		gatekeeper:       NewGateKeeper(),
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
	local, other := net.Pipe()
	t.Cleanup(func() {
		xio.CloseIgnoringErrors(local)
		xio.CloseIgnoringErrors(other)
	})
	return local, &addressedConn{Conn: other, addr: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 6881}}
}

// addressedConn is a connection reporting a remote address other than the one it actually has.
type addressedConn struct {
	net.Conn
	addr net.Addr
}

func (c *addressedConn) RemoteAddr() net.Addr { return c.addr }

// handlerFunc adapts a function to the ConnectionHandler interface.
type handlerFunc func(conn net.Conn, log *slog.Logger, extensions ProtocolExtensions, infoHash tfs.InfoHash, sendHandshake bool)

func (f handlerFunc) HandleConnection(conn net.Conn, log *slog.Logger, extensions ProtocolExtensions,
	infoHash tfs.InfoHash, sendHandshake bool,
) {
	f(conn, log, extensions, infoHash, sendHandshake)
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
