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
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
	"github.com/richardwilkes/torrent/dispatcher"
)

// DownloadCap sets the maximum download speed of the client, subject to the
// dispatcher's overall limit. Must be at least dispatcher.MinimumRateCap.
// Default is no limit.
func DownloadCap(bytesPerSecond int) func(*Client) error {
	return func(c *Client) error {
		if bytesPerSecond < dispatcher.MinimumRateCap {
			return errs.Newf("DownloadCap must be at least %d", dispatcher.MinimumRateCap)
		}
		c.InRate.SetCap(bytesPerSecond)
		return nil
	}
}

// UploadCap sets the maximum upload speed of the client, subject to the
// dispatcher's overall limit. Must be at least dispatcher.MinimumRateCap.
// Default is no limit.
func UploadCap(bytesPerSecond int) func(*Client) error {
	return func(c *Client) error {
		if bytesPerSecond < dispatcher.MinimumRateCap {
			return errs.Newf("UploadCap must be at least %d", dispatcher.MinimumRateCap)
		}
		c.OutRate.SetCap(bytesPerSecond)
		return nil
	}
}

// PeersWanted sets the number of peers to ask the tracker for. Default is 32.
func PeersWanted(wanted int) func(*Client) error {
	return func(c *Client) error {
		if wanted < 1 {
			return errs.New("PeersWanted must be at least 1")
		}
		c.peersWanted = wanted
		return nil
	}
}

// ConcurrentDownloads sets the number of peers to actively download from at
// the same time. Peers beyond this many are still connected to, and still
// uploaded to, but are not asked for pieces until one of the peers downloading
// finishes or gives up its piece. Default is 4.
//
// What the cap governs is how many peers hold a fresh claim on a piece. In the
// endgame, once every piece still missing has already been claimed, further
// peers may take a duplicate of a claim regardless of the cap — the peers
// holding it are the ones being backed up — with at most maxPieceOwners peers
// fetching any one piece.
func ConcurrentDownloads(concurrent int) func(*Client) error {
	return func(c *Client) error {
		if concurrent < 1 {
			return errs.New("ConcurrentDownloads must be at least 1")
		}
		c.concurrentDownloads = concurrent
		return nil
	}
}

// SeedDuration sets the maximum amount of time to seed. Default is 4 days.
func SeedDuration(duration time.Duration) func(*Client) error {
	return func(c *Client) error {
		if duration < 0 {
			return errs.New("SeedDuration must be at least 0")
		}
		c.seedDuration = duration
		return nil
	}
}

// NotifyWhenDownloadComplete sets a channel to notify when the download is
// verified and completes.
func NotifyWhenDownloadComplete(notifier chan *Client) func(*Client) error {
	return func(c *Client) error {
		c.downloadCompleteNotifier = notifier
		return nil
	}
}

// NotifyWhenStopped sets a channel to notify when the client is stopped.
func NotifyWhenStopped(notifier chan *Client) func(*Client) error {
	return func(c *Client) error {
		c.stoppedNotifier = notifier
		return nil
	}
}
