package torrent

import (
	"io"
	"os"

	"github.com/richardwilkes/errs"
)

// EmbeddedFile represents a file embedded in torrent file storage.
type EmbeddedFile struct {
	Name   string
	Dir    string
	Length int64
	offset int64
	path   string
	file   *os.File
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
