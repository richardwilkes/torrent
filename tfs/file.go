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

	// MaxTorrentFileLength is the largest .torrent file that will be accepted. A torrent declaring MaxPieceCount
	// pieces carries 40MB of piece hashes by itself, so the bound has to sit above that; what it rules out is an
	// input that exists only to make the parser allocate, which matters because a reader handed to
	// NewFileFromReader may be a network stream with no end to it.
	MaxTorrentFileLength = 64 * 1024 * 1024

	// MaxFileCount is the largest number of file entries a torrent may declare. Each entry is materialized several
	// times over — twice while its path is validated, again in the map backing the virtual file system, and again as a
	// node in the tree it hangs from — so the resident cost of a torrent is a sizable multiple of the bytes it spent
	// describing its files, and MaxTorrentFileLength alone leaves that unbounded: a maximally sized file of minimal
	// entries carries millions of them. Real torrents run to the thousands of files, so this sits far above anything
	// legitimate while keeping what the metadata can cost us in the tens of megabytes.
	MaxFileCount = 1 << 17

	// MaxNestingDepth is the deepest that bencoded lists and dictionaries may nest. The decoder recurses once per
	// level, and a torrent may be as little as a megabyte of nothing but list heads, so the depth has to be bounded
	// before decoding rather than after. Real torrents sit at five levels or so, and even the per-component
	// dictionaries of a v2 file tree stay far below this.
	MaxNestingDepth = 64

	// MaxPathDepth is the largest number of components the path of a file within a torrent may have. Every ancestor
	// of an entry is visited while validating and again while building the virtual tree, so an unbounded depth makes
	// a single entry cost O(depth²) to process; the tree is also walked recursively when sorting it.
	MaxPathDepth = 64

	// MaxPathLength is the largest number of bytes the path of a file within a torrent may occupy, separators
	// included. This matches the limit nearly every filesystem in common use places on a path, and together with
	// MaxPathDepth it keeps the work done per entry proportional to the bytes the torrent spent on it.
	MaxPathLength = 4096

	// maxStorageNameLength is the largest number of bytes a single path element may occupy in the storage path. 255 is
	// the limit imposed by nearly every filesystem in common use.
	maxStorageNameLength = 255

	// maxPathInError is the largest number of bytes of a rejected path that an error message will quote. The path
	// came out of the torrent, so it can be as large as the file that carried it.
	maxPathInError = 96

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

// NewFileFromReader creates a torrent file structure from the raw torrent file data. At most MaxTorrentFileLength
// bytes are read, since the reader may be attached to something unbounded, such as a network stream.
func NewFileFromReader(r io.Reader) (*File, error) {
	// One byte beyond the limit is read so that a file sitting exactly at it is still accepted while anything larger
	// is seen to be larger and rejected by NewFileFromBytes.
	data, err := io.ReadAll(io.LimitReader(r, MaxTorrentFileLength+1))
	if err != nil {
		return nil, errs.Wrap(err)
	}
	return NewFileFromBytes(data)
}

// NewFileFromBytes creates a torrent file structure from the raw torrent file data.
func NewFileFromBytes(data []byte) (*File, error) {
	if len(data) > MaxTorrentFileLength {
		return nil, errs.Newf("torrent file length of %d exceeds the maximum of %d", len(data), MaxTorrentFileLength)
	}
	if err := checkNesting(data); err != nil {
		return nil, err
	}
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

// checkNesting rejects data the decoder can't be handed safely: bencoded lists and dictionaries that nest more than
// MaxNestingDepth levels deep, and strings claiming more bytes than the data actually holds. The decoder recurses once
// per level, so a file that is little more than a long run of list heads drives it past the goroutine stack limit, and
// the resulting stack overflow is a fatal runtime error that no recover() can catch. It also allocates a string's
// declared length before reading it, so a few dozen bytes claiming a gigabyte make it allocate a gigabyte and only
// then report that the file was truncated — a bound on the size of the file is no help there, since it is the claimed
// length rather than the bytes present that sizes the allocation. Both therefore have to be judged before the decoder
// ever sees the bytes. Nothing else is: input this scan can't make sense of is left for the decoder to report, since
// the decoder walks the same tokens the same way and stops at the same place, without ever recursing deeper or
// allocating more than what was measured up to it.
func checkNesting(data []byte) error {
	depth := 0
	for i := 0; i < len(data); {
		switch c := data[i]; {
		case c == 'd' || c == 'l':
			depth++
			if depth > MaxNestingDepth {
				return errs.Newf("torrent data nests more than %d levels deep", MaxNestingDepth)
			}
			i++
		case c == 'e':
			depth--
			i++
		case c == 'i':
			end := bytes.IndexByte(data[i+1:], 'e')
			if end == -1 {
				return nil
			}
			i += end + 2
		case c >= '0' && c <= '9':
			next, err := skipBencodedString(data, i)
			if err != nil {
				return err
			}
			if next < 0 {
				return nil
			}
			i = next
		default:
			return nil
		}
		if depth <= 0 {
			// The value that started the data is complete, which is exactly where the decoder stops as well.
			return nil
		}
	}
	return nil
}

// skipBencodedString returns the index just past the bencoded string starting at i. A negative index means what is
// there isn't a string the decoder could read either, which is left for the decoder to report along with everything
// else this scan doesn't judge. An error is returned for the one case that can't wait for the decoder: a string
// claiming more bytes than the data holds, which the decoder allocates in full before discovering there is nothing to
// fill it with.
func skipBencodedString(data []byte, i int) (int, error) {
	length := 0
	for ; i < len(data) && data[i] >= '0' && data[i] <= '9'; i++ {
		length = length*10 + int(data[i]-'0')
		if length > len(data) {
			// Already beyond everything we hold, so no digit that follows could bring it back within reach, and
			// accumulating more of them is how the length itself overflows.
			return 0, errs.Newf("torrent data declares a string of at least %d bytes, but holds only %d", length,
				len(data))
		}
	}
	if i == len(data) || data[i] != ':' {
		return -1, nil
	}
	i++
	if length > len(data)-i {
		return 0, errs.Newf("torrent data declares a %d byte string, but only %d bytes remain", length, len(data)-i)
	}
	return i + length, nil
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
	// Checked before the entries are walked, since the walk is what turns each of them into the structures the bound
	// exists to limit.
	if len(f.Info.Files) > MaxFileCount {
		return errs.Newf("torrent file count of %d exceeds the maximum of %d", len(f.Info.Files), MaxFileCount)
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
			return 0, errs.Newf("invalid torrent file path: %q", describePath(one.path))
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
// nothing usable remains, or if the result would be deeper than MaxPathDepth or longer than MaxPathLength, since
// everything downstream of here does work per ancestor of a path and a torrent is free to claim any depth at all.
func cleanVirtualPath(parts []string) (string, bool) {
	list := make([]string, 0, min(len(parts), MaxPathDepth))
	length := 0
	for _, part := range parts {
		for sub := range strings.SplitSeq(part, "/") {
			switch sub {
			case "", ".", "..":
			default:
				if len(list) == MaxPathDepth {
					return "", false
				}
				if length != 0 {
					length++ // The separator this component will be joined with.
				}
				if length += len(sub); length > MaxPathLength {
					return "", false
				}
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

// describePath renders the path components of a torrent file entry for an error message. Only the first
// maxPathInError bytes are shown, cut at a rune boundary, since a path that was rejected for its size would
// otherwise put the whole of it into the message.
func describePath(parts []string) string {
	var buf strings.Builder
	for i, part := range parts {
		if i != 0 {
			buf.WriteByte('/')
		}
		if remaining := maxPathInError + 1 - buf.Len(); remaining <= len(part) {
			if remaining > 0 {
				buf.WriteString(part[:remaining])
			}
			break
		}
		buf.WriteString(part)
	}
	result := buf.String()
	if len(result) <= maxPathInError {
		return result
	}
	cut := maxPathInError
	for cut > 0 && !utf8.RuneStart(result[cut]) {
		cut--
	}
	return result[:cut] + "..."
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
