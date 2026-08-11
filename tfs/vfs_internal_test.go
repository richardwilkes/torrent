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
	"errors"
	"io"
	"testing"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/zeebo/bencode"
)

// decodeUnvalidated decodes an info dictionary straight into a File, bypassing the checks NewFileFromBytes applies,
// so that buildFS's own defenses against a corrupt tree can be exercised.
func decodeUnvalidated(t *testing.T, files []any) *File {
	t.Helper()
	c := check.New(t)
	data, err := bencode.EncodeBytes(map[string]any{
		"info": map[string]any{
			"name":         exampleName,
			"piece length": int64(16),
			"pieces":       make([]byte, 20),
			"files":        files,
		},
	})
	c.NoError(err)
	var f File
	c.NoError(bencode.DecodeBytes(data, &f))
	f.Path = exampleName
	return &f
}

// entry builds a bencode-shaped file entry for the info dictionary's file list.
func entry(length int64, parts ...string) map[string]any {
	list := make([]any, 0, len(parts))
	for _, one := range parts {
		list = append(list, one)
	}
	return map[string]any{"length": length, "path": list}
}

func TestBuildFSDropsEntriesThatCleanAwayToTheRoot(t *testing.T) {
	c := check.New(t)
	f := decodeUnvalidated(t, []any{entry(4), entry(6, "b.txt")})
	f.buildFS()

	// The root must still be the directory it started as, rather than a regular file.
	c.True(f.fs["."].IsDir())
	c.Equal(f.root, f.fs["."])
	c.Equal(2, len(f.fs))
	c.Equal(1, len(f.root.children))
	c.Equal("b.txt", f.root.children[0].Name())

	// The offset still accounts for the dropped entry, since the data layout is fixed by the torrent.
	c.Equal(int64(4), f.root.children[0].offset)
	c.Equal(int64(6), f.root.children[0].length)
}

func TestBuildFSDropsDuplicatePaths(t *testing.T) {
	c := check.New(t)
	f := decodeUnvalidated(t, []any{entry(4, "sub", "a.txt"), entry(6, "sub", "a.txt")})
	f.buildFS()

	c.Equal(1, len(f.root.children))
	c.Equal(1, len(f.fs["sub"].children))
	c.Equal(int64(0), f.fs["sub/a.txt"].offset)
	c.Equal(int64(4), f.fs["sub/a.txt"].length)
}

func TestBuildFSWontTreatAFileAsADirectory(t *testing.T) {
	c := check.New(t)
	f := decodeUnvalidated(t, []any{entry(4, "a"), entry(6, "a", "b")})
	f.buildFS()

	c.False(f.fs["a"].IsDir())
	c.Equal(0, len(f.fs["a"].children))
	_, exists := f.fs["a/b"]
	c.False(exists)
	c.Equal(1, len(f.root.children))
}

func TestBuildFSWontReplaceADirectoryWithAFile(t *testing.T) {
	c := check.New(t)
	f := decodeUnvalidated(t, []any{entry(4, "a", "b"), entry(6, "a")})
	f.buildFS()

	c.True(f.fs["a"].IsDir())
	c.Equal(1, len(f.fs["a"].children))
	c.Equal("b", f.fs["a"].children[0].Name())
	c.Equal(int64(4), f.fs["a/b"].length)
}

func TestReaddirOnAnEmptyDirectory(t *testing.T) {
	c := check.New(t)
	f := decodeUnvalidated(t, []any{entry(4)})
	f.buildFS()
	c.Equal(0, len(f.root.children))

	// With a positive count, an empty directory reports io.EOF on the very first call, per the os.File contract.
	d, err := f.root.open()
	c.NoError(err)
	dir, ok := d.(*vdir)
	c.True(ok)
	list, err := dir.Readdir(1)
	c.True(errors.Is(err, io.EOF))
	c.Equal(0, len(list))
	entries, err := dir.ReadDir(1)
	c.True(errors.Is(err, io.EOF))
	c.Equal(0, len(entries))

	// With a non-positive count, it reports an empty slice and no error, no matter how often it is called.
	list, err = dir.Readdir(-1)
	c.NoError(err)
	c.Equal(0, len(list))
	entries, err = dir.ReadDir(0)
	c.NoError(err)
	c.Equal(0, len(entries))
	c.NoError(d.Close())
}

func TestCleanVirtualPath(t *testing.T) {
	c := check.New(t)
	const (
		fileB = "b.txt"
		abTxt = "a/" + fileB
	)
	for _, one := range []struct {
		name     string
		expected string
		parts    []string
		ok       bool
	}{
		{name: "simple", parts: []string{"a", fileB}, expected: abTxt, ok: true},
		{name: "embedded separators", parts: []string{"a/b", "c.txt"}, expected: "a/b/c.txt", ok: true},
		{name: "leading separator", parts: []string{"/a", fileB}, expected: abTxt, ok: true},
		{name: "dot components", parts: []string{".", "a", "..", fileB}, expected: abTxt, ok: true},
		{name: "empty components", parts: []string{"", "a", "", fileB}, expected: abTxt, ok: true},
		{name: "nothing", parts: nil},
		{name: "only empties", parts: []string{"", ""}},
		{name: "only dots", parts: []string{".", "..", "/"}},
	} {
		t.Run(one.name, func(_ *testing.T) {
			actual, ok := cleanVirtualPath(one.parts)
			c.Equal(one.ok, ok, one.name)
			c.Equal(one.expected, actual, one.name)
		})
	}
}
