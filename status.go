package torrent

import (
	"fmt"
	"time"
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
	Errored                bool
}

func (s Status) String() string {
	switch s.State {
	case Initializing:
		return fmt.Sprintf("Initializing: %0.1f%%", s.PercentComplete)
	case Downloading:
		return fmt.Sprintf("Downloading: %.2f%%   Dn %.2f KB/s   Up %.2f KB/s   Peers %dD/%dC/%dL/%dP",
			s.PercentComplete,
			float64(s.DownloadBytesPerSecond)/1024,
			float64(s.UploadBytesPerSecond)/1024,
			s.PeersDownloading,
			s.PeersConnected,
			s.Leechers,
			s.Seeders)
	case Seeding:
		return fmt.Sprintf("Seeding: Up %.2f KB/s   %s remaining   Peers %dC/%dL/%dP",
			float64(s.UploadBytesPerSecond)/1024,
			FormatDuration(time.Until(s.SeedingStopsAt), false),
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
