package torrent

import (
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
)

// DownloadCap sets the maximum download speed of the client, subject to the
// dispatcher's overall limit. Default is no limit.
func DownloadCap(bytesPerSecond int) func(*Client) error {
	return func(c *Client) error {
		if bytesPerSecond < 1 {
			return errs.New("DownloadCap must be at least 1")
		}
		c.InRate.SetCap(bytesPerSecond)
		return nil
	}
}

// UploadCap sets the maximum upload speed of the client, subject to the
// dispatcher's overall limit. Default is no limit.
func UploadCap(bytesPerSecond int) func(*Client) error {
	return func(c *Client) error {
		if bytesPerSecond < 1 {
			return errs.New("UploadCap must be at least 1")
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
// the same time. Default is 4.
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
