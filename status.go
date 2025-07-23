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
	"fmt"
	"time"

	"github.com/richardwilkes/toolbox/v2/xtime"
)

// Possible states.
const (
	Initializing State = iota
	Downloading
	Seeding
	Stopping
	Done
	Errored
)

const toMegaBytesPerSecond = 1024 * 1024

// State holds the enumeration of possible states.
type State int

// Status holds the status information for a torrent.
type Status struct {
	SeedingStopsAt         time.Time
	State                  State
	PercentComplete        float64
	TotalBytes             int64
	RemainingBytes         int64
	UploadBytesPerSecond   int
	DownloadBytesPerSecond int
	PeersDownloading       int
	PeersConnected         int
	Leechers               int
	Seeders                int
}

func (s *Status) String() string {
	switch s.State {
	case Initializing:
		return fmt.Sprintf("Initializing: %0.1f%%", s.PercentComplete)
	case Downloading:
		return fmt.Sprintf("Downloading: %.2f%% - Dn %.2f MB/s - Up %.2f MB/s - Peers %dD/%dC/%dL/%dP",
			s.PercentComplete,
			float64(s.DownloadBytesPerSecond)/toMegaBytesPerSecond,
			float64(s.UploadBytesPerSecond)/toMegaBytesPerSecond,
			s.PeersDownloading,
			s.PeersConnected,
			s.Leechers,
			s.Seeders)
	case Seeding:
		return fmt.Sprintf("Seeding: Up %.2f MB/s - %s remaining - Peers %dC/%dL/%dP",
			float64(s.UploadBytesPerSecond)/toMegaBytesPerSecond,
			xtime.FormatDuration(time.Until(s.SeedingStopsAt), false),
			s.PeersConnected,
			s.Leechers,
			s.Seeders)
	case Stopping:
		return "Cleaning up..."
	case Done:
		return "Stopped"
	case Errored:
		return "Stopped due to error"
	default:
		return "Unknown state"
	}
}
