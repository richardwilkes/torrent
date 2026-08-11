// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package tfs

import (
	"crypto/sha1" //nolint:gosec // The spec requires sha1
	"testing"

	"github.com/richardwilkes/toolbox/v2/check"
)

// exampleName is the torrent name the internal tests use.
const exampleName = "example"

// TestPieceCountIsBounded verifies the limit on how many pieces a torrent may declare. The count decides the size of
// the bit field peers exchange as a single protocol message, as well as how much of everything else is allocated per
// torrent, so it can't be left to whatever a .torrent file cares to claim. validate is called directly here, since
// building the bencoded form of a torrent this large just to reach it would copy tens of megabytes to no purpose.
func TestPieceCountIsBounded(t *testing.T) {
	c := check.New(t)
	hashes := make([]byte, sha1.Size*(MaxPieceCount+1))

	// The largest piece count that may be declared is accepted
	f := fileWithPieces(hashes[:sha1.Size*MaxPieceCount])
	c.NoError(f.validate())
	c.Equal(MaxPieceCount, f.PieceCount())

	// One more is not, even though it is entirely self-consistent
	f = fileWithPieces(hashes)
	c.HasError(f.validate())
}

// fileWithPieces returns a torrent whose size matches the supplied piece hashes exactly, so that only the piece count
// itself is left for validation to object to.
func fileWithPieces(hashes []byte) *File {
	var f File
	f.Info.Name = exampleName
	f.Info.PieceLength = 16384
	f.Info.Pieces = hashes
	f.Info.Length = f.Info.PieceLength * int64(len(hashes)/sha1.Size)
	return &f
}
