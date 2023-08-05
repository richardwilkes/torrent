package tfs

import (
	"io"
	"os"
)

type vdir struct {
	owner  *vfs
	next   int
	done   bool
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

func (v *vdir) Readdir(count int) ([]os.FileInfo, error) {
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

func (v *vdir) Close() error {
	if v.closed {
		return os.ErrClosed
	}
	v.closed = true
	return nil
}
