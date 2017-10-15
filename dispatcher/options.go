package dispatcher

import (
	"github.com/richardwilkes/errs"
	"github.com/richardwilkes/logadapter"
)

// GlobalDownloadCap sets the maximum download speed of the dispatcher.
// Default is no limit.
func GlobalDownloadCap(bytesPerSecond int) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if bytesPerSecond < 1 {
			return errs.New("DownloadCap must be at least 1")
		}
		d.InRate.SetCap(bytesPerSecond)
		return nil
	}
}

// GlobalUploadCap sets the maximum upload speed of the dispatcher. Default
// is no limit.
func GlobalUploadCap(bytesPerSecond int) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if bytesPerSecond < 1 {
			return errs.New("UploadCap must be at least 1")
		}
		d.OutRate.SetCap(bytesPerSecond)
		return nil
	}
}

// LogTo sets the logger the dispatcher should use. Default discards logs.
func LogTo(logger logadapter.Logger) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		d.logger = logger
		return nil
	}
}

// UseNATPMP sets the dispatcher to use NatPMP to route traffic through the
// external gateway.
func UseNATPMP(d *Dispatcher) error {
	d.natpmpChan = make(chan interface{}, 1)
	return nil
}

// PortRange sets the available ports to listen on to the specified range.
// Default is to let the system choose a random port.
func PortRange(from, to uint32) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if from < 1 || from > 65536 || to < 1 || to > 65536 {
			return errs.New("Ports must be in the range 1 to 65535")
		}
		if from > to {
			d.internalPort = to
			d.externalPort = from
		} else {
			d.internalPort = from
			d.externalPort = to
		}
		return nil
	}
}
