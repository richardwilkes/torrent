// Copyright (c) 2017-2025 by Richard A. Wilkes. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, version 2.0. If a copy of the MPL was not distributed with
// this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This Source Code Form is "Incompatible With Secondary Licenses", as
// defined by the Mozilla Public License, version 2.0.

package tfs_test

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // The spec requires sha1
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/zeebo/bencode"
)

const (
	sha1Size = 20

	// maxStorageNameLength mirrors the limit tfs applies to a single path element of the storage path.
	maxStorageNameLength = 255

	lengthKey = "length"
	pathKey   = "path"
	subDir    = "sub"
	fileA     = "a.txt"
	fileB     = "b.txt"
	coverName = "cover.jpg"
)

// readdirFile is the legacy, os.File-style directory reading interface that an opened directory also provides.
type readdirFile interface {
	Readdir(count int) ([]os.FileInfo, error)
}

// hashes returns a placeholder block of piece hashes for the given piece count.
func hashes(count int) []byte {
	return make([]byte, count*sha1Size)
}

// encodeTorrent bencodes a torrent whose info dictionary is the one supplied.
func encodeTorrent(t *testing.T, info map[string]any) []byte {
	t.Helper()
	data, err := bencode.EncodeBytes(map[string]any{
		"announce": "http://example.com/announce",
		"info":     info,
	})
	check.New(t).NoError(err)
	return data
}

// singleFileInfo returns a valid single-file info dictionary of 20 bytes in two pieces.
func singleFileInfo() map[string]any {
	return map[string]any{
		"name":         "example.bin",
		"piece length": int64(16),
		"pieces":       hashes(2),
		lengthKey:      int64(20),
	}
}

// multiFileInfo returns a valid multi-file info dictionary of 20 bytes in two pieces.
func multiFileInfo() map[string]any {
	return map[string]any{
		"name":         "example",
		"piece length": int64(16),
		"pieces":       hashes(2),
		"files": []any{
			map[string]any{lengthKey: int64(12), pathKey: []any{subDir, fileA}},
			map[string]any{lengthKey: int64(8), pathKey: []any{fileB}},
		},
	}
}

func TestNewFileFromBytesRejectsInconsistentMetadata(t *testing.T) {
	for _, one := range []struct {
		alter func(info map[string]any)
		name  string
	}{
		{name: "empty name", alter: func(info map[string]any) { info["name"] = "" }},
		{name: "zero piece length", alter: func(info map[string]any) { info["piece length"] = int64(0) }},
		{name: "negative piece length", alter: func(info map[string]any) { info["piece length"] = int64(-16) }},
		{name: "no pieces", alter: func(info map[string]any) { info["pieces"] = []byte{} }},
		{name: "partial piece hash", alter: func(info map[string]any) { info["pieces"] = make([]byte, sha1Size+7) }},
		{name: "too few pieces", alter: func(info map[string]any) { info["pieces"] = hashes(1) }},
		{name: "too many pieces", alter: func(info map[string]any) { info["pieces"] = hashes(3) }},
		{name: "negative length", alter: func(info map[string]any) { info[lengthKey] = int64(-20) }},
		{name: "zero length", alter: func(info map[string]any) { info[lengthKey] = int64(0) }},
		{
			name: "both length and files",
			alter: func(info map[string]any) {
				info["files"] = []any{map[string]any{lengthKey: int64(20), pathKey: []any{fileA}}}
			},
		},
		{
			name: "negative file length",
			alter: func(info map[string]any) {
				info[lengthKey] = nil
				delete(info, lengthKey)
				info["files"] = []any{
					map[string]any{lengthKey: int64(-12), pathKey: []any{fileA}},
					map[string]any{lengthKey: int64(32), pathKey: []any{fileB}},
				}
			},
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			info := singleFileInfo()
			one.alter(info)
			_, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
			check.New(t).HasError(err)
		})
	}
}

func TestNewFileFromBytesRejectsBadFileLists(t *testing.T) {
	for _, one := range []struct {
		name  string
		files []any
	}{
		{
			name: "duplicate paths",
			files: []any{
				map[string]any{lengthKey: int64(12), pathKey: []any{subDir, fileA}},
				map[string]any{lengthKey: int64(8), pathKey: []any{subDir, fileA}},
			},
		},
		{
			name: "path that cleans away to nothing",
			files: []any{
				map[string]any{lengthKey: int64(12), pathKey: []any{}},
				map[string]any{lengthKey: int64(8), pathKey: []any{fileB}},
			},
		},
		{
			name: "path made only of dot components",
			files: []any{
				map[string]any{lengthKey: int64(12), pathKey: []any{"..", "."}},
				map[string]any{lengthKey: int64(8), pathKey: []any{fileB}},
			},
		},
		{
			name: "path colliding with the root after cleaning",
			files: []any{
				map[string]any{lengthKey: int64(12), pathKey: []any{"/"}},
				map[string]any{lengthKey: int64(8), pathKey: []any{fileB}},
			},
		},
		{
			name: "file that a later entry uses as a directory",
			files: []any{
				map[string]any{lengthKey: int64(12), pathKey: []any{subDir}},
				map[string]any{lengthKey: int64(8), pathKey: []any{subDir, fileB}},
			},
		},
		{
			name: "directory that a later entry uses as a file",
			files: []any{
				map[string]any{lengthKey: int64(12), pathKey: []any{subDir, fileB}},
				map[string]any{lengthKey: int64(8), pathKey: []any{subDir}},
			},
		},
		{
			name: "file that a later entry uses as an ancestor directory",
			files: []any{
				map[string]any{lengthKey: int64(12), pathKey: []any{subDir}},
				map[string]any{lengthKey: int64(8), pathKey: []any{subDir, "deeper", fileB}},
			},
		},
	} {
		t.Run(one.name, func(t *testing.T) {
			info := multiFileInfo()
			info["files"] = one.files
			_, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
			check.New(t).HasError(err)
		})
	}
}

func TestNewFileFromBytesAcceptsValidMetadata(t *testing.T) {
	c := check.New(t)

	f, err := tfs.NewFileFromBytes(encodeTorrent(t, singleFileInfo()))
	c.NoError(err)
	c.Equal(2, f.PieceCount())
	c.Equal(int64(20), f.Size())
	c.Equal(int64(16), f.LengthOf(0))
	c.Equal(int64(4), f.LengthOf(1))

	f, err = tfs.NewFileFromBytes(encodeTorrent(t, multiFileInfo()))
	c.NoError(err)
	c.Equal(2, f.PieceCount())
	c.Equal(int64(20), f.Size())
	c.Equal(int64(4), f.LengthOf(1))

	// Entries that merely share a directory are not a file/directory collision.
	info := multiFileInfo()
	info["files"] = []any{
		map[string]any{lengthKey: int64(12), pathKey: []any{subDir, fileA}},
		map[string]any{lengthKey: int64(8), pathKey: []any{subDir, fileB}},
	}
	f, err = tfs.NewFileFromBytes(encodeTorrent(t, info))
	c.NoError(err)
	c.Equal(int64(20), f.Size())
}

// TestNewFileFromBytesRejectsAnOversizedPieceLength guards the allocation sites that do
// make([]byte, Info.PieceLength): a self-consistent torrent declaring an absurd piece length would otherwise drive
// those to a terabyte-scale allocation the moment verification or a piece download starts.
func TestNewFileFromBytesRejectsAnOversizedPieceLength(t *testing.T) {
	for _, one := range []struct {
		name        string
		pieceLength int64
	}{
		{name: "one byte too large", pieceLength: tfs.MaxPieceLength + 1},
		{name: "absurdly large", pieceLength: int64(1) << 40},
		{name: "larger than math.MaxInt32", pieceLength: int64(math.MaxInt32) + 1},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			info := singleFileInfo()
			info["piece length"] = one.pieceLength
			// The piece count is made consistent with the declared piece length, so only the bound itself can
			// reject this.
			info["pieces"] = hashes(1)
			_, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
			c.HasError(err)
		})
	}

	// The largest permitted piece length is still accepted.
	c := check.New(t)
	info := singleFileInfo()
	info["piece length"] = int64(tfs.MaxPieceLength)
	info["pieces"] = hashes(1)
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
	c.NoError(err)
	c.Equal(int64(tfs.MaxPieceLength), f.Info.PieceLength)
	c.Equal(int64(20), f.LengthOf(0))
}

// bstr returns the bencoded form of a string.
func bstr(s string) string {
	return strconv.Itoa(len(s)) + ":" + s
}

// torrentWithUnsortedInfoKeys returns a bencoded torrent whose info dictionary carries its keys in an order other
// than the sorted one, along with the exact bytes of that dictionary. Decoders accept it as-is, but re-encoding the
// decoded form sorts the keys, changing the bytes an info hash would be taken over.
func torrentWithUnsortedInfoKeys() (torrent, info []byte) {
	var infoBuf bytes.Buffer
	infoBuf.WriteString("d")
	infoBuf.WriteString(bstr(lengthKey) + "i20e")
	infoBuf.WriteString(bstr("piece length") + "i16e")
	infoBuf.WriteString(bstr("pieces") + strconv.Itoa(2*sha1Size) + ":")
	infoBuf.Write(hashes(2))
	infoBuf.WriteString(bstr("name") + bstr("example.bin"))
	infoBuf.WriteString("e")

	var buf bytes.Buffer
	buf.WriteString("d")
	buf.WriteString(bstr("announce") + bstr("http://example.com/announce"))
	buf.WriteString(bstr("info"))
	buf.Write(infoBuf.Bytes())
	buf.WriteString("e")
	return buf.Bytes(), infoBuf.Bytes()
}

// TestInfoHashComesFromTheRawInfoDictionary verifies the hash is taken over the info dictionary exactly as it
// appeared in the file. A hash taken over a re-encoding would differ for any torrent that wasn't already canonically
// encoded, and every announce and handshake for it would fail.
func TestInfoHashComesFromTheRawInfoDictionary(t *testing.T) {
	c := check.New(t)
	data, info := torrentWithUnsortedInfoKeys()
	f, err := tfs.NewFileFromBytes(data)
	c.NoError(err)
	c.Equal(tfs.InfoHash(sha1.Sum(info)), f.InfoHash) //nolint:gosec // The spec requires sha1

	// The round trip through the decoder sorts the keys, so hashing that form would have yielded a different hash.
	var m map[string]any
	c.NoError(bencode.DecodeBytes(data, &m))
	reencoded, err := bencode.EncodeBytes(m["info"])
	c.NoError(err)
	c.NotEqual(info, reencoded)
	c.NotEqual(tfs.InfoHash(sha1.Sum(reencoded)), f.InfoHash) //nolint:gosec // The spec requires sha1

	// A canonically encoded torrent hashes to the same value either way.
	data = encodeTorrent(t, singleFileInfo())
	f, err = tfs.NewFileFromBytes(data)
	c.NoError(err)
	c.NoError(bencode.DecodeBytes(data, &m))
	reencoded, err = bencode.EncodeBytes(m["info"])
	c.NoError(err)
	c.Equal(tfs.InfoHash(sha1.Sum(reencoded)), f.InfoHash) //nolint:gosec // The spec requires sha1
}

// TestStoragePathStaysAValidFilename covers names long enough to require truncation: the cut may not split a
// multi-byte rune or one of SanitizeName's two-character escapes, since the result has to be a name the filesystem
// will actually create.
func TestStoragePathStaysAValidFilename(t *testing.T) {
	for _, one := range []struct {
		label string
		name  string
	}{
		{label: "multi-byte runes", name: strings.Repeat("é", 200)},
		{label: "escaped characters", name: strings.Repeat("@", 200)},
		{label: "both", name: strings.Repeat("日@", 100)},
		{label: "ascii", name: strings.Repeat("a", 400)},
	} {
		t.Run(one.label, func(t *testing.T) {
			c := check.New(t)
			info := singleFileInfo()
			info["name"] = one.name
			f, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
			c.NoError(err)
			f.Path = filepath.Join(t.TempDir(), f.Path)

			base := filepath.Base(f.StoragePath())
			c.True(len(base) <= maxStorageNameLength, "%d bytes exceeds the limit", len(base))
			c.True(utf8.ValidString(base), "storage name must be valid UTF-8")
			c.True(strings.HasSuffix(base, tfs.DownloadExt))
			// A trailing '@' would be the orphaned first half of an escape pair.
			c.False(strings.HasSuffix(strings.TrimSuffix(base, tfs.DownloadExt), "@"))

			// The real test of all of the above: the filesystem accepts the name.
			file, err := os.OpenFile(f.StoragePath(), os.O_CREATE|os.O_RDWR, 0o600)
			c.NoError(err)
			c.NoError(file.Close())
		})
	}

	// Names short enough to fit are left alone.
	c := check.New(t)
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, singleFileInfo()))
	c.NoError(err)
	c.Equal("example"+tfs.DownloadExt, f.StoragePath())
}

// TestEmbeddedFilesOrderIsStable verifies the order doesn't shift from call to call. The input order comes from map
// iteration, so files whose base names compare equal need the full path as a tie-breaker to stay put.
func TestEmbeddedFilesOrderIsStable(t *testing.T) {
	c := check.New(t)
	info := multiFileInfo()
	info["files"] = []any{
		map[string]any{lengthKey: int64(5), pathKey: []any{"y", coverName}},
		map[string]any{lengthKey: int64(7), pathKey: []any{"x", coverName}},
		map[string]any{lengthKey: int64(8), pathKey: []any{fileB}},
	}
	data := encodeTorrent(t, info)

	// Only base names are exposed, so the sizes stand in as the identity of the two cover images: x's comes first,
	// since the full path breaks the tie between their equal names.
	expected := []string{fileB + ":8", coverName + ":7", coverName + ":5"}
	for i := range 25 {
		f, err := tfs.NewFileFromBytes(data)
		c.NoError(err)
		for range 2 {
			actual := make([]string, 0, len(expected))
			for _, one := range f.EmbeddedFiles() {
				actual = append(actual, one.Name()+":"+strconv.FormatInt(one.Size(), 10))
			}
			c.Equal(expected, actual, "iteration %d", i)
		}
	}
}

// TestOpenReportsMissingStorageAsAPathError checks the fs.FS contract for the one failure that isn't decided by the
// virtual tree: the backing storage file not being openable.
func TestOpenReportsMissingStorageAsAPathError(t *testing.T) {
	c := check.New(t)
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, multiFileInfo()))
	c.NoError(err)
	f.Path = filepath.Join(t.TempDir(), f.Path) // The storage file is never created.

	_, err = f.Open(fileB)
	c.HasError(err)
	var pathErr *fs.PathError
	c.True(errors.As(err, &pathErr), "must yield an *fs.PathError")
	c.Equal("open", pathErr.Op)
	// The path must name the virtual file, not the on-disk storage file behind it.
	c.Equal(fileB, pathErr.Path)
	c.True(errors.Is(err, fs.ErrNotExist))
	c.NotContains(err.Error(), tfs.DownloadExt)
}

// TestLengthOfStaysPositive guards the allocation sites that do make([]byte, LengthOf(i)): validation must make it
// impossible for a decoded torrent to yield a negative or oversized final piece length.
func TestLengthOfStaysPositive(t *testing.T) {
	c := check.New(t)
	info := singleFileInfo()
	// A piece count far beyond what the size supports used to make the last piece length wildly negative.
	info["pieces"] = hashes(1000)
	_, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
	c.HasError(err)

	f, err := tfs.NewFileFromBytes(encodeTorrent(t, multiFileInfo()))
	c.NoError(err)
	for i := range f.PieceCount() {
		c.True(f.LengthOf(i) > 0, "piece %d", i)
		c.True(f.LengthOf(i) <= f.Info.PieceLength, "piece %d", i)
	}
}

// newPopulatedFile returns a torrent File whose backing storage exists on disk and holds the supplied content.
func newPopulatedFile(t *testing.T, info map[string]any, content []byte) *tfs.File {
	t.Helper()
	c := check.New(t)
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
	c.NoError(err)
	dir := t.TempDir()
	f.Path = filepath.Join(dir, f.Path)
	c.NoError(os.WriteFile(f.StoragePath(), content, 0o600))
	return f
}

func TestFileSatisfiesTestFS(t *testing.T) {
	c := check.New(t)
	content := []byte("0123456789ab" + "cdefghij")

	f := newPopulatedFile(t, multiFileInfo(), content)
	c.NoError(fstest.TestFS(f, subDir, subDir+"/"+fileA, fileB))

	f = newPopulatedFile(t, singleFileInfo(), content)
	c.NoError(fstest.TestFS(f, "example.bin"))
}

func TestOpenObeysTheFSContract(t *testing.T) {
	c := check.New(t)
	f := newPopulatedFile(t, multiFileInfo(), []byte("0123456789abcdefghij"))

	for _, name := range []string{"", "/", "/b.txt", "./b.txt", "b.txt/", "sub/../b.txt", "../b.txt"} {
		_, err := f.Open(name)
		c.HasError(err, "open %q", name)
		var pathErr *fs.PathError
		c.True(errors.As(err, &pathErr), "open %q should yield an *fs.PathError", name)
		c.True(errors.Is(err, fs.ErrInvalid), "open %q should report fs.ErrInvalid", name)
		c.Equal(name, pathErr.Path)
	}

	_, err := f.Open("nope.txt")
	var pathErr *fs.PathError
	c.True(errors.As(err, &pathErr))
	c.True(errors.Is(err, fs.ErrNotExist))
	c.Equal("open", pathErr.Op)

	// The root is reachable as "." on every platform, and the same content is reachable by its virtual path.
	root, err := f.Open(".")
	c.NoError(err)
	c.NoError(root.Close())

	data, err := fs.ReadFile(f, subDir+"/"+fileA)
	c.NoError(err)
	c.Equal("0123456789ab", string(data))

	data, err = fs.ReadFile(f, fileB)
	c.NoError(err)
	c.Equal("cdefghij", string(data))
}

func TestWalkDirTraversesTheWholeTree(t *testing.T) {
	c := check.New(t)
	f := newPopulatedFile(t, multiFileInfo(), []byte("0123456789abcdefghij"))
	var paths []string
	c.NoError(fs.WalkDir(f, ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, p)
		return nil
	}))
	slices.Sort(paths)
	c.Equal([]string{".", fileB, subDir, subDir + "/" + fileA}, paths)
}

func TestStatAndDirEntriesReportBaseNames(t *testing.T) {
	c := check.New(t)
	f := newPopulatedFile(t, multiFileInfo(), []byte("0123456789abcdefghij"))

	fi, err := fs.Stat(f, subDir+"/"+fileA)
	c.NoError(err)
	c.Equal(fileA, fi.Name())
	c.Equal(int64(12), fi.Size())
	c.False(fi.IsDir())

	fi, err = fs.Stat(f, subDir)
	c.NoError(err)
	c.Equal(subDir, fi.Name())
	c.True(fi.IsDir())

	fi, err = fs.Stat(f, ".")
	c.NoError(err)
	c.Equal(".", fi.Name())
	c.True(fi.IsDir())

	entries, err := fs.ReadDir(f, subDir)
	c.NoError(err)
	c.Equal(1, len(entries))
	c.Equal(fileA, entries[0].Name())
	c.Equal(fs.FileMode(0), entries[0].Type())
	info, err := entries[0].Info()
	c.NoError(err)
	c.Equal(int64(12), info.Size())

	names := make([]string, 0, len(f.EmbeddedFiles()))
	for _, one := range f.EmbeddedFiles() {
		names = append(names, one.Name())
	}
	c.Equal([]string{fileA, fileB}, names)
}

// TestSingleFileNameWithSeparators covers a single-file torrent whose name carries path separators or dot
// components: the entry must still be reachable through Open, with the implied directories present.
func TestSingleFileNameWithSeparators(t *testing.T) {
	c := check.New(t)

	info := singleFileInfo()
	info["name"] = "sub/dir/thing.bin"
	f := newPopulatedFile(t, info, []byte("0123456789abcdefghij"))
	data, err := fs.ReadFile(f, "sub/dir/thing.bin")
	c.NoError(err)
	c.Equal(20, len(data))
	fi, err := fs.Stat(f, subDir)
	c.NoError(err)
	c.True(fi.IsDir())

	info = singleFileInfo()
	info["name"] = "../../etc/passwd"
	f = newPopulatedFile(t, info, []byte("0123456789abcdefghij"))
	data, err = fs.ReadFile(f, "etc/passwd")
	c.NoError(err)
	c.Equal(20, len(data))

	info = singleFileInfo()
	info["name"] = ".."
	_, err = tfs.NewFileFromBytes(encodeTorrent(t, info))
	c.HasError(err)
}

func TestReaddirFollowsOSFileSemantics(t *testing.T) {
	c := check.New(t)
	info := multiFileInfo()
	info["files"] = []any{
		map[string]any{lengthKey: int64(5), pathKey: []any{fileA}},
		map[string]any{lengthKey: int64(5), pathKey: []any{fileB}},
		map[string]any{lengthKey: int64(5), pathKey: []any{"c.txt"}},
		map[string]any{lengthKey: int64(5), pathKey: []any{"d.txt"}},
	}
	f := newPopulatedFile(t, info, []byte("0123456789abcdefghij"))

	// With a positive count, entries come back in chunks and io.EOF arrives once they run out.
	d, err := f.Open(".")
	c.NoError(err)
	rd, ok := d.(readdirFile)
	c.True(ok)
	list, err := rd.Readdir(3)
	c.NoError(err)
	c.Equal(3, len(list))
	list, err = rd.Readdir(3)
	c.NoError(err)
	c.Equal(1, len(list))
	list, err = rd.Readdir(3)
	c.True(errors.Is(err, io.EOF))
	c.Equal(0, len(list))
	c.NoError(d.Close())

	// With a non-positive count, everything remaining comes back at once and later calls report no error.
	d, err = f.Open(".")
	c.NoError(err)
	rd, ok = d.(readdirFile)
	c.True(ok)
	list, err = rd.Readdir(-1)
	c.NoError(err)
	c.Equal(4, len(list))
	list, err = rd.Readdir(-1)
	c.NoError(err)
	c.Equal(0, len(list))
	c.NoError(d.Close())

	// A closed directory reports it, rather than pretending to be at EOF.
	_, err = rd.Readdir(1)
	c.True(errors.Is(err, os.ErrClosed))
	rdf, ok := d.(fs.ReadDirFile)
	c.True(ok)
	_, err = rdf.ReadDir(1)
	c.True(errors.Is(err, os.ErrClosed))
}

func TestReadDirFollowsTheFSContract(t *testing.T) {
	c := check.New(t)
	f := newPopulatedFile(t, multiFileInfo(), []byte("0123456789abcdefghij"))

	d, err := f.Open(".")
	c.NoError(err)
	rdf, ok := d.(fs.ReadDirFile)
	c.True(ok)
	entries, err := rdf.ReadDir(1)
	c.NoError(err)
	c.Equal(1, len(entries))
	entries, err = rdf.ReadDir(1)
	c.NoError(err)
	c.Equal(1, len(entries))
	entries, err = rdf.ReadDir(1)
	c.True(errors.Is(err, io.EOF))
	c.Equal(0, len(entries))
	c.NoError(d.Close())

	d, err = f.Open(".")
	c.NoError(err)
	rdf, ok = d.(fs.ReadDirFile)
	c.True(ok)
	entries, err = rdf.ReadDir(-1)
	c.NoError(err)
	c.Equal(2, len(entries))
	entries, err = rdf.ReadDir(-1)
	c.NoError(err)
	c.Equal(0, len(entries))
	c.NoError(d.Close())
}
