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
	"io"
	"os"

	"github.com/richardwilkes/toolbox/v2/errs"
)

// EmbeddedFile represents a file embedded in torrent file storage.
type EmbeddedFile struct {
	file   *os.File
	path   string
	Name   string
	Dir    string
	Length int64
	offset int64
}

// Open the file for reading.
func (ef *EmbeddedFile) Open() (*io.SectionReader, error) {
	var err error
	if ef.file, err = os.Open(ef.path); err != nil {
		return nil, errs.Wrap(err)
	}
	return io.NewSectionReader(ef.file, ef.offset, ef.Length), nil
}

// Close the file. May be called more than once.
func (ef *EmbeddedFile) Close() error {
	if ef.file == nil {
		return nil
	}
	err := ef.file.Close()
	ef.file = nil
	return err
}
