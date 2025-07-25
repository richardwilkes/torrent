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
	"io/fs"
	"os"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
)

var _ fs.File = &vfile{}

type vfs struct {
	storage  string
	name     string
	modTime  time.Time
	children []*vfs
	offset   int64
	length   int64
	mode     os.FileMode
}

func (v *vfs) Name() string {
	return v.name
}

func (v *vfs) Size() int64 {
	return v.length
}

func (v *vfs) Mode() os.FileMode {
	return v.mode
}

func (v *vfs) ModTime() time.Time {
	return v.modTime
}

func (v *vfs) IsDir() bool {
	return (v.mode & os.ModeDir) == os.ModeDir
}

func (v *vfs) Sys() any {
	return nil
}

func (v *vfs) open() (fs.File, error) {
	if v.IsDir() {
		return &vdir{owner: v}, nil
	}
	f, err := os.Open(v.storage)
	if err != nil {
		return nil, errs.NewWithCausef(err, "unable to open %s", v.name)
	}
	return &vfile{
		owner: v,
		file:  f,
		sr:    io.NewSectionReader(f, v.offset, v.length),
	}, nil
}
