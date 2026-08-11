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
	"bytes"
	"crypto/sha1" //nolint:gosec // The spec requires sha1
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/richardwilkes/toolbox/v2/errs"
	"github.com/richardwilkes/toolbox/v2/xfilepath"
	"github.com/richardwilkes/toolbox/v2/xstrings"
	"github.com/zeebo/bencode"
)

var _ fs.FS = &File{}

const (
	// DownloadExt is the extension used for the torrent download data file.
	DownloadExt = ".tordata"

	// MaxPieceLength is the largest piece length that will be accepted. The protocol itself sets no upper bound, but a
	// piece length becomes a buffer allocation both while verifying storage and while downloading a piece, so values
	// beyond the 16MB that sits at the top of the range real torrents use are treated as hostile rather than merely
	// unusual. Keeping it below math.MaxInt32 also means int(LengthOf(index)) can't truncate on a 32-bit platform.
	MaxPieceLength = 32 * 1024 * 1024

	// MaxPieceCount is the largest number of pieces a torrent may declare. This matches the limit other clients apply
	// by default, so a torrent beyond it is one no one else will load either. It also bounds the bit field that says
	// which pieces a peer has, since that is exchanged as a single protocol message whose size grows with the piece
	// count, as well as the piece hash data itself, which is 20 bytes per piece.
	MaxPieceCount = 1 << 21

	// maxStorageNameLength is the largest number of bytes a single path element may occupy in the storage path. 255 is
	// the limit imposed by nearly every filesystem in common use.
	maxStorageNameLength = 255

	// openOp is the operation name carried by the *fs.PathError values this package returns.
	openOp = "open"
)

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
func NewFileFromPath(filePath string) (*File, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, errs.Wrap(err)
	}
	f, err := NewFileFromReader(file)
	if err == nil {
		f.Path = filePath
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
	// The info hash must be taken over the info dictionary's bytes exactly as they appeared in the file. Hashing a
	// re-encoding of the decoded form would normalize key order and collapse duplicate keys, silently yielding a hash
	// that no tracker or peer agrees with for any torrent that wasn't already canonically encoded.
	var raw struct {
		Info bencode.RawMessage `bencode:"info"`
	}
	if err := bencode.DecodeBytes(data, &raw); err != nil {
		return nil, errs.Wrap(err)
	}
	if len(raw.Info) == 0 {
		return nil, errs.New("torrent info dictionary is missing")
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	f.InfoHash = sha1.Sum(raw.Info) //nolint:gosec // The spec requires sha1
	f.Path = xfilepath.SanitizeName(f.Info.Name)
	return &f, nil
}

// validate rejects metadata that is internally inconsistent. Without this, a corrupt or hostile .torrent file can
// drive LengthOf() to a negative or absurdly large value, which callers hand straight to make([]byte, length).
func (f *File) validate() error {
	if f.Info.Name == "" {
		return errs.New("torrent name may not be empty")
	}
	if f.Info.PieceLength <= 0 || f.Info.PieceLength > MaxPieceLength {
		return errs.Newf("invalid torrent piece length: %d", f.Info.PieceLength)
	}
	if len(f.Info.Pieces) == 0 || len(f.Info.Pieces)%sha1.Size != 0 {
		return errs.Newf("invalid torrent piece hash data length: %d", len(f.Info.Pieces))
	}
	if f.PieceCount() > MaxPieceCount {
		return errs.Newf("torrent piece count of %d exceeds the maximum of %d", f.PieceCount(), MaxPieceCount)
	}
	if f.Info.Length < 0 {
		return errs.Newf("invalid torrent length: %d", f.Info.Length)
	}
	if f.Info.Length > 0 && len(f.Info.Files) != 0 {
		return errs.New("torrent must specify either a length or a file list, but not both")
	}
	size, err := f.validateEntries()
	if err != nil {
		return err
	}
	if size <= 0 {
		return errs.New("torrent size must be greater than zero")
	}
	expected := size / f.Info.PieceLength
	if size%f.Info.PieceLength != 0 {
		expected++
	}
	if int64(f.PieceCount()) != expected {
		return errs.Newf("torrent piece count of %d doesn't match the %d expected for a size of %d and a piece length of %d",
			f.PieceCount(), expected, size, f.Info.PieceLength)
	}
	return nil
}

// validateEntries checks the individual file entries and returns their total size.
func (f *File) validateEntries() (int64, error) {
	entries := f.fileEntries()
	// Every path has to be usable as the kind of node it is used as: an entry whose path names a directory implied by
	// another entry can't be represented in the tree, and buildFS would silently drop it, leaving the data of a
	// torrent that parsed successfully unreachable.
	files := make(map[string]bool, len(entries))
	dirs := make(map[string]bool, len(entries))
	var size int64
	for _, one := range entries {
		if one.length < 0 {
			return 0, errs.Newf("invalid torrent file length: %d", one.length)
		}
		if size > math.MaxInt64-one.length {
			return 0, errs.New("torrent size overflows")
		}
		size += one.length
		p, ok := cleanVirtualPath(one.path)
		if !ok {
			return 0, errs.Newf("invalid torrent file path: %v", one.path)
		}
		if files[p] {
			return 0, errs.Newf("duplicate torrent file path: %s", p)
		}
		if dirs[p] {
			return 0, errs.Newf("torrent file path is also used as a directory: %s", p)
		}
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if files[dir] {
				return 0, errs.Newf("torrent file path is also used as a directory: %s", dir)
			}
			if dirs[dir] {
				break
			}
			dirs[dir] = true
		}
		files[p] = true
	}
	return size, nil
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
	return filepath.Join(dir, truncateName(filename, maxStorageNameLength-len(DownloadExt))+DownloadExt)
}

// truncateName shortens name to at most maxBytes bytes, cutting only at a rune boundary and never between the two
// characters of one of xfilepath.SanitizeName's "@n" escapes. A cut in either place would produce a name that
// filesystems enforcing valid UTF-8 (APFS, for one) refuse to create, or one that no longer unsanitizes to itself.
func truncateName(name string, maxBytes int) string {
	if len(name) <= maxBytes {
		return name
	}
	for maxBytes > 0 && !utf8.RuneStart(name[maxBytes]) {
		maxBytes--
	}
	// A trailing '@' can only be the first half of an escape whose second half was just cut away.
	return strings.TrimSuffix(name[:maxBytes], "@")
}

// Validate checks the supplied buffer to determine if it contains the data
// for the piece at the specified index.
func (f *File) Validate(index int, buffer []byte) bool {
	s := sha1.Sum(buffer) //nolint:gosec // The spec requires sha1
	return bytes.Equal(s[:], f.Info.Pieces[index*sha1.Size:(index+1)*sha1.Size])
}

// EmbeddedFiles returns the files embedded in the torrent file, ordered by base name, with files whose base names
// compare equal ordered by their full path. This should only be used after a torrent has completely downloaded. Note
// that the returned info carries only the base name of each file; use fs.WalkDir with this File to obtain the paths
// needed by Open.
func (f *File) EmbeddedFiles() []os.FileInfo {
	f.buildFS()
	nodes := make([]*vfs, 0, len(f.fs))
	for _, one := range f.fs {
		if !one.IsDir() {
			nodes = append(nodes, one)
		}
	}
	// The input order comes from map iteration, so the comparison has to be a total order for the result to be the
	// same from one call to the next. Base names collide freely — the same file name in two directories, or names
	// differing only in case — so equal names are broken by the full path, which is unique.
	sort.Slice(nodes, func(i, j int) bool {
		iName := nodes[i].Name()
		jName := nodes[j].Name()
		if xstrings.NaturalLess(iName, jName, true) {
			return true
		}
		if xstrings.NaturalLess(jName, iName, true) {
			return false
		}
		return nodes[i].path < nodes[j].path
	})
	files := make([]os.FileInfo, 0, len(nodes))
	for _, one := range nodes {
		files = append(files, one)
	}
	return files
}

// Open implements the fs.FS interface. As required by that interface, name must satisfy fs.ValidPath, i.e. it is an
// unrooted, slash-separated path, with the root of the torrent's content being ".".
func (f *File) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: openOp, Path: name, Err: fs.ErrInvalid}
	}
	f.buildFS()
	file, ok := f.fs[name]
	if !ok {
		return nil, &fs.PathError{Op: openOp, Path: name, Err: fs.ErrNotExist}
	}
	return file.open()
}

// fileEntry is a single file within the torrent, with the single-file form normalized into the multi-file one.
type fileEntry struct {
	path   []string
	length int64
}

// fileEntries returns the torrent's file entries, presenting the single-file form as a one-entry list so that both
// forms can be processed identically.
func (f *File) fileEntries() []fileEntry {
	if f.Info.Length > 0 {
		return []fileEntry{{path: []string{f.Info.Name}, length: f.Info.Length}}
	}
	entries := make([]fileEntry, 0, len(f.Info.Files))
	for _, one := range f.Info.Files {
		entries = append(entries, fileEntry{path: one.Path, length: one.Length})
	}
	return entries
}

func (f *File) buildFS() {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.root != nil {
		return
	}
	f.fs = make(map[string]*vfs)
	storage := f.StoragePath()
	modTime := time.Now()
	f.root = &vfs{
		storage: storage,
		path:    ".",
		mode:    os.ModeDir | 0o775,
		modTime: modTime,
	}
	f.fs[f.root.path] = f.root
	var offset int64
	for _, one := range f.fileEntries() {
		// The offset advances even for entries that can't be represented in the virtual tree, since the layout of the
		// underlying data is fixed by the torrent regardless of what we can express here.
		start := offset
		offset += one.length
		p, ok := cleanVirtualPath(one.path)
		if !ok {
			continue
		}
		// Never let an entry displace an existing node: a duplicate path, or one that collides with a directory
		// created for another entry, would otherwise corrupt the tree.
		if _, exists := f.fs[p]; exists {
			continue
		}
		dir, ok := f.mkdirs(path.Dir(p))
		if !ok {
			continue
		}
		child := &vfs{
			storage: storage,
			path:    p,
			offset:  start,
			length:  one.length,
			mode:    0o664,
			modTime: modTime,
		}
		dir.children = append(dir.children, child)
		f.fs[p] = child
	}
	sortDirs(f.root)
}

// mkdirs returns the directory node for dirPath, creating any missing nodes along the way. false is returned if part
// of the path already exists as a regular file, since such a node can't hold children.
func (f *File) mkdirs(dirPath string) (*vfs, bool) {
	dir := f.root
	if dirPath == "." || dirPath == "" {
		return dir, true
	}
	var cur strings.Builder
	for part := range strings.SplitSeq(dirPath, "/") {
		if part == "" {
			continue
		}
		if cur.Len() != 0 {
			cur.WriteByte('/')
		}
		cur.WriteString(part)
		name := cur.String()
		if existing, ok := f.fs[name]; ok {
			if !existing.IsDir() {
				return nil, false
			}
			dir = existing
			continue
		}
		d := &vfs{
			storage: dir.storage,
			path:    name,
			mode:    os.ModeDir | 0o775,
			modTime: dir.modTime,
		}
		dir.children = append(dir.children, d)
		f.fs[name] = d
		dir = d
	}
	return dir, true
}

// cleanVirtualPath turns the path components of a torrent file entry into a slash-separated path usable with io/fs,
// dropping empty and dot components rather than letting them escape or collapse onto the root. false is returned if
// nothing usable remains.
func cleanVirtualPath(parts []string) (string, bool) {
	list := make([]string, 0, len(parts))
	for _, part := range parts {
		for sub := range strings.SplitSeq(part, "/") {
			switch sub {
			case "", ".", "..":
			default:
				list = append(list, sub)
			}
		}
	}
	if len(list) == 0 {
		return "", false
	}
	result := strings.Join(list, "/")
	if !fs.ValidPath(result) {
		return "", false
	}
	return result, true
}

func sortDirs(dir *vfs) {
	if dir.IsDir() {
		sort.Slice(dir.children, func(i, j int) bool {
			iDir := dir.children[i].IsDir()
			jDir := dir.children[j].IsDir()
			if iDir == jDir {
				return xstrings.NaturalLess(dir.children[i].path, dir.children[j].path, true)
			}
			return iDir
		})
		for _, child := range dir.children {
			sortDirs(child)
		}
	}
}
