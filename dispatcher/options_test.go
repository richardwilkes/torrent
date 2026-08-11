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
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
)

// rateSettleTime is how long a test will wait for a rate limiter request that can never be satisfied to prove it isn't
// going to be. It must be longer than the one second period the limiters use, so that the request is reconsidered.
const rateSettleTime = 2500 * time.Millisecond

// TestLogToReplacesTheDefaultLogger verifies both halves of what LogTo documents: that a dispatcher nobody gave a
// logger to writes to the process's default logger, and that supplying one puts it in place of that. A caller has to
// be able to trust which of the two it gets, since the dispatcher logs accept failures and a line apiece for starting
// and stopping listening.
func TestLogToReplacesTheDefaultLogger(t *testing.T) {
	c := check.New(t)
	c.Equal(slog.Default(), newTestDispatcher(t).Logger())

	logger := slog.New(slog.DiscardHandler)
	c.Equal(logger, newTestDispatcher(t, LogTo(logger)).Logger())
}

// TestLogToSubstitutesForANilLogger verifies that a nil logger falls back to the process default rather than being
// stored. The dispatcher's own goroutines reach for the logger without checking it — the listen goroutine logs the
// line saying it started, every accept failure, and the line saying it stopped — so a stored nil is a nil pointer
// panic on a goroutine no caller can recover from, taking the whole process down rather than reporting anything.
func TestLogToSubstitutesForANilLogger(t *testing.T) {
	c := check.New(t)
	sink := captureDefaultLogger(t)
	d := newTestDispatcher(t, LogTo(nil))
	c.Equal(slog.Default(), d.Logger(), "a nil logger must not be handed back out")

	// The whole of the listen goroutine's logging is exercised: it logs that it started as soon as it has a port, and
	// that it stopped once the listener is closed, both of which would panic on a nil logger
	waitForLoggedText(t, sink, `msg=listening`)
	d.Stop()
	waitForLoggedText(t, sink, `msg="stopped listening"`)
}

// waitForLoggedText fails the test if the text doesn't reach the sink promptly. The dispatcher logs from its own
// goroutines, so what it has written by the time the call that provoked it returns isn't fixed.
func waitForLoggedText(t *testing.T, sink *logSink, want string) {
	t.Helper()
	deadline := time.Now().Add(goroutineWait)
	for {
		if strings.Contains(sink.contents(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q was never logged; what was logged was: %s", want, sink.contents())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestGlobalRateCapsTooSmallForAPieceMessageAreRejected verifies that a global cap which would stall every client
// using the dispatcher isn't accepted in the first place.
func TestGlobalRateCapsTooSmallForAPieceMessageAreRejected(t *testing.T) {
	d := newTestDispatcher(t)
	for _, one := range []struct {
		option func(int) func(*Dispatcher) error
		name   string
	}{
		{name: "GlobalDownloadCap", option: GlobalDownloadCap},
		{name: "GlobalUploadCap", option: GlobalUploadCap},
	} {
		t.Run(one.name, func(t *testing.T) {
			tc := check.New(t)
			for _, bytesPerSecond := range []int{-1, 0, 1, ChunkSize, MinimumRateCap - 1} {
				tc.HasError(one.option(bytesPerSecond)(d), "%d bytes per second must not be accepted", bytesPerSecond)
			}
			tc.NoError(one.option(MinimumRateCap)(d))
			tc.NoError(one.option(MinimumRateCap + 1)(d))
		})
	}
}

// TestMinimumGlobalRateCapPermitsAPieceMessage verifies that a client's limiter, which is subordinate to the
// dispatcher's, can pass a whole piece message when the global cap is at the minimum, and that a global cap one byte
// below it can never carry that same request, which is what makes the minimum necessary.
func TestMinimumGlobalRateCapPermitsAPieceMessage(t *testing.T) {
	c := check.New(t)
	d := newTestDispatcher(t)
	c.NoError(GlobalUploadCap(MinimumRateCap)(d))
	client := d.OutRate.New(math.MaxInt32)
	defer client.Close()
	select {
	case uerr := <-client.Use(MaxPieceMessageLength):
		c.NoError(uerr)
	case <-time.After(rateSettleTime):
		t.Fatal("a piece message could not be sent at the minimum global cap")
	}

	// A request the limiter tree can never satisfy is refused outright, rather than being left queued for capacity
	// that will never come, so the peer that made it is disconnected instead of stalling for good
	d.OutRate.SetCap(MinimumRateCap - 1)
	select {
	case uerr := <-client.Use(MaxPieceMessageLength):
		c.HasError(uerr, "a piece message must not be possible below the minimum global cap")
	case <-time.After(rateSettleTime):
		t.Fatal("a piece message below the minimum global cap was neither refused nor sent")
	}
}
