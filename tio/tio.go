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

// ShouldLogIOError returns true if the error should be logged. Peers arrive and depart constantly and most dials to
// them fail, so the ordinary ways a peer connection ends say nothing worth a log line: the peer going away mid-write
// (EPIPE) or mid-read (ECONNRESET), a dial nothing answers (ECONNREFUSED) or that never leaves our own network
// (EHOSTUNREACH, ENETUNREACH), a deadline expiring, and our own close of the connection. The sentinel and errno checks
// come first, since they hold however the error was worded or wrapped; the text match behind them covers the platforms
// and layers that report the same conditions without one of those values attached.
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
	} {
		if errors.Is(err, ignore) {
			return false
		}
	}
	msg := err.Error()
	for _, ignore := range []string{
		"use of closed network connection",
		"operation timed out",
		"connection reset by peer",
		"i/o timeout",
		"connection refused",
		"broken pipe",
		"connection aborted",
		"no route to host",
		"network is unreachable",
		"network is down",
	} {
		if strings.Contains(msg, ignore) {
			return false
		}
	}
	return true
}
