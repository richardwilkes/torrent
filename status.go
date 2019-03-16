package torrent

import (
	"fmt"
	"time"

	"github.com/richardwilkes/toolbox/txt"
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

const toMegaBitsPerSecond = 1024 * 1024 / 8

// State holds the enumeration of possible states.
type State int

// Status holds the status information for a torrent.
type Status struct {
	State                  State
	PercentComplete        float64
	UploadBytesPerSecond   int
	DownloadBytesPerSecond int
	PeersDownloading       int
	PeersConnected         int
	Leechers               int
	Seeders                int
	SeedingStopsAt         time.Time
}

func (s *Status) String() string {
	switch s.State {
	case Initializing:
		return fmt.Sprintf("Initializing: %0.1f%%", s.PercentComplete)
	case Downloading:
		return fmt.Sprintf("Downloading: %.2f%% - Dn %.2f Mbps - Up %.2f Mbps - Peers %dD/%dC/%dL/%dP",
			s.PercentComplete,
			float64(s.DownloadBytesPerSecond)/toMegaBitsPerSecond,
			float64(s.UploadBytesPerSecond)/toMegaBitsPerSecond,
			s.PeersDownloading,
			s.PeersConnected,
			s.Leechers,
			s.Seeders)
	case Seeding:
		return fmt.Sprintf("Seeding: Up %.2f Mbps - %s remaining - Peers %dC/%dL/%dP",
			float64(s.UploadBytesPerSecond)/toMegaBitsPerSecond,
			txt.FormatDuration(time.Until(s.SeedingStopsAt), false),
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
