package torrent

import (
	"bytes"
	"crypto/sha1"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/richardwilkes/errs"
	"github.com/richardwilkes/fileutil"
	"github.com/zeebo/bencode"
)

const downloadExt = ".tordata"

// File holds the contents of a .torrent file.
type File struct {
	Path     string `bencode:"-"`
	Announce string `bencode:"announce"`
	Info     struct {
		Name        string `bencode:"name"`
		PieceLength int    `bencode:"piece length"`
		Pieces      []byte `bencode:"pieces"`
		Length      int64  `bencode:"length,omitempty"`
		Files       []struct {
			Length int64    `bencode:"length"`
			Path   []string `bencode:"path"`
		} `bencode:"files,omitempty"`
		Private bool `bencode:"private"`
	} `bencode:"info"`
	InfoHash [sha1.Size]byte `bencode:"-"`
}

// NewFileFromPath creates a torrent file structure from the raw torrent file data.
func NewFileFromPath(path string) (*File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	f, err := NewFileFromReader(file)
	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = errs.Wrap(closeErr)
	}
	f.Path = path
	return f, err
}

// NewFileFromReader creates a torrent file structure from the raw torrent file data.
func NewFileFromReader(r io.Reader) (*File, error) {
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return NewFileFromBytes(data)
}

// NewFileFromBytes creates a torrent file structure from the raw torrent file data.
func NewFileFromBytes(data []byte) (*File, error) {
	var f File
	if err := bencode.DecodeBytes(data, &f); err != nil {
		return nil, errs.Wrap(err)
	}
	var m map[string]interface{}
	if err := bencode.DecodeBytes(data, &m); err != nil {
		return nil, errs.Wrap(err)
	}
	data, err := bencode.EncodeBytes(m["info"])
	if err != nil {
		return nil, errs.Wrap(err)
	}
	f.InfoHash = sha1.Sum(data)
	f.Path = fileutil.SanitizeName(f.Info.Name)
	return &f, nil
}

// EmbeddedFiles returns the files embedded in the torrent file. This should
// only be used after a torrent has completely downloaded.
func (f *File) EmbeddedFiles() []*EmbeddedFile {
	path := f.StoragePath()
	if f.Info.Length != 0 {
		list := make([]*EmbeddedFile, 1)
		list[0] = &EmbeddedFile{
			Name:   f.Info.Name,
			Dir:    "",
			Length: f.Info.Length,
			path:   path,
		}
		return list
	}
	list := make([]*EmbeddedFile, len(f.Info.Files))
	var offset int64
	for i, one := range f.Info.Files {
		list[i] = &EmbeddedFile{
			Name:   one.Path[len(one.Path)-1],
			Dir:    filepath.Join(one.Path[:len(one.Path)-1]...),
			Length: one.Length,
			offset: offset,
			path:   path,
		}
		offset += one.Length
	}
	return list
}

func (f *File) offsetOf(index int) int64 {
	return int64(index) * int64(f.Info.PieceLength)
}

func (f *File) lengthOf(index int) int {
	last := f.pieceCount() - 1
	if index >= last {
		return int(f.size() - int64(last)*int64(f.Info.PieceLength))
	}
	return f.Info.PieceLength
}

func (f *File) pieceCount() int {
	return len(f.Info.Pieces) / sha1.Size
}

func (f *File) size() int64 {
	if f.Info.Length != 0 {
		return f.Info.Length
	}
	var total int64
	for _, one := range f.Info.Files {
		total += one.Length
	}
	return total
}

// StoragePath returns the path that will be used for torrent file storage.
func (f *File) StoragePath() string {
	dir, filename := filepath.Split(f.Path)
	ext := filepath.Ext(filename)
	filename = filename[:len(filename)-len(ext)]
	if len(filename)+len(downloadExt) > 255 {
		filename = filename[:255-len(downloadExt)]
	}
	return filepath.Join(dir, filename+downloadExt)
}

func (f *File) validate(index int, buffer []byte) bool {
	s := sha1.Sum(buffer)
	return bytes.Equal(s[:], f.Info.Pieces[index*sha1.Size:(index+1)*sha1.Size])
}
