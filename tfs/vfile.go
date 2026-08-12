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
	"sync"
)

// vfile is one of a torrent's files, presented as an open file over the region of the storage file that holds it. The
// os.File it stands in for may be closed while another goroutine is reading it, and answers that read with an error
// rather than a panic, so the same has to hold here: the lock is what makes the closed check mean anything, since
// without it a read that has passed the check can be descheduled past a concurrent Close and then dereference the
// section reader that Close just nil'd out. Reads serialize against each other as a consequence, which a shared
// io.SectionReader requires in any case, since the offset it advances is state the readers share.
type vfile struct {
	owner *vfs
	file  *os.File
	sr    *io.SectionReader
	lock  sync.Mutex
}

func (v *vfile) Stat() (os.FileInfo, error) {
	return v.owner, nil
}

func (v *vfile) Seek(offset int64, whence int) (int64, error) {
	v.lock.Lock()
	defer v.lock.Unlock()
	if v.file == nil {
		return 0, os.ErrClosed
	}
	return v.sr.Seek(offset, whence)
}

func (v *vfile) Read(p []byte) (int, error) {
	v.lock.Lock()
	defer v.lock.Unlock()
	if v.file == nil {
		return 0, os.ErrClosed
	}
	return v.sr.Read(p)
}

func (v *vfile) ReadAt(p []byte, offset int64) (int, error) {
	v.lock.Lock()
	defer v.lock.Unlock()
	if v.file == nil {
		return 0, os.ErrClosed
	}
	return v.sr.ReadAt(p, offset)
}

func (v *vfile) Readdir(_ int) ([]os.FileInfo, error) {
	return nil, os.ErrInvalid
}

func (v *vfile) Close() error {
	v.lock.Lock()
	defer v.lock.Unlock()
	if v.file == nil {
		return os.ErrClosed
	}
	err := v.file.Close()
	v.file = nil
	v.sr = nil
	return err
}
