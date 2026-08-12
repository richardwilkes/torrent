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
	"time"

	"github.com/richardwilkes/toolbox/v2/check"
)

// TestStatusPeerCountsAreLabeledByWhatTheyCount verifies the letters in the peer summary. Each one is the initial of
// the quantity in front of it — Downloading, Connected, Leechers, Seeders — so a seeder count labeled anything else
// reads as a fourth, unrelated number rather than as the one the legend already accounts for.
func TestStatusPeerCountsAreLabeledByWhatTheyCount(t *testing.T) {
	c := check.New(t)
	s := &Status{
		State:            Downloading,
		PeersDownloading: 3,
		PeersConnected:   12,
		Leechers:         5,
		Seeders:          8,
	}
	c.Contains(s.String(), "Peers 3D/12C/5L/8S")

	s.State = Seeding
	s.SeedingStopsAt = time.Now().Add(time.Hour)
	c.Contains(s.String(), "Peers 12C/5L/8S")
}
