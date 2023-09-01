package tfs

import (
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/richardwilkes/toolbox/errs"
)

var _ fs.File = &vfile{}

type vfs struct {
	storage  string
	name     string
	offset   int64
	length   int64
	mode     os.FileMode
	modTime  time.Time
	children []*vfs
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
		return nil, errs.NewWithCausef(err, "Unable to open %s", v.name)
	}
	return &vfile{
		owner: v,
		file:  f,
		sr:    io.NewSectionReader(f, v.offset, v.length),
	}, nil
}
