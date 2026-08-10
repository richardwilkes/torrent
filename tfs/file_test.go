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
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/zeebo/bencode"
)

const (
	sha1Size = 20

	lengthKey = "length"
	pathKey   = "path"
	subDir    = "sub"
	fileA     = "a.txt"
	fileB     = "b.txt"
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
