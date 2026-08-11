// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package dispatcher

const (
	// ChunkSize is the number of bytes of piece data a single piece message carries.
	ChunkSize = 16384

	// MaxPieceMessageLength is the number of bytes a piece message carrying a full chunk occupies on the wire,
	// including its 4-byte length prefix, its 1-byte ID and the 8 bytes identifying the range being sent. This is the
	// largest amount that is ever accounted for in a single rate limiter call: messages that are larger — a bit field
	// grows with the number of pieces in the torrent, and outgrows this one at around 131,000 pieces — are charged
	// over as many calls as it takes.
	MaxPieceMessageLength = 4 + 9 + ChunkSize

	// MinimumRateCap is the smallest download or upload speed, in bytes per second, that may be set. A limiter refuses
	// any single amount larger than its own cap and never satisfies one larger than a cap it is subordinate to, so a
	// cap below the largest amount charged in a single call would fail or permanently stall every transfer.
	MinimumRateCap = MaxPieceMessageLength
)
