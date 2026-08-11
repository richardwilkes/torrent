// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package tio

import (
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/toolbox/v2/errs"
	"github.com/richardwilkes/toolbox/v2/xio"
)

// testDeadlineWait is how long the other end of a connection is made to wait before answering, which has to be long
// enough that a deadline left over from an earlier call would have expired well before it does.
const testDeadlineWait = 50 * time.Millisecond

// bareMessagePrefix is what the net package puts in front of the reason a connection operation failed, which is all
// that is left of an error whose errno an intervening layer stripped away.
const bareMessagePrefix = "read tcp 10.0.0.1:6881->10.0.0.2:51413: "

// TestReadWithNoDeadlineClearsAPreviousOne verifies that a read asking for no deadline clears whatever an earlier call
// armed on the connection instead of leaving it in place. A deadline stays on the connection until it is replaced, so
// a caller mixing deadline'd and "no deadline" reads on the same connection would otherwise have the "no deadline"
// ones fail immediately against an already expired deadline that was never theirs.
func TestReadWithNoDeadlineClearsAPreviousOne(t *testing.T) {
	c := check.New(t)
	local, remote := net.Pipe()
	defer xio.CloseIgnoringErrors(local)
	defer xio.CloseIgnoringErrors(remote)

	// A read with a deadline that nothing answers, leaving the connection carrying a deadline in the past
	buffer := make([]byte, 4)
	c.HasError(ReadWithDeadline(local, buffer, time.Millisecond))

	// The read that follows asks for no deadline, so the expired one must not be what it gets
	go func() {
		time.Sleep(testDeadlineWait)
		_, _ = remote.Write([]byte("data")) //nolint:errcheck // The read below is what the test is judging
	}()
	c.NoError(ReadWithDeadline(local, buffer, 0))
	c.Equal("data", string(buffer))

	// Which holds for a negative duration as well, the other way of asking for no deadline
	c.HasError(ReadWithDeadline(local, buffer, time.Millisecond))
	go func() {
		time.Sleep(testDeadlineWait)
		_, _ = remote.Write([]byte("more")) //nolint:errcheck // The read below is what the test is judging
	}()
	c.NoError(ReadWithDeadline(local, buffer, -time.Second))
	c.Equal("more", string(buffer))
}

// TestWriteWithNoDeadlineClearsAPreviousOne verifies the same for writes, which arm a deadline of their own on the
// connection.
func TestWriteWithNoDeadlineClearsAPreviousOne(t *testing.T) {
	c := check.New(t)
	local, remote := net.Pipe()
	defer xio.CloseIgnoringErrors(local)
	defer xio.CloseIgnoringErrors(remote)

	// A write with a deadline that nothing is there to receive, leaving a deadline in the past on the connection
	c.HasError(WriteWithDeadline(local, []byte("data"), time.Millisecond))

	// The write that follows asks for no deadline, so it must wait for the reader rather than fail on that deadline
	received := make(chan string, 1)
	go func() {
		time.Sleep(testDeadlineWait)
		buffer := make([]byte, 4)
		if _, err := io.ReadFull(remote, buffer); err != nil {
			received <- err.Error()
			return
		}
		received <- string(buffer)
	}()
	c.NoError(WriteWithDeadline(local, []byte("data"), 0))
	select {
	case got := <-received:
		c.Equal("data", got)
	case <-time.After(testDeadlineWait * 20):
		t.Fatal("the write never reached the other end")
	}
}

// TestShouldLogIOErrorIgnoresRoutinePeerErrors verifies that the ordinary ways a peer connection ends stay out of the
// log. A peer departing mid-write, a dial nothing answers and a route that doesn't exist happen constantly across a
// swarm and say nothing about this client, so logging them buries whatever else the log had to say.
func TestShouldLogIOErrorIgnoresRoutinePeerErrors(t *testing.T) {
	for _, one := range []struct {
		err  error
		name string
		log  bool
	}{
		{name: "no error", err: nil},
		{name: "end of stream", err: io.EOF},
		{name: "truncated read", err: errs.Wrap(io.ErrUnexpectedEOF)},
		{name: "our own close", err: errs.Wrap(net.ErrClosed)},
		{name: "deadline", err: errs.Wrap(os.ErrDeadlineExceeded)},
		{name: "peer gone mid-write", err: wrappedOpError("write", syscall.EPIPE)},
		{name: "peer reset", err: wrappedOpError("read", syscall.ECONNRESET)},
		{name: "dial refused", err: wrappedOpError("dial", syscall.ECONNREFUSED)},
		{name: "host unreachable", err: wrappedOpError("dial", syscall.EHOSTUNREACH)},
		{name: "network unreachable", err: wrappedOpError("dial", syscall.ENETUNREACH)},
		{name: "broken pipe by text alone", err: errs.New("write tcp 10.0.0.1:6881->10.0.0.2:51413: broken pipe")},
		{name: "no route to host by text alone", err: errs.New("dial tcp 10.0.0.2:51413: no route to host")},
		{name: "unreachable network by text alone", err: errs.New("dial tcp 10.0.0.2:51413: network is unreachable")},
		{name: "anything else", err: errs.New("the piece hash didn't match"), log: true},
		{name: "a wrapped anything else", err: errs.Wrap(io.ErrShortWrite), log: true},
	} {
		t.Run(one.name, func(t *testing.T) {
			check.New(t).Equal(one.log, ShouldLogIOError(one.err))
		})
	}
}

// wrappedOpError builds the error a connection actually hands back for a failed operation: the operating system's
// error, inside the net package's own, inside the wrapper every call site here puts around it. Matching has to hold
// through all of that, since nothing above the syscall ever sees the bare error.
func wrappedOpError(op string, err error) error {
	return errs.Wrap(&net.OpError{Op: op, Net: "tcp", Err: &os.SyscallError{Syscall: op, Err: err}})
}

// wrappedTextError builds the error a connection hands back for a condition that survived only as its wording, the
// errno having been stripped by some intervening layer, inside the same wrapping a real one arrives in.
func wrappedTextError(msg string) error {
	return errs.Wrap(&net.OpError{Op: "read", Net: "tcp", Err: errs.New(msg)})
}

// TestShouldLogIOErrorIgnoresPOSIXPeerErrorWordings verifies that the text fallback recognizes the routine peer
// departures as the POSIX systems actually word them, which is all an error carries once it has crossed a boundary that
// kept only the message. Wording a condition by its own name is a guess rather than a rule — ECONNABORTED is "software
// caused connection abort" and never "connection aborted" — and the two systems don't even agree with each other, since
// ETIMEDOUT is "operation timed out" on darwin and "connection timed out" on linux, so every wording has to be listed
// for the fallback to cover the platforms this builds for.
func TestShouldLogIOErrorIgnoresPOSIXPeerErrorWordings(t *testing.T) {
	for _, one := range []struct {
		name string
		msg  string
	}{
		{name: "peer gone mid-write by POSIX wording", msg: "broken pipe"},
		{name: "peer reset by POSIX wording", msg: "connection reset by peer"},
		{name: "dial refused by POSIX wording", msg: "connection refused"},
		{name: "connection aborted by POSIX wording", msg: "software caused connection abort"},
		{name: "host unreachable by POSIX wording", msg: "no route to host"},
		{name: "network unreachable by POSIX wording", msg: "network is unreachable"},
		{name: "network down by POSIX wording", msg: "network is down"},
		{name: "timed out by the darwin wording", msg: "operation timed out"},
		{name: "timed out by the linux wording", msg: "connection timed out"},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			c.False(ShouldLogIOError(errs.New(bareMessagePrefix+one.msg)), "the bare message: %s", one.msg)
			c.False(ShouldLogIOError(wrappedTextError(one.msg)), "the wrapped message: %s", one.msg)
		})
	}
}

// TestShouldLogIOErrorIgnoresThisPlatformsErrorWordings verifies the same against the wordings this platform itself
// puts on those conditions, so that a system whose phrasing the list above doesn't anticipate is caught here rather
// than by the log filling up with routine peer departures. Windows is left out because the POSIX errno values it
// defines are synthetic ones nothing ever returns, so the messages they carry there describe nothing the platform
// produces; the Winsock codes and wordings it does produce are covered by the tests below.
func TestShouldLogIOErrorIgnoresThisPlatformsErrorWordings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the POSIX errno values are synthetic on Windows")
	}
	for _, one := range []struct {
		name  string
		errno syscall.Errno
	}{
		{name: "peer gone mid-write as worded here", errno: syscall.EPIPE},
		{name: "peer reset as worded here", errno: syscall.ECONNRESET},
		{name: "dial refused as worded here", errno: syscall.ECONNREFUSED},
		{name: "connection aborted as worded here", errno: syscall.ECONNABORTED},
		{name: "host unreachable as worded here", errno: syscall.EHOSTUNREACH},
		{name: "network unreachable as worded here", errno: syscall.ENETUNREACH},
		{name: "network down as worded here", errno: syscall.ENETDOWN},
		{name: "timed out as worded here", errno: syscall.ETIMEDOUT},
	} {
		t.Run(one.name, func(t *testing.T) {
			// The errno is deliberately dropped, leaving only what it was worded as: with it still attached the
			// sentinel checks would answer and the fallback this is about would never be consulted.
			msg := one.errno.Error()
			check.New(t).False(ShouldLogIOError(errs.New(bareMessagePrefix+msg)),
				"%s is worded %q here, which the fallback doesn't recognize", one.name, msg)
		})
	}
}

// TestShouldLogIOErrorIgnoresWindowsPeerErrors verifies that the routine peer errors stay out of the log on Windows
// too, which reports them with neither the errno nor the wording the POSIX systems use. A Windows socket fails with a
// Winsock code, while the syscall.ECONNRESET and friends that exist on Windows are synthetic values nothing ever
// returns, so matching against those alone would log every reset, refused dial and unreachable host the swarm produces.
// The codes are written as numbers rather than named constants for the same reason the library writes them that way:
// they aren't exported off Windows, and running this everywhere is the point, since a platform nothing here runs on is
// where the mistake was made in the first place.
func TestShouldLogIOErrorIgnoresWindowsPeerErrors(t *testing.T) {
	for _, one := range []struct {
		err  error
		name string
		log  bool
	}{
		// The Winsock code as the socket hands it back, inside the wrapping it has by the time anything sees it
		{name: "peer reset by code", err: wrappedOpError("wsarecv", syscall.Errno(10054))},
		{name: "connection aborted by code", err: wrappedOpError("wsasend", syscall.Errno(10053))},
		{name: "dial refused by code", err: wrappedOpError("connectex", syscall.Errno(10061))},
		{name: "host unreachable by code", err: wrappedOpError("connectex", syscall.Errno(10065))},
		{name: "network unreachable by code", err: wrappedOpError("connectex", syscall.Errno(10051))},
		{name: "network down by code", err: wrappedOpError("wsasend", syscall.Errno(10050))},
		{name: "socket shut down by code", err: wrappedOpError("wsasend", syscall.Errno(10058))},
		{name: "connect timed out by code", err: wrappedOpError("connectex", syscall.Errno(10060))},

		// A Winsock code that isn't one of the routine departures still has to reach the log, since matching the range
		// rather than the specific codes would silence everything the platform reports
		{name: "a Winsock error worth hearing about", err: wrappedOpError("wsarecv", syscall.Errno(10014)), log: true},
	} {
		t.Run(one.name, func(t *testing.T) {
			check.New(t).Equal(one.log, ShouldLogIOError(one.err))
		})
	}
}

// TestShouldLogIOErrorIgnoresWindowsPeerErrorWordings verifies the same against the messages Windows words those codes
// with, which is all an error carries once it has crossed a boundary that kept only the text.
func TestShouldLogIOErrorIgnoresWindowsPeerErrorWordings(t *testing.T) {
	for _, one := range []struct {
		name string
		msg  string
	}{
		{name: "peer reset by wording", msg: "An existing connection was forcibly closed by the remote host."},
		{name: "connection aborted by wording", msg: "An established connection was aborted by the software in your host machine."},
		{name: "dial refused by wording", msg: "No connection could be made because the target machine actively refused it."},
		{name: "host unreachable by wording", msg: "A socket operation was attempted to an unreachable host."},
		{name: "network unreachable by wording", msg: "A socket operation was attempted to an unreachable network."},
		{name: "network down by wording", msg: "A socket operation encountered a dead network."},
		{name: "connect timed out by wording", msg: "A connection attempt failed because the connected party did not properly " +
			"respond after a period of time, or established connection failed because connected host has failed to " +
			"respond."},
		{name: "socket shut down by wording", msg: "A request to send or receive data was disallowed because the socket had " +
			"already been shut down in that direction with a previous shutdown call."},

		// The peer vanishing mid-read also surfaces as ERROR_NETNAME_DELETED, whose value collides with a POSIX errno
		// and so can only ever be recognized by its wording
		{name: "network name deleted by wording", msg: "The specified network name is no longer available."},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			c.False(ShouldLogIOError(errs.New("wsarecv: "+one.msg)), "the bare message: %s", one.msg)
			c.False(ShouldLogIOError(wrappedTextError(one.msg)), "the wrapped message: %s", one.msg)
		})
	}
}

// TestShouldLogIOErrorIgnoresARealPeerHangingUp exercises the same against an error the operating system produced,
// rather than one assembled to look like it, since the wording and the errno both vary by platform.
func TestShouldLogIOErrorIgnoresARealPeerHangingUp(t *testing.T) {
	c := check.New(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	c.NoError(err)
	defer xio.CloseIgnoringErrors(listener)
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	c.NoError(err)
	defer xio.CloseIgnoringErrors(conn)

	// A nil here is the accept having failed, which the receive below is bounded against in any case; everything that
	// follows dereferences it, so it ends the test rather than merely failing an assertion.
	var remote net.Conn
	select {
	case remote = <-accepted:
	case <-time.After(testDeadlineWait * 100):
	}
	if remote == nil {
		t.Fatal("the connection was never accepted")
	}

	// The peer goes away without reading a thing, which is what a peer disconnecting mid-transfer looks like from
	// here. A zero linger makes the close a reset, so what follows fails the way it would against a peer that vanished
	// rather than one that shut down politely.
	if tcp, ok := remote.(*net.TCPConn); ok {
		c.NoError(tcp.SetLinger(0))
	}
	c.NoError(remote.Close())

	// Writing into that takes a few passes to fail: the first of them only fills the send buffer.
	buffer := make([]byte, 64*1024)
	var writeErr error
	deadline := time.Now().Add(testDeadlineWait * 100)
	for writeErr == nil && time.Now().Before(deadline) {
		writeErr = WriteWithDeadline(conn, buffer, testDeadlineWait*10)
	}
	c.HasError(writeErr)
	c.False(ShouldLogIOError(writeErr), "a peer hanging up mid-write isn't worth a log line: %v", writeErr)
}

// TestDeadlineStillApplies verifies that the deadline a caller asks for is still armed, since clearing a stale one is
// only correct if the deadline that was actually requested continues to bound the call.
func TestDeadlineStillApplies(t *testing.T) {
	c := check.New(t)
	local, remote := net.Pipe()
	defer xio.CloseIgnoringErrors(local)
	defer xio.CloseIgnoringErrors(remote)

	// Nothing is reading or writing on the other end, so both calls can only come back by way of their deadline
	started := time.Now()
	c.HasError(ReadWithDeadline(local, make([]byte, 4), 10*time.Millisecond))
	c.HasError(WriteWithDeadline(local, []byte("data"), 10*time.Millisecond))
	c.True(time.Since(started) < testDeadlineWait*10, "the deadlines took far longer than they were given")
}
