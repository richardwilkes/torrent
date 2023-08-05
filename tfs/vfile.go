package tfs

import (
	"io"
	"os"
)

type vfile struct {
	owner *vfs
	file  *os.File
	sr    *io.SectionReader
}

func (v *vfile) Stat() (os.FileInfo, error) {
	return v.owner, nil
}

func (v *vfile) Seek(offset int64, whence int) (int64, error) {
	if v.file == nil {
		return 0, os.ErrClosed
	}
	return v.sr.Seek(offset, whence)
}

func (v *vfile) Read(p []byte) (int, error) {
	if v.file == nil {
		return 0, os.ErrClosed
	}
	return v.sr.Read(p)
}

func (v *vfile) ReadAt(p []byte, offset int64) (int, error) {
	if v.file == nil {
		return 0, os.ErrClosed
	}
	return v.sr.ReadAt(p, offset)
}

func (v *vfile) Readdir(_ int) ([]os.FileInfo, error) {
	return nil, os.ErrInvalid
}

func (v *vfile) Close() error {
	if v.file == nil {
		return os.ErrClosed
	}
	err := v.file.Close()
	v.file = nil
	v.sr = nil
	return err
}
