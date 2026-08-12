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
	"encoding/hex"
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

var (
	_ fs.FS     = &File{}
	_ fs.StatFS = &File{}
)

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

	// MaxNameLength is the largest number of bytes the torrent's name may occupy. The single-file form already carries
	// this bound, since there the name is the whole of the entry's path and cleanVirtualPath judges it as such, but
	// the multi-file form never routes it through there while NewFileFromBytes still materializes a sanitized copy of
	// it — up to twice its length — into Path, where it is retained for the life of the File and stamped onto every
	// log line naming the torrent. Nothing ever uses more than the 206 bytes StoragePath truncates the name to, so
	// matching MaxPathLength keeps every torrent that was acceptable before acceptable while denying a 64MB file
	// carrying nothing but a name the gigabyte of allocation that name would otherwise cost to parse.
	MaxNameLength = MaxPathLength

	// componentDelimiters are the bytes that end a path component somewhere a path of ours may be used. A backslash
	// separates components on Windows, so a component carrying one is several components there: the lone,
	// unremarkable looking node `..\..\evil.exe` is two levels above the directory a consumer joined it onto. A NUL
	// ends the path outright in every interface that hands it to the operating system as a C string.
	componentDelimiters = "\\\x00"

	// maxReservedNameLength is the length of the longest name Windows resolves to a device ("COM9" and its kind),
	// which bounds the work usableComponent does before it can rule a component out as one of them.
	maxReservedNameLength = 4

	// maxStorageNameLength is the largest number of bytes a single path element may occupy in the storage path. 255 is
	// the limit imposed by nearly every filesystem in common use.
	maxStorageNameLength = 255

	// maxPathInError is the largest number of bytes of a rejected path that an error message will quote. The path
	// came out of the torrent, so it can be as large as the file that carried it.
	maxPathInError = 96

	// openOp is the operation name carried by the *fs.PathError values this package returns.
	openOp = "open"

	// statOp is the operation name carried by the *fs.PathError values Stat returns.
	statOp = "stat"
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
	// size is the total the metadata declares, worked out while it was being validated. Nothing changes it afterwards,
	// so Size can read it without any synchronization of its own from the goroutines a peer's messages drive.
	size int64
	lock sync.Mutex
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
	// The info hash must be taken over the info dictionary's bytes exactly as they appeared in the file. Hashing a
	// re-encoding of the decoded form would normalize key order and collapse duplicate keys, silently yielding a hash
	// that no tracker or peer agrees with for any torrent that wasn't already canonically encoded.
	var raw struct {
		Announce string             `bencode:"announce"`
		Info     bencode.RawMessage `bencode:"info"`
	}
	if err := bencode.DecodeBytes(data, &raw); err != nil {
		return nil, errs.Wrap(err)
	}
	if len(raw.Info) == 0 {
		return nil, errs.New("torrent info dictionary is missing")
	}
	// The metadata is decoded from those same bytes rather than from the file a second time, so that what we hold and
	// what we hashed can't describe different torrents. A file carrying two top-level "info" keys is decoded by
	// merging the two dictionaries field by field, last one to name a field winning, while the bytes above are only
	// the last dictionary: the metadata in use would then match neither what was hashed nor anything a peer or tracker
	// could agree with, leaving a torrent that parses and then fails every handshake.
	var f File
	f.Announce = raw.Announce
	if err := bencode.DecodeBytes(raw.Info, &f.Info); err != nil {
		return nil, errs.Wrap(err)
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
	// Bounded here rather than left to the entry walk, since only the single-file form presents the name as a path
	// there; the multi-file form uses it for the storage path alone, which is where an unbounded one does its damage.
	if len(f.Info.Name) > MaxNameLength {
		return errs.Newf("torrent name length of %d exceeds the maximum of %d", len(f.Info.Name), MaxNameLength)
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
	// Recorded now that the metadata has been judged consistent, since nothing changes it afterwards and Size is asked
	// for it on paths a remote peer drives.
	f.size = size
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
	// The total is worked out once, while the metadata is being validated, and the metadata is immutable from then on.
	// Summing the file list on every call costs O(file count) — MaxFileCount is 131,072 entries — and LengthOf calls
	// this for the last piece, which the peer code reaches for on every 17 byte request message that arrives: a remote
	// asking for the final piece over and over would otherwise buy itself ~10^5 iterations of work per tiny message.
	if f.size > 0 {
		return f.size
	}
	return f.computeSize()
}

// computeSize sums the lengths the metadata declares, which is what a File that this package didn't build — and so
// never validated, and never worked the total out for — has to fall back on.
func (f *File) computeSize() int64 {
	if f.Info.Length > 0 {
		return f.Info.Length
	}
	var total int64
	for _, one := range f.Info.Files {
		total += one.Length
	}
	return total
}

// StoragePath returns the path that will be used for torrent file storage. The torrent's name is only there to make
// the file recognizable; what makes it the storage of one particular torrent, and of no other, is the info hash
// appended to it. A name alone maps distinct torrents onto the same storage file whenever those names agree once the
// extension has been trimmed away ("movie.mkv" and "movie.srt" both leave "movie"), or once they have been cut down to
// what a filesystem will accept, and two such torrents sharing a download directory then open, truncate and write that
// one file, destroying each other's data with nothing to say it happened. A torrent carries whatever name it likes, so
// that is something a hostile one arranges deliberately rather than a coincidence to be hoped against.
//
// The extension is trimmed with xfilepath.TrimExtension rather than filepath.Ext, since the whole of a dotfile-style
// name (".config") is an extension as far as the latter is concerned, which would leave nothing of the name at all.
// The hash is written in hex rather than in a denser encoding because the storage lands on filesystems that don't
// distinguish letter case, on which anything mixing the two would let distinct hashes collide all over again.
func (f *File) StoragePath() string {
	dir, filename := filepath.Split(f.Path)
	suffix := "-" + hex.EncodeToString(f.InfoHash[:]) + DownloadExt
	filename = truncateName(xfilepath.TrimExtension(filename), maxStorageNameLength-len(suffix))
	return filepath.Join(dir, filename+suffix)
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

// Validate checks the supplied buffer to determine if it contains the data for the piece at the specified index. An
// index that isn't one of the torrent's pieces is answered with false rather than being allowed to run off the end of
// the piece hashes: this is exported on a type built from untrusted metadata, and its siblings LengthOf and OffsetOf
// answer for any index at all, so a caller that forgot to bound one taken from a peer's message would otherwise take
// the whole process down with a slice out of range.
func (f *File) Validate(index int, buffer []byte) bool {
	if index < 0 || index >= f.PieceCount() {
		return false
	}
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

// Stat implements the fs.StatFS interface. Without it, fs.Stat falls back to Open, which opens the on-disk storage
// file behind the node: a metadata query would then fail with "no such file or directory" for every torrent whose
// data hasn't been downloaded yet, while fs.ReadDir of the directory holding it answers the same question correctly
// out of the same tree, and would cost an open and a close of that file even once it has. Everything a stat reports
// comes from the metadata, which is fully known from the moment the torrent was parsed.
func (f *File) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: statOp, Path: name, Err: fs.ErrInvalid}
	}
	f.buildFS()
	file, ok := f.fs[name]
	if !ok {
		return nil, &fs.PathError{Op: statOp, Path: name, Err: fs.ErrNotExist}
	}
	return file, nil
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
	modTime := time.Now()
	f.root = &vfs{
		owner:   f,
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
			owner:   f,
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
			owner:   f,
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
// nothing usable remains, if the result would be deeper than MaxPathDepth or longer than MaxPathLength, since
// everything downstream of here does work per ancestor of a path and a torrent is free to claim any depth at all, or
// if any component is one that can't be handed out safely as part of a path (see usableComponent).
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
				// The bounds are applied before the component is examined, so that a hostile path is turned away by
				// the O(1) checks rather than by a scan of however many megabytes it spent on one component.
				if length += len(sub); length > MaxPathLength {
					return "", false
				}
				if !usableComponent(sub) {
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

// windowsReservedNames are the names Windows resolves to a device rather than to a file, in the upper case form the
// comparison is made against.
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// usableComponent reports whether one component of a torrent file entry's path can be handed out as part of a path of
// this filesystem's. What a consumer does with such a path is turn it back into a local one, with something on the
// order of filepath.Join(dir, filepath.FromSlash(p)), and what is refused here are the components that stop being one
// component of a path below dir when it does: those carrying a byte that separates or ends a path elsewhere, those
// naming a drive, and those Windows resolves to a device rather than to a file, which CreateFile answers with the
// device itself in any directory and whatever extension the name carries, before the filesystem is consulted at all.
// The torrent is refused rather than the name quietly rewritten, since the tree has to keep describing the torrent it
// came from; nothing legitimate is lost by it, as a torrent carrying such a name is one no Windows client can unpack
// in the first place.
func usableComponent(sub string) bool {
	if strings.ContainsAny(sub, componentDelimiters) {
		return false
	}
	// A leading drive designation makes the path drive-relative on Windows — "C:" and "C:dir" alike — so what it names
	// no longer hangs from the directory it was joined onto.
	if len(sub) > 1 && sub[1] == ':' {
		if c := sub[0] | 0x20; c >= 'a' && c <= 'z' {
			return false
		}
	}
	// Windows resolves the device from what precedes the first '.', ignoring trailing spaces, so "COM1.txt" and "CON "
	// name devices just as bare "CON" does.
	device := sub
	if i := strings.IndexByte(device, '.'); i != -1 {
		device = device[:i]
	}
	device = strings.TrimRight(device, " ")
	// Measured before the case fold, which would otherwise copy however much of the component precedes its first dot,
	// and that is as much as the torrent cared to spend on it.
	if len(device) > maxReservedNameLength {
		return true
	}
	return !windowsReservedNames[strings.ToUpper(device)]
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
