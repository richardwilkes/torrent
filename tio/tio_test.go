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
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/toolbox/v2/xio"
)

// testDeadlineWait is how long the other end of a connection is made to wait before answering, which has to be long
// enough that a deadline left over from an earlier call would have expired well before it does.
const testDeadlineWait = 50 * time.Millisecond

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
