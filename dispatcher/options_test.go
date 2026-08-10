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
	"math"
	"testing"
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
)

// rateSettleTime is how long a test will wait for a rate limiter request that can never be satisfied to prove it isn't
// going to be. It must be longer than the one second period the limiters use, so that the request is reconsidered.
const rateSettleTime = 2500 * time.Millisecond

// TestGlobalRateCapsTooSmallForAPieceMessageAreRejected verifies that a global cap which would stall every client
// using the dispatcher isn't accepted in the first place.
func TestGlobalRateCapsTooSmallForAPieceMessageAreRejected(t *testing.T) {
	c := check.New(t)
	d, err := NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
	d, err := NewDispatcher()
	c.NoError(err)
	defer d.Stop()
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
