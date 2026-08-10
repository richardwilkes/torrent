// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package torrent

import (
	"testing"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/torrent/dispatcher"
)

// TestRateCapsTooSmallForAPieceMessageAreRejected verifies that a cap which would refuse every piece message, and
// therefore tear down the connection to each peer as soon as one arrives, isn't accepted in the first place.
func TestRateCapsTooSmallForAPieceMessageAreRejected(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	for _, one := range []struct {
		option func(int) func(*Client) error
		name   string
	}{
		{name: "DownloadCap", option: DownloadCap},
		{name: "UploadCap", option: UploadCap},
	} {
		t.Run(one.name, func(t *testing.T) {
			tc := check.New(t)
			client := newTestClient(d)
			defer client.closeRateLimiters()
			for _, bytesPerSecond := range []int{-1, 0, 1, chunkSize, dispatcher.MinimumRateCap - 1} {
				tc.HasError(one.option(bytesPerSecond)(client), "%d bytes per second must not be accepted",
					bytesPerSecond)
			}
			tc.NoError(one.option(dispatcher.MinimumRateCap)(client))
			tc.NoError(one.option(dispatcher.MinimumRateCap + 1)(client))
		})
	}
}

// TestMinimumRateCapPermitsAPieceMessage verifies that the minimum cap is actually large enough for the largest amount
// we ever ask a rate limiter to account for at once, and that anything smaller really is refused by the limiter, which
// is what makes the minimum necessary.
func TestMinimumRateCapPermitsAPieceMessage(t *testing.T) {
	c := check.New(t)
	d, err := dispatcher.NewDispatcher()
	c.NoError(err)
	defer d.Stop()
	client := newTestClient(d)
	defer client.closeRateLimiters()

	c.NoError(DownloadCap(dispatcher.MinimumRateCap)(client))
	c.NoError(UploadCap(dispatcher.MinimumRateCap)(client))
	c.NoError(<-client.InRate.Use(dispatcher.MaxPieceMessageLength))
	c.NoError(<-client.OutRate.Use(dispatcher.MaxPieceMessageLength))

	client.OutRate.SetCap(dispatcher.MinimumRateCap - 1)
	c.HasError(<-client.OutRate.Use(dispatcher.MaxPieceMessageLength))
}
