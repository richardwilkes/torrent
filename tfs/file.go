package tfs

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // The spec requires sha1
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/richardwilkes/toolbox/errs"
	"github.com/richardwilkes/toolbox/txt"
	xfs "github.com/richardwilkes/toolbox/xio/fs"
	"github.com/zeebo/bencode"
)

var _ fs.FS = &File{}

// DownloadExt is the extension used for the torrent download data file.
const DownloadExt = ".tordata"

// InfoHash holds the hash of the torrent info.
type InfoHash [sha1.Size]byte

// File holds the contents of a .torrent file.
type File struct {
	root     *vfs            // protected by lock
	fs       map[string]*vfs // protected by lock
	Path     string          `bencode:"-"`
	Announce string          `bencode:"announce"`
	Info     struct {        //nolint:govet // We can't change the order of these fields
		Name        string     `bencode:"name"`
		PieceLength int64      `bencode:"piece length"`
		Pieces      []byte     `bencode:"pieces"`
		Length      int64      `bencode:"length,omitempty"`
		Files       []struct { //nolint:govet // We can't change the order of these fields
			Length int64    `bencode:"length"`
			Path   []string `bencode:"path"`
		} `bencode:"files,omitempty"`
		Private bool `bencode:"private"`
	} `bencode:"info"`
	InfoHash InfoHash `bencode:"-"`
	lock     sync.Mutex
}

// NewFileFromPath creates a torrent file structure from the raw torrent file data.
func NewFileFromPath(path string) (*File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	f, err := NewFileFromReader(file)
	if err == nil {
		f.Path = path
	}
	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = errs.Wrap(closeErr)
	}
	return f, err
}

// NewFileFromReader creates a torrent file structure from the raw torrent file data.
func NewFileFromReader(r io.Reader) (*File, error) {
	data, err := io.ReadAll(r)
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
	var m map[string]any
	if err := bencode.DecodeBytes(data, &m); err != nil {
		return nil, errs.Wrap(err)
	}
	data, err := bencode.EncodeBytes(m["info"])
	if err != nil {
		return nil, errs.Wrap(err)
	}
	f.InfoHash = sha1.Sum(data) //nolint:gosec // The spec requires sha1
	f.Path = xfs.SanitizeName(f.Info.Name)
	return &f, nil
}

// OffsetOf returns the offset into the data for the piece at the specified
// index.
func (f *File) OffsetOf(index int) int64 {
	return int64(index) * f.Info.PieceLength
}

// LengthOf returns the length of the piece at the specified index.
func (f *File) LengthOf(index int) int64 {
	if last := f.PieceCount() - 1; index == last {
		return f.Size() - int64(last)*f.Info.PieceLength
	}
	return f.Info.PieceLength
}

// PieceCount returns the number of pieces.
func (f *File) PieceCount() int {
	return len(f.Info.Pieces) / sha1.Size
}

// Size returns the size of the complete data.
func (f *File) Size() int64 {
	if f.Info.Length > 0 {
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
	if len(filename)+len(DownloadExt) > 255 {
		filename = filename[:255-len(DownloadExt)]
	}
	return filepath.Join(dir, filename+DownloadExt)
}

// Validate checks the supplied buffer to determine if it contains the data
// for the piece at the specified index.
func (f *File) Validate(index int, buffer []byte) bool {
	s := sha1.Sum(buffer) //nolint:gosec // The spec requires sha1
	return bytes.Equal(s[:], f.Info.Pieces[index*sha1.Size:(index+1)*sha1.Size])
}

// EmbeddedFiles returns the files embedded in the torrent file. This should
// only be used after a torrent has completely downloaded.
func (f *File) EmbeddedFiles() []os.FileInfo {
	f.buildFS()
	var files []os.FileInfo
	for _, one := range f.fs {
		if !one.IsDir() {
			files = append(files, one)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return txt.NaturalLess(files[i].Name(), files[j].Name(), true)
	})
	return files
}

// Open implements the fs.FS interface.
func (f *File) Open(name string) (fs.File, error) {
	if name == "" {
		return nil, os.ErrInvalid
	}
	if !filepath.IsAbs(name) {
		name = "/" + name
	}
	name = filepath.Clean(name)
	f.buildFS()
	file, ok := f.fs[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return file.open()
}

func (f *File) buildFS() {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.root == nil {
		f.fs = make(map[string]*vfs)
		storage := f.StoragePath()
		modTime := time.Now()
		f.root = &vfs{
			storage: storage,
			name:    "/",
			mode:    os.ModeDir | 0o775,
			modTime: modTime,
		}
		f.fs[f.root.name] = f.root
		if f.Info.Length > 0 {
			child := &vfs{
				storage: storage,
				name:    f.root.name + f.Info.Name,
				length:  f.Info.Length,
				mode:    0o664,
				modTime: modTime,
			}
			f.root.children = []*vfs{child}
			f.fs[child.name] = child
		} else {
			var offset int64
			for _, one := range f.Info.Files {
				path := filepath.Clean("/" + filepath.Join(one.Path...))
				dir := f.mkdirs(filepath.Dir(path))
				child := &vfs{
					storage: storage,
					name:    path,
					offset:  offset,
					length:  one.Length,
					mode:    0o664,
					modTime: modTime,
				}
				dir.children = append(dir.children, child)
				f.fs[child.name] = child
				offset += one.Length
			}
		}
		sortDirs(f.root)
	}
}

func (f *File) mkdirs(path string) *vfs {
	if !filepath.IsAbs(path) {
		path = "/" + path
	}
	path = filepath.Clean(path)
	dir := f.root
	cur := "/"
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			cur += "/" + part
			found := false
			for _, child := range dir.children {
				if child.name == cur {
					dir = child
					found = true
					break
				}
			}
			if !found {
				d := &vfs{
					storage: dir.storage,
					name:    cur,
					mode:    os.ModeDir | 0o775,
					modTime: dir.modTime,
				}
				dir.children = append(dir.children, d)
				f.fs[d.name] = d
				dir = d
			}
		}
	}
	return dir
}

func sortDirs(dir *vfs) {
	if dir.IsDir() {
		sort.Slice(dir.children, func(i, j int) bool {
			iDir := dir.children[i].IsDir()
			jDir := dir.children[j].IsDir()
			if iDir == jDir {
				return txt.NaturalLess(dir.children[i].name, dir.children[j].name, true)
			}
			return iDir
		})
		for _, child := range dir.children {
			sortDirs(child)
		}
	}
}
