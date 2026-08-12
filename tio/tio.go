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
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
)

// ReadWithDeadline reads a buffer from a connection with a deadline. A deadline of zero or less means no deadline.
func ReadWithDeadline(conn net.Conn, buffer []byte, deadline time.Duration) error {
	if err := conn.SetReadDeadline(deadlineTime(deadline)); err != nil {
		return errs.Wrap(err)
	}
	_, err := io.ReadFull(conn, buffer)
	return errs.Wrap(err)
}

// WriteWithDeadline writes a buffer to a connection with a deadline. A deadline of zero or less means no deadline.
func WriteWithDeadline(conn net.Conn, buffer []byte, deadline time.Duration) error {
	if err := conn.SetWriteDeadline(deadlineTime(deadline)); err != nil {
		return errs.Wrap(err)
	}
	_, err := conn.Write(buffer)
	return errs.Wrap(err)
}

// deadlineTime turns a caller's duration into the absolute time a connection deadline is set from. Zero or less means
// no deadline, which is the zero time rather than no call at all: a deadline stays armed on the connection until it is
// replaced, so leaving one in place would run the "no deadline" call against whatever an earlier one set — quite
// possibly a deadline that has already passed, failing the call the moment it is made.
func deadlineTime(deadline time.Duration) time.Time {
	if deadline <= 0 {
		return time.Time{}
	}
	return time.Now().Add(deadline)
}

// Winsock error codes, which are what Windows sockets actually fail with. The syscall.ECONNRESET and friends used below
// only carry their POSIX meaning on POSIX systems: on Windows they are synthetic values Go invented so the os package
// would have something to name (APPLICATION_ERROR + iota), and nothing ever returns one, so errors.Is comparing against
// them never matches a real network failure there. The numbers are spelled out here rather than taken from the syscall
// package because only two of them are exported even on Windows and none of them exist elsewhere, and they are safe to
// check on every platform: Winsock numbers everything it reports from 10000 up, far past any POSIX errno, so a match
// can only ever be a Windows socket error. The standard library resorts to the same literals in net/http for the same
// reason.
const (
	wsaeNetDown     syscall.Errno = 10050
	wsaeNetUnreach  syscall.Errno = 10051
	wsaeConnAborted syscall.Errno = 10053
	wsaeConnReset   syscall.Errno = 10054
	wsaeShutdown    syscall.Errno = 10058
	wsaeTimedOut    syscall.Errno = 10060
	wsaeConnRefused syscall.Errno = 10061
	wsaeHostUnreach syscall.Errno = 10065
)

// ShouldLogIOError returns true if the error should be logged. Peers arrive and depart constantly and most dials to
// them fail, so the ordinary ways a peer connection ends say nothing worth a log line: the peer going away mid-write
// (EPIPE) or mid-read (ECONNRESET), a dial nothing answers (ECONNREFUSED) or that never leaves our own network
// (EHOSTUNREACH, ENETUNREACH), a deadline expiring, and our own close of the connection. The sentinel and errno checks
// come first, since they hold however the error was worded or wrapped; the text match behind them covers the platforms
// and layers that report the same conditions without one of those values attached. Each condition is listed three
// times over — as a POSIX errno, as its Winsock counterpart, and as wording — because no one of those reaches every
// platform this builds for.
//
// The text match is applied only to errors a socket operation produced. Matching on wording alone is a judgment made
// from bytes, and not all of the bytes an error carries are ours: a tracker's bencoded "failure reason" is returned
// verbatim as an error, and that error is asked about here. A tracker — or anything that can answer as one — wording
// its refusal "connection refused" would otherwise silence every announce failure for as long as it cared to keep
// saying it, leaving the operator with a client that retries forever and a log with nothing in it.
func ShouldLogIOError(err error) bool {
	if err == nil {
		return false
	}
	for _, ignore := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		net.ErrClosed,
		os.ErrDeadlineExceeded,
		syscall.EPIPE,
		syscall.ECONNRESET,
		syscall.ECONNREFUSED,
		syscall.ECONNABORTED,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
		syscall.ENETDOWN,
		syscall.ETIMEDOUT,
		wsaeNetDown,
		wsaeNetUnreach,
		wsaeConnAborted,
		wsaeConnReset,
		wsaeShutdown,
		wsaeTimedOut,
		wsaeConnRefused,
		wsaeHostUnreach,
	} {
		if errors.Is(err, ignore) {
			return false
		}
	}
	if !fromSocketOperation(err) {
		return true
	}
	msg := err.Error()
	for _, ignore := range []string{
		"use of closed network connection",
		"connection reset by peer",
		"i/o timeout",
		"connection refused",
		"broken pipe",
		"no route to host",
		"network is unreachable",
		"network is down",

		// The POSIX wordings that aren't simply the condition's name. Go's errno tables are what these are taken from,
		// and the two systems don't agree on all of them: ECONNABORTED is "software caused connection abort" on both
		// darwin and linux, while ETIMEDOUT is "operation timed out" on darwin and "connection timed out" on linux, so
		// each wording has to be listed for the fallback to cover every platform this builds for.
		"software caused connection abort", // ECONNABORTED
		"operation timed out",              // ETIMEDOUT on darwin
		"connection timed out",             // ETIMEDOUT on linux

		// The Windows wordings of the same conditions, for the layers that pass the message along without the errno
		// still attached to it. ERROR_NETNAME_DELETED, which Windows reads return alongside WSAECONNRESET when the peer
		// vanishes, is matched only here: its value is 64, which collides with real POSIX errnos, so it cannot join the
		// list above the way the Winsock codes can.
		"forcibly closed by the remote host",              // WSAECONNRESET
		"aborted by the software in your host machine",    // WSAECONNABORTED
		"actively refused it",                             // WSAECONNREFUSED
		"attempted to an unreachable host",                // WSAEHOSTUNREACH
		"attempted to an unreachable network",             // WSAENETUNREACH
		"encountered a dead network",                      // WSAENETDOWN
		"did not properly respond after a period of time", // WSAETIMEDOUT
		"socket had already been shut down",               // WSAESHUTDOWN
		"network name is no longer available",             // ERROR_NETNAME_DELETED
	} {
		if strings.Contains(msg, ignore) {
			return false
		}
	}
	return true
}

// fromSocketOperation reports whether the error came out of an operation on a socket, which is what makes matching it
// by wording a judgment about our own I/O rather than about a string someone else chose. The net package wraps
// everything a connection fails with in *net.OpError, and the operating system's own errors arrive inside
// *os.SyscallError, so a condition that survived only as its wording still has one of those around it however many
// layers of ours it has passed through. What doesn't is an error assembled from a remote party's words. The value is
// what is judged rather than the match alone, since a typed nil in the chain matches while saying nothing about where
// the error came from.
func fromSocketOperation(err error) bool {
	if opErr, ok := errors.AsType[*net.OpError](err); ok && opErr != nil {
		return true
	}
	syscallErr, ok := errors.AsType[*os.SyscallError](err)
	return ok && syscallErr != nil
}
