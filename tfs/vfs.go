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
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"time"
)

var (
	_ fs.File        = &vfile{}
	_ fs.ReadDirFile = &vdir{}
	_ fs.FileInfo    = &vfs{}
	_ fs.DirEntry    = &vfs{}
)

type vfs struct {
	storage  string
	path     string // The full, slash-separated, fs.ValidPath-conforming path of this node; the root is "."
	modTime  time.Time
	children []*vfs
	offset   int64
	length   int64
	mode     os.FileMode
}

// Name implements the fs.FileInfo and fs.DirEntry interfaces, both of which require the base name of the entry
// rather than the full path.
func (v *vfs) Name() string {
	return path.Base(v.path)
}

// Type implements the fs.DirEntry interface.
func (v *vfs) Type() fs.FileMode {
	return v.mode.Type()
}

// Info implements the fs.DirEntry interface.
func (v *vfs) Info() (fs.FileInfo, error) {
	return v, nil
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
		// The fs.FS contract calls for an *fs.PathError naming the path that was opened. The one os.Open produced
		// names the on-disk storage file, which isn't part of this filesystem's namespace, so only its underlying
		// cause is carried over.
		if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
			err = pathErr.Err
		}
		return nil, &fs.PathError{Op: openOp, Path: v.path, Err: err}
	}
	return &vfile{
		owner: v,
		file:  f,
		sr:    io.NewSectionReader(f, v.offset, v.length),
	}, nil
}
