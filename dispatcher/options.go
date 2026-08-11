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
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
)

// GlobalDownloadCap sets the maximum download speed of the dispatcher, which also caps every client using it. Must be
// at least MinimumRateCap. Default is no limit.
func GlobalDownloadCap(bytesPerSecond int) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if bytesPerSecond < MinimumRateCap {
			return errs.Newf("GlobalDownloadCap must be at least %d", MinimumRateCap)
		}
		d.InRate.SetCap(bytesPerSecond)
		return nil
	}
}

// GlobalUploadCap sets the maximum upload speed of the dispatcher, which also caps every client using it. Must be at
// least MinimumRateCap. Default is no limit.
func GlobalUploadCap(bytesPerSecond int) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if bytesPerSecond < MinimumRateCap {
			return errs.Newf("GlobalUploadCap must be at least %d", MinimumRateCap)
		}
		d.OutRate.SetCap(bytesPerSecond)
		return nil
	}
}

// FixedExternalIP sets the external IP address the dispatcher reports, rather than having it consult outside sites to
// determine one. Pass nil to report no external address at all. Callers that already know their address, and tests,
// which have no business issuing network requests to third parties, want this. Default is to look the address up.
func FixedExternalIP(ip net.IP) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		d.lookupExternalIP = func(_ context.Context, _ time.Duration) net.IP { return ip }
		return nil
	}
}

// LogTo sets the logger the dispatcher should use. Default is slog.Default(), so a caller that wants the dispatcher's
// logging — accept failures, and the lines saying we started and stopped listening — kept off of the process's default
// logger has to say where it should go instead, which may be a logger built on slog.DiscardHandler.
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
