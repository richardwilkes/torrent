package torrent

import (
	"io"
	"net/http"
	"os"
	"time"

	"github.com/richardwilkes/errs"
)

type vfile struct {
	storage  string
	name     string
	offset   int64
	length   int64
	mode     os.FileMode
	modTime  time.Time
	children []*vfile
}

func (v *vfile) Name() string {
	return v.name
}

func (v *vfile) Size() int64 {
	return v.length
}

func (v *vfile) Mode() os.FileMode {
	return v.mode
}

func (v *vfile) ModTime() time.Time {
	return v.modTime
}

func (v *vfile) IsDir() bool {
	return (v.mode & os.ModeDir) == os.ModeDir
}

func (v *vfile) Sys() interface{} {
	return nil
}

func (v *vfile) open() (http.File, error) {
	if v.IsDir() {
		return &vdirAccess{owner: v}, nil
	}
	f, err := os.Open(v.storage)
	if err != nil {
		return nil, errs.NewfWithCause(err, "Unable to open %s", v.name)
	}
	return &vfileAccess{
		owner: v,
		file:  f,
		sr:    io.NewSectionReader(f, v.offset, v.length),
	}, nil
}

type vfileAccess struct {
	owner *vfile
	file  *os.File
	sr    *io.SectionReader
}

func (v *vfileAccess) Stat() (os.FileInfo, error) {
	return v.owner, nil
}

func (v *vfileAccess) Seek(offset int64, whence int) (int64, error) {
	if v.file == nil {
		return 0, os.ErrClosed
	}
	return v.sr.Seek(offset, whence)
}

func (v *vfileAccess) Read(p []byte) (n int, err error) {
	if v.file == nil {
		return 0, os.ErrClosed
	}
	return v.sr.Read(p)
}

func (v *vfileAccess) ReadAt(p []byte, offset int64) (n int, err error) {
	if v.file == nil {
		return 0, os.ErrClosed
	}
	return v.sr.ReadAt(p, offset)
}

func (v *vfileAccess) Readdir(count int) ([]os.FileInfo, error) {
	return nil, os.ErrInvalid
}

func (v *vfileAccess) Close() error {
	if v.file == nil {
		return os.ErrClosed
	}
	err := v.file.Close()
	v.file = nil
	v.sr = nil
	return err
}

type vdirAccess struct {
	owner  *vfile
	next   int
	done   bool
	closed bool
}

func (v *vdirAccess) Stat() (os.FileInfo, error) {
	return v.owner, nil
}

func (v *vdirAccess) Seek(offset int64, whence int) (int64, error) {
	return 0, os.ErrInvalid
}

func (v *vdirAccess) Read(p []byte) (n int, err error) {
	return 0, os.ErrInvalid
}

func (v *vdirAccess) ReadAt(p []byte, offset int64) (n int, err error) {
	return 0, os.ErrInvalid
}

func (v *vdirAccess) Readdir(count int) ([]os.FileInfo, error) {
	if v.closed {
		return nil, os.ErrClosed
	}
	if v.done {
		return nil, io.EOF
	}
	max := len(v.owner.children) - v.next
	if count < 1 {
		count = max
	}
	if count > max {
		count = max
	}
	result := make([]os.FileInfo, count)
	for i := range result {
		result[i] = v.owner.children[v.next+i]
	}
	v.next += count
	v.done = v.next == len(v.owner.children)
	return result, nil
}

func (v *vdirAccess) Close() error {
	if v.closed {
		return os.ErrClosed
	}
	v.closed = true
	return nil
}
