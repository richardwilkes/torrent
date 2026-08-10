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
)

type vdir struct {
	owner  *vfs
	next   int
	closed bool
}

func (v *vdir) Stat() (os.FileInfo, error) {
	return v.owner, nil
}

func (v *vdir) Seek(_ int64, _ int) (int64, error) {
	return 0, os.ErrInvalid
}

func (v *vdir) Read(_ []byte) (int, error) {
	return 0, os.ErrInvalid
}

func (v *vdir) ReadAt(_ []byte, _ int64) (int, error) {
	return 0, os.ErrInvalid
}

// Readdir mimics the os.File method of the same name: with count > 0 it returns at most count entries and reports
// io.EOF once none remain, while with count <= 0 it returns everything left, which may legitimately be an empty
// slice, and never reports io.EOF.
func (v *vdir) Readdir(count int) ([]os.FileInfo, error) {
	entries, err := v.read(count)
	if err != nil {
		return nil, err
	}
	result := make([]os.FileInfo, len(entries))
	for i, one := range entries {
		result[i] = one
	}
	return result, nil
}

// ReadDir implements the fs.ReadDirFile interface, which uses the same count conventions as Readdir. Without this,
// fs.ReadDir and fs.WalkDir can't traverse the tree.
func (v *vdir) ReadDir(count int) ([]fs.DirEntry, error) {
	entries, err := v.read(count)
	if err != nil {
		return nil, err
	}
	result := make([]fs.DirEntry, len(entries))
	for i, one := range entries {
		result[i] = one
	}
	return result, nil
}

// read returns the next batch of children, advancing the read position.
func (v *vdir) read(count int) ([]*vfs, error) {
	if v.closed {
		return nil, os.ErrClosed
	}
	remaining := len(v.owner.children) - v.next
	if count > 0 {
		if remaining == 0 {
			return nil, io.EOF
		}
		if count > remaining {
			count = remaining
		}
	} else {
		count = remaining
	}
	result := v.owner.children[v.next : v.next+count]
	v.next += count
	return result, nil
}

func (v *vdir) Close() error {
	if v.closed {
		return os.ErrClosed
	}
	v.closed = true
	return nil
}
