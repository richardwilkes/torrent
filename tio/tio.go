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
	"strings"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
)

// ReadWithDeadline reads a buffer from a connection with a deadline.
func ReadWithDeadline(conn net.Conn, buffer []byte, deadline time.Duration) error {
	if deadline > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
			return errs.Wrap(err)
		}
	}
	_, err := io.ReadFull(conn, buffer)
	return errs.Wrap(err)
}

// WriteWithDeadline writes a buffer to a connection with a deadline.
func WriteWithDeadline(conn net.Conn, buffer []byte, deadline time.Duration) error {
	if deadline > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(deadline)); err != nil {
			return errs.Wrap(err)
		}
	}
	_, err := conn.Write(buffer)
	return errs.Wrap(err)
}

// ShouldLogIOErrorOriginal returns true if the error should be logged. (kept for use later)
func ShouldLogIOErrorOriginal(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}
	msg := err.Error()
	for _, ignore := range []string{
		"use of closed network connection",
		"operation timed out",
		"connection reset by peer",
		"i/o timeout",
		"connection refused",
	} {
		if strings.Contains(msg, ignore) {
			return false
		}
	}
	return true
}

// ShouldLogIOError returns true if the error should be logged.
func ShouldLogIOError(err error) bool {
	return err != nil && !errors.Is(err, io.EOF)
}
