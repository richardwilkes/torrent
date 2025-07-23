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

	"github.com/richardwilkes/toolbox/v2/errs"
)

// GlobalDownloadCap sets the maximum download speed of the dispatcher.
// Default is no limit.
func GlobalDownloadCap(bytesPerSecond int) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if bytesPerSecond < 1 {
			return errs.New("GlobalDownloadCap must be at least 1")
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
			return errs.New("GlobalUploadCap must be at least 1")
		}
		d.OutRate.SetCap(bytesPerSecond)
		return nil
	}
}

// LogTo sets the logger the dispatcher should use. Default discards logs.
func LogTo(logger *slog.Logger) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		d.logger = logger
		return nil
	}
}

// PortRange sets the available ports to listen on to the specified range.
// Default is to let the system choose a random port.
func PortRange(from, to uint32) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if from < 1 || from > 65535 || to < 1 || to > 65535 {
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
