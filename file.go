package torrent

import (
	"bytes"
	"crypto/sha1"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/richardwilkes/errs"
	"github.com/richardwilkes/fileutil"
	"github.com/richardwilkes/strutil"
	"github.com/zeebo/bencode"
)

// DownloadExt is the extension used for the torrent download data file.
const DownloadExt = ".tordata"

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
	lock     sync.Mutex
	root     *vfile
	fs       map[string]*vfile
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
	if len(filename)+len(DownloadExt) > 255 {
		filename = filename[:255-len(DownloadExt)]
	}
	return filepath.Join(dir, filename+DownloadExt)
}

func (f *File) validate(index int, buffer []byte) bool {
	s := sha1.Sum(buffer)
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
		return strutil.NaturalLess(files[i].Name(), files[j].Name(), true)
	})
	return files
}

// Open implements the http.FileSystem interface.
func (f *File) Open(name string) (http.File, error) {
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
		f.fs = make(map[string]*vfile)
		storage := f.StoragePath()
		modTime := time.Now()
		f.root = &vfile{
			storage: storage,
			name:    "/",
			mode:    os.ModeDir | 0775,
			modTime: modTime,
		}
		f.fs[f.root.name] = f.root
		if f.Info.Length != 0 {
			child := &vfile{
				storage: storage,
				name:    f.root.name + f.Info.Name,
				length:  f.Info.Length,
				mode:    0664,
				modTime: modTime,
			}
			f.root.children = []*vfile{child}
			f.fs[child.name] = child
		} else {
			var offset int64
			for _, one := range f.Info.Files {
				path := filepath.Clean("/" + filepath.Join(one.Path...))
				dir := f.mkdirs(filepath.Dir(path))
				child := &vfile{
					storage: storage,
					name:    path,
					offset:  offset,
					length:  one.Length,
					mode:    0664,
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

func (f *File) mkdirs(path string) *vfile {
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
				d := &vfile{
					storage: dir.storage,
					name:    cur,
					mode:    os.ModeDir | 0775,
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

func sortDirs(dir *vfile) {
	if dir.IsDir() {
		sort.Slice(dir.children, func(i, j int) bool {
			iDir := dir.children[i].IsDir()
			jDir := dir.children[j].IsDir()
			if iDir == jDir {
				return strutil.NaturalLess(dir.children[i].name, dir.children[j].name, true)
			}
			return iDir
		})
		for _, child := range dir.children {
			sortDirs(child)
		}
	}
}
