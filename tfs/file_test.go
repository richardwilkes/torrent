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
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/richardwilkes/toolbox/v2/check"
	"github.com/richardwilkes/toolbox/v2/xfilepath"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/zeebo/bencode"
)

const (
	sha1Size = 20

	// allocationRuns is how many times allocatedBy runs the function it is measuring, taking the smallest measurement
	// of the set as the one least polluted by allocations made elsewhere in the process while it ran.
	allocationRuns = 3

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

// nestedLists returns a value made of depth lists nested one inside the next.
func nestedLists(depth int) any {
	var v any = "x"
	for range depth {
		v = []any{v}
	}
	return v
}

// torrentWithNesting bencodes a valid torrent carrying an extra key whose value nests to the given depth. The
// torrent's own dictionary is the first level, so the depth of the result is one more than what is asked for here.
func torrentWithNesting(t *testing.T, depth int) []byte {
	t.Helper()
	data, err := bencode.EncodeBytes(map[string]any{
		"announce": "http://example.com/announce",
		"info":     singleFileInfo(),
		"nested":   nestedLists(depth),
	})
	check.New(t).NoError(err)
	return data
}

// TestNewFileFromBytesRejectsDeeplyNestedData covers the depth of the bencoded structure itself. The decoder recurses
// once per level of nesting, so a small file of nothing but list heads used to run the goroutine stack out and take
// the whole process down with a stack overflow, which is a fatal runtime error that no recover() can catch. This test
// reaching its end at all is most of what it asserts.
func TestNewFileFromBytesRejectsDeeplyNestedData(t *testing.T) {
	c := check.New(t)

	// Nesting right up to the limit is still accepted.
	f, err := tfs.NewFileFromBytes(torrentWithNesting(t, tfs.MaxNestingDepth-1))
	c.NoError(err)
	c.Equal(int64(20), f.Size())

	// One level beyond it is not.
	_, err = tfs.NewFileFromBytes(torrentWithNesting(t, tfs.MaxNestingDepth))
	c.HasError(err)

	// Nesting deep enough to overflow the stack is rejected no matter what leads up to it: the depth is measured
	// before the decoder is handed the bytes, and the measurement can't be shaken off by a token in front of it.
	deep := strings.Repeat("l", 1000000)
	for _, one := range []struct {
		name string
		data string
	}{
		{name: "nothing but list heads", data: deep},
		{name: "dictionaries", data: strings.Repeat("d1:x", 1000000)},
		{name: "after a string", data: "l" + bstr("announce") + deep},
		{name: "after an integer", data: "li17e" + deep},
		{name: "inside the info dictionary", data: "d" + bstr("info") + "d" + bstr("name") + deep},
		{name: "mixed", data: strings.Repeat("dl", 500000)},
	} {
		t.Run(one.name, func(t *testing.T) {
			_, decodeErr := tfs.NewFileFromBytes([]byte(one.data))
			check.New(t).HasError(decodeErr)
		})
	}
}

// TestNewFileFromBytesRejectsOverlongStrings covers the length a bencoded string declares for itself. The decoder
// allocates that length before reading it, so a few dozen bytes claiming a gigabyte make it allocate a gigabyte and
// only then report that the file was truncated. MaxTorrentFileLength is no defense, since what sizes the allocation is
// the length claimed rather than the bytes that arrived.
func TestNewFileFromBytesRejectsOverlongStrings(t *testing.T) {
	const claimed = 1 << 30
	claim := strconv.Itoa(claimed)
	// Which of the two guards each case has to trip is named, since both of them answer with an error and the decoder
	// behind them answers a truncated file with one as well: without this, a case aimed at one guard passes just as
	// well when that guard has been deleted and something further along reports the same input.
	const beyondEverything = "at least"
	const beyondWhatRemains = "bytes remain"
	for _, one := range []struct {
		name    string
		data    string
		refusal string
	}{
		{name: "the reported case", data: "d4:infod4:name1500000000:xe", refusal: beyondEverything},
		{name: "as a value", data: "d8:announce" + claim + ":xe", refusal: beyondEverything},
		{name: "as a key", data: "d" + claim + ":xi0ee", refusal: beyondEverything},
		// Six declared bytes with five behind them, so the length is within the data as a whole and beyond what is
		// left of it, which is the only way to reach the second guard.
		{name: "one byte more than remains", data: "d4:name6:abcde", refusal: beyondWhatRemains},
		{name: "nested inside the info dictionary", data: "d4:infod6:pieces" + claim + ":xee", refusal: beyondEverything},
		{name: "no digits could bring it back", data: "d4:name99999999999999999999:xe", refusal: beyondEverything},
	} {
		t.Run(one.name, func(t *testing.T) {
			c := check.New(t)
			var f *tfs.File
			var err error
			allocated := allocatedBy(func() { f, err = tfs.NewFileFromBytes([]byte(one.data)) })
			c.Nil(f)
			c.HasError(err)
			if err != nil {
				c.Contains(err.Error(), one.refusal)
			}
			// The point of refusing it before the decoder sees it: the claim never becomes an allocation.
			c.True(allocated < 1<<20, "%d bytes were allocated for a %d byte input", allocated, len(one.data))
		})
	}

	// A string that exactly fills what remains is not over-long, and a valid torrent still parses.
	c := check.New(t)
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, singleFileInfo()))
	c.NoError(err)
	c.Equal(int64(20), f.Size())
}

// allocatedBy returns an upper bound on the number of bytes f allocates. TotalAlloc counts the whole process, so a
// single difference across a call also carries whatever the runtime's own goroutines allocated while it ran, and
// attributes all of it to f. That over-count can only make a bound on f's allocations fail, never pass wrongly, but it
// is still noise standing in for the thing being measured, so the smallest of several runs is taken: f allocates the
// same amount every time, while the noise alongside it does not.
func allocatedBy(f func()) uint64 {
	var smallest uint64
	for i := range allocationRuns {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		if allocated := after.TotalAlloc - before.TotalAlloc; i == 0 || allocated < smallest {
			smallest = allocated
		}
	}
	return smallest
}

// TestNewFileFromBytesRejectsTooManyFileEntries covers the number of file entries, the one dimension of the metadata
// that the bound on the file's size leaves open. Every entry is materialized several times over as it is validated and
// turned into the virtual tree, so a file of nothing but minimal entries costs many times its own size in memory.
func TestNewFileFromBytesRejectsTooManyFileEntries(t *testing.T) {
	c := check.New(t)

	// The largest permitted number of entries is still accepted, with every one of them accounted for.
	f, err := tfs.NewFileFromBytes(torrentWithFileCount(tfs.MaxFileCount))
	c.NoError(err)
	c.Equal(int64(tfs.MaxFileCount), f.Size())
	c.Equal(tfs.MaxFileCount, len(f.Info.Files))

	// One entry beyond it is refused rather than parsed.
	_, err = tfs.NewFileFromBytes(torrentWithFileCount(tfs.MaxFileCount + 1))
	c.HasError(err)
}

// torrentWithFileCount bencodes a torrent whose info dictionary carries the given number of one-byte file entries. The
// bytes are assembled here rather than routed through the encoder, which spends far longer building a map per entry
// than the parse under test takes. The piece length is one byte, so the piece count matches the number of entries.
func torrentWithFileCount(count int) []byte {
	var buf strings.Builder
	buf.WriteString("d" + bstr("announce") + bstr("http://example.com/announce") + bstr("info") + "d")
	buf.WriteString(bstr("files") + "l")
	for i := range count {
		buf.WriteString("d" + bstr(lengthKey) + "i1e" + bstr(pathKey) + "l" + bstr(strconv.Itoa(i)) + "ee")
	}
	buf.WriteString("e" + bstr("name") + bstr("example"))
	buf.WriteString(bstr("piece length") + "i1e")
	buf.WriteString(bstr("pieces") + strconv.Itoa(count*sha1Size) + ":")
	buf.Write(hashes(count))
	buf.WriteString("ee")
	return []byte(buf.String())
}

// endlessReader stands in for a reader with nothing bounding it, such as a network stream, reporting every byte asked
// of it as delivered.
type endlessReader struct {
	read int
}

func (r *endlessReader) Read(p []byte) (int, error) {
	r.read += len(p)
	return len(p), nil
}

// TestNewFileFromReaderStopsAtTheSizeLimit verifies the public entry point that takes a reader can't be driven to
// arbitrary memory consumption by a caller handing it an untrusted stream.
func TestNewFileFromReaderStopsAtTheSizeLimit(t *testing.T) {
	c := check.New(t)

	f, err := tfs.NewFileFromReader(bytes.NewReader(encodeTorrent(t, singleFileInfo())))
	c.NoError(err)
	c.Equal(int64(20), f.Size())

	r := &endlessReader{}
	_, err = tfs.NewFileFromReader(r)
	c.HasError(err)
	// One byte beyond the limit is read, which is what proves the file is over it rather than exactly at it.
	c.Equal(tfs.MaxTorrentFileLength+1, r.read)
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

// infoWithPath returns a valid two-file info dictionary whose first entry carries the supplied path.
func infoWithPath(parts ...string) map[string]any {
	list := make([]any, 0, len(parts))
	for _, one := range parts {
		list = append(list, one)
	}
	info := multiFileInfo()
	info["files"] = []any{
		map[string]any{lengthKey: int64(12), pathKey: list},
		map[string]any{lengthKey: int64(8), pathKey: []any{fileB}},
	}
	return info
}

// TestNewFileFromBytesBoundsFilePaths covers the depth and length of a file entry's path. Both validation and the
// building of the virtual tree do work for every ancestor of every entry, so an unbounded depth makes a single entry
// cost O(depth²): a 240KB torrent naming one 80,000-component path took 7.5 seconds to parse, and grew from there,
// while decoding the same bytes is linear.
func TestNewFileFromBytesBoundsFilePaths(t *testing.T) {
	c := check.New(t)

	// A path at the depth limit is accepted, and the whole of it is reachable in the virtual tree.
	deepest := make([]string, tfs.MaxPathDepth)
	for i := range deepest {
		deepest[i] = "d"
	}
	deepest[len(deepest)-1] = fileA
	f := newPopulatedFile(t, infoWithPath(deepest...), []byte("0123456789abcdefghij"))
	fi, err := fs.Stat(f, strings.Join(deepest, "/"))
	c.NoError(err)
	c.Equal(int64(12), fi.Size())

	// So is one at the length limit, separators included.
	longest := strings.Repeat("a", tfs.MaxPathLength-(len(fileA)+1))
	_, err = tfs.NewFileFromBytes(encodeTorrent(t, infoWithPath(longest, fileA)))
	c.NoError(err)

	// Beyond either limit, the torrent is rejected rather than parsed at leisure.
	for _, one := range []struct {
		name  string
		parts []string
	}{
		{name: "one component too deep", parts: append(slices.Clone(deepest), fileA)},
		{name: "one byte too long", parts: []string{longest + "a", fileA}},
		{name: "separators count toward the length", parts: []string{longest, "a", fileA}},
		{name: "components hidden inside one string", parts: []string{strings.Join(deepest, "/") + "/" + fileA}},
		{name: "the reported case", parts: []string{strings.Repeat("a/", 79999) + fileA}},
	} {
		t.Run(one.name, func(t *testing.T) {
			sub := check.New(t)
			_, pathErr := tfs.NewFileFromBytes(encodeTorrent(t, infoWithPath(one.parts...)))
			sub.HasError(pathErr)
			if pathErr != nil {
				// The rejected path came out of the torrent, so the message may not quote all of it back.
				message, _, _ := strings.Cut(pathErr.Error(), "\n")
				sub.True(len(message) < 256, "%d byte message", len(message))
			}
		})
	}
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

// TestMetadataComesFromTheHashedInfoDictionary verifies that what we hold as the torrent's metadata is the same
// dictionary the info hash was taken over. A file carrying two top-level "info" keys is decoded by merging them field
// by field, so any field the second leaves out is filled in from the first, while the hash covers only the second: the
// torrent would parse and then fail every handshake and announce, describing a torrent that exists nowhere else.
func TestMetadataComesFromTheHashedInfoDictionary(t *testing.T) {
	c := check.New(t)
	first := singleFileInfo()
	first["name"] = "decoy.bin"
	first["private"] = 1
	second := singleFileInfo()
	data, hashed := torrentWithTwoInfoDictionaries(t, first, second)

	f, err := tfs.NewFileFromBytes(data)
	c.NoError(err)
	c.Equal(tfs.InfoHash(sha1.Sum(hashed)), f.InfoHash) //nolint:gosec // The spec requires sha1

	// Every field is the hashed dictionary's, including the one only the discarded dictionary named, which a merge
	// would have carried over.
	c.Equal("example.bin", f.Info.Name)
	c.False(f.Info.Private)
}

// torrentWithTwoInfoDictionaries returns a bencoded torrent carrying two top-level "info" keys, along with the exact
// bytes of the second one, which is the dictionary a decoder keeps when asked for the key's raw bytes.
func torrentWithTwoInfoDictionaries(t *testing.T, first, second map[string]any) (torrent, hashed []byte) {
	t.Helper()
	c := check.New(t)
	firstBytes, err := bencode.EncodeBytes(first)
	c.NoError(err)
	hashed, err = bencode.EncodeBytes(second)
	c.NoError(err)
	var buf bytes.Buffer
	buf.WriteString("d")
	buf.WriteString(bstr("announce") + bstr("http://example.com/announce"))
	buf.WriteString(bstr("info"))
	buf.Write(firstBytes)
	buf.WriteString(bstr("info"))
	buf.Write(hashed)
	buf.WriteString("e")
	return buf.Bytes(), hashed
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
		// Every case above happens to have the cut land between two escapes rather than inside one: an odd byte in
		// front of them is what puts the second half of an escape at the truncation point, which is the only thing
		// that can leave an orphaned '@' behind.
		{label: "escape split by the cut", name: "a" + strings.Repeat("@", 200)},
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
			// The readable part is what truncation acts on, so the info hash and the extension that follow it have to
			// come off before it can be judged: with them left on, the name always ends in a hex digit and every
			// assertion about how it was cut is answered by the suffix rather than by the truncation.
			readable := strings.TrimSuffix(base, storageSuffix(f))
			c.NotEqual(base, readable, "the storage name must carry the info hash")
			// A trailing '@' would be the orphaned first half of an escape pair.
			c.False(strings.HasSuffix(readable, "@"), "the name was cut in the middle of an escape: %q", readable)
			// Which is the same thing said in terms of what the escapes are for: a name carrying half of one no longer
			// describes the name it was made from.
			c.Equal(readable, xfilepath.SanitizeName(xfilepath.UnsanitizeName(readable)),
				"the truncated name must still be one that unsanitizes to itself")

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
	c.Equal(storageName(f, "example"), f.StoragePath())
}

// storageName returns the base name the storage file of the given torrent is expected to have: the readable part the
// caller supplies, followed by the info hash that keeps two torrents from ever being handed the same storage file.
func storageName(f *tfs.File, readable string) string {
	return readable + storageSuffix(f)
}

// storageSuffix returns everything a storage name carries after its readable part, which is the info hash that keeps
// two torrents from ever being handed the same storage file, followed by the extension.
func storageSuffix(f *tfs.File) string {
	return "-" + hex.EncodeToString(f.InfoHash[:]) + tfs.DownloadExt
}

// TestStoragePathKeepsCollidingNamesDistinct covers the names that a storage path derived from the torrent's name
// alone maps onto the same file: names that agree once the extension has been trimmed away, and names that agree once
// they have been cut down to what a filesystem will accept. Two such torrents sharing a download directory would
// open, truncate and write the same file, destroying each other's data with nothing to say it happened, and since a
// torrent carries whatever name it likes that is something a hostile one arranges deliberately.
func TestStoragePathKeepsCollidingNamesDistinct(t *testing.T) {
	c := check.New(t)
	dir := t.TempDir()
	long := strings.Repeat("a", 300)
	owner := make(map[string]string)
	for _, name := range []string{
		"movie.mkv", "movie.srt", "movie", // the same once the extension has been trimmed away
		long + "-one.bin", long + "-two.bin", // the same once the name has been truncated
	} {
		info := singleFileInfo()
		info["name"] = name
		f, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
		c.NoError(err)
		f.Path = filepath.Join(dir, f.Path)
		storage := f.StoragePath()
		base := filepath.Base(storage)
		c.True(len(base) <= maxStorageNameLength, "%d bytes exceeds the limit for %q", len(base), name)
		if other, taken := owner[storage]; taken {
			t.Fatalf("%q and %q were both given the storage file %q", other, name, storage)
		}
		owner[storage] = name

		// The storage file has to be reachable by the same path every time, since that is what lets a download that
		// was interrupted carry on from what is already there rather than starting over
		again, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
		c.NoError(err)
		again.Path = filepath.Join(dir, again.Path)
		c.Equal(storage, again.StoragePath(), "the same torrent must always be handed the same storage file")
	}
	c.Equal(5, len(owner))
}

// TestStoragePathKeepsDotfileNamesDistinct covers names that filepath.Ext considers to be nothing but an extension.
// Trimming that from ".config" leaves an empty name, so every torrent whose name starts with a dot and carries no
// other extension would be handed the same storage file and write its data over whatever the last one put there.
func TestStoragePathKeepsDotfileNamesDistinct(t *testing.T) {
	c := check.New(t)
	dir := t.TempDir()
	owner := make(map[string]string)
	files := make(map[string]*tfs.File)
	for _, name := range []string{".config", ".env", ".hidden.bin", "example.bin"} {
		info := singleFileInfo()
		info["name"] = name
		f, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
		c.NoError(err)
		f.Path = filepath.Join(dir, f.Path)
		files[name] = f
		storage := f.StoragePath()
		c.True(strings.HasSuffix(storage, tfs.DownloadExt))
		if other, taken := owner[storage]; taken {
			t.Fatalf("%q and %q were both given the storage file %q", other, name, storage)
		}
		owner[storage] = name
	}

	// The leading dot is part of the name rather than an extension, so it stays; a real extension behind one is still
	// trimmed.
	c.Equal(".config", owner[filepath.Join(dir, storageName(files[".config"], ".config"))])
	c.Equal(".env", owner[filepath.Join(dir, storageName(files[".env"], ".env"))])
	c.Equal(".hidden.bin", owner[filepath.Join(dir, storageName(files[".hidden.bin"], ".hidden"))])
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
	// Fatal rather than recorded and stepped over: everything below reaches through the pointer errors.As fills in,
	// and a nil one would panic and take the whole test binary with it instead of failing this one test.
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("must yield an *fs.PathError, got %T", err)
	}
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

// TestSizeIsWorkedOutWhileTheMetadataIsValidated verifies that the total isn't summed out of the file list on every
// call. LengthOf asks for it for the last piece, and the peer code asks LengthOf for every 17 byte request message
// that arrives, so a remote asking for the final piece over and over would otherwise buy itself an iteration per file
// entry — up to MaxFileCount of them — for each tiny message it sent.
func TestSizeIsWorkedOutWhileTheMetadataIsValidated(t *testing.T) {
	c := check.New(t)
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, multiFileInfo()))
	c.NoError(err)
	c.Equal(int64(20), f.Size())
	c.Equal(int64(4), f.LengthOf(f.PieceCount()-1))

	// Summing the file list again is the only way to arrive at what it says now, so a size that comes back unchanged
	// is one that was worked out while the metadata was being validated and hasn't been recomputed since
	f.Info.Files = append(f.Info.Files, f.Info.Files[0])
	c.Equal(int64(20), f.Size(), "the size must not be summed out of the file list on each call")
	c.Equal(int64(4), f.LengthOf(f.PieceCount()-1))

	// The single-file form has no list to sum, but has to agree with what it declared all the same
	f, err = tfs.NewFileFromBytes(encodeTorrent(t, singleFileInfo()))
	c.NoError(err)
	c.Equal(int64(20), f.Size())
}

// TestValidateRefusesIndexesOutsideTheTorrent verifies that an index that isn't one of the torrent's pieces is
// answered rather than run off the end of the piece hashes. Validate is exported on a type built from untrusted
// metadata, and its siblings LengthOf and OffsetOf answer for any index at all, so a caller that forgot to bound one
// taken from a peer's message would otherwise take the whole process down with a slice out of range.
func TestValidateRefusesIndexesOutsideTheTorrent(t *testing.T) {
	c := check.New(t)
	content := []byte("0123456789abcdefghij")
	first := sha1.Sum(content[:16])  //nolint:gosec // The spec requires sha1
	second := sha1.Sum(content[16:]) //nolint:gosec // The spec requires sha1
	info := singleFileInfo()
	info["pieces"] = append(first[:], second[:]...)
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
	c.NoError(err)

	// The pieces that are there are still judged on what they hold
	c.True(f.Validate(0, content[:16]))
	c.True(f.Validate(1, content[16:]))
	c.False(f.Validate(0, content[16:]), "a buffer that doesn't hash to the piece must not be accepted")

	for _, index := range []int{-1, 2, 3, math.MaxInt32, math.MinInt32} {
		c.NotPanics(func() {
			c.False(f.Validate(index, content[:16]), "piece %d is not part of the torrent", index)
		}, "piece %d", index)
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
		// Reaching through the pointer errors.As fills in only once it actually filled one in, since a nil one would
		// panic and take the whole test binary down rather than fail this case. The remaining names are still checked.
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) {
			t.Errorf("open %q should yield an *fs.PathError, got %T", name, err)
			continue
		}
		c.True(errors.Is(err, fs.ErrInvalid), "open %q should report fs.ErrInvalid", name)
		c.Equal(name, pathErr.Path)
	}

	_, err := f.Open("nope.txt")
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("opening a name that isn't there should yield an *fs.PathError, got %T", err)
	}
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

// TestNewFileFromBytesBoundsTheName covers the limit on the torrent's own name, the one dimension of the metadata that
// used to be left open. Only the single-file form presents the name as a path, where the bound on a path's length
// applies to it; the multi-file form never does, yet NewFileFromBytes still materializes a sanitized copy of it — up
// to twice its length — into Path, which lives as long as the File does and is stamped onto every log line naming the
// torrent. A 62,914,692 byte torrent, comfortably under MaxTorrentFileLength, whose name was 60MB of '@' parsed
// successfully, allocating roughly a gigabyte on the way, and NewFileFromReader is documented as taking a reader
// attached to a network stream.
func TestNewFileFromBytesBoundsTheName(t *testing.T) {
	c := check.New(t)

	// A name at the limit is accepted in both of the forms a torrent may take, and what is retained from it is bounded
	// even when every byte of it is one that sanitizing expands.
	longest := strings.Repeat("@", tfs.MaxNameLength)
	info := multiFileInfo()
	info["name"] = longest
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, info))
	c.NoError(err)
	c.Equal(longest, f.Info.Name)
	c.True(len(f.Path) <= 2*tfs.MaxNameLength, "%d bytes retained for a %d byte name", len(f.Path), tfs.MaxNameLength)

	info = singleFileInfo()
	info["name"] = longest
	_, err = tfs.NewFileFromBytes(encodeTorrent(t, info))
	c.NoError(err)

	// One byte beyond it is refused, in both forms and without the name reaching the message.
	for _, one := range []struct {
		info map[string]any
		name string
	}{
		{name: "the multi-file form", info: multiFileInfo()},
		{name: "the single-file form", info: singleFileInfo()},
	} {
		t.Run(one.name, func(t *testing.T) {
			sub := check.New(t)
			one.info["name"] = longest + "@"
			_, nameErr := tfs.NewFileFromBytes(encodeTorrent(t, one.info))
			sub.HasError(nameErr)
			if nameErr != nil {
				message, _, _ := strings.Cut(nameErr.Error(), "\n")
				sub.True(len(message) < 256, "%d byte message", len(message))
			}
		})
	}
}

// TestNewFileFromBytesRefusesUnsafePathComponents covers the components a path may not be assembled from. The tree is
// exported as an fs.FS, and what a consumer does with a path out of it is turn it back into a local one, with
// something on the order of filepath.Join(dir, filepath.FromSlash(p)); a component carrying a byte that separates or
// ends a path elsewhere is no longer one component below that directory when it does, so `..\..\evil.exe` is a lone,
// unremarkable looking node here and an escape from the target directory on Windows.
func TestNewFileFromBytesRefusesUnsafePathComponents(t *testing.T) {
	for _, one := range []struct {
		name  string
		parts []string
	}{
		{name: "a backslash escape", parts: []string{`..\..\evil.exe`}},
		{name: "a backslash separator", parts: []string{subDir + `\` + fileA}},
		{name: "a drive designation", parts: []string{"C:", "Windows", "evil.exe"}},
		{name: "a drive-relative name", parts: []string{"c:evil.exe"}},
		{name: "a NUL byte", parts: []string{subDir, "a\x00.txt"}},
		{name: "a device name", parts: []string{subDir, "CON"}},
		{name: "a device name with an extension", parts: []string{"COM1.txt"}},
		{name: "a device name with trailing spaces", parts: []string{"nul  "}},
		{name: "a device name in trailing colon form", parts: []string{"NUL:"}},
		{name: "a numbered device name in trailing colon form", parts: []string{subDir, "COM1:"}},
		{name: "a device name with a colon and more behind it", parts: []string{"con:fusion.txt"}},
		{name: "a device name with spaces before its colon", parts: []string{"prn :"}},
	} {
		t.Run(one.name, func(t *testing.T) {
			_, err := tfs.NewFileFromBytes(encodeTorrent(t, infoWithPath(one.parts...)))
			check.New(t).HasError(err)
		})
	}

	// A name that merely resembles one of those is an ordinary name, and the torrent carrying it parses.
	for _, one := range []struct {
		name  string
		parts []string
	}{
		{name: "a longer name starting with a device name", parts: []string{"CONSOLE.txt"}},
		{name: "a device name beyond the numbered ones", parts: []string{"COM10"}},
		{name: "a colon that isn't a drive", parts: []string{"12:30", fileA}},
		{name: "a colon further along", parts: []string{"ep 1: pilot.mkv"}},
	} {
		t.Run(one.name, func(t *testing.T) {
			sub := check.New(t)
			f, err := tfs.NewFileFromBytes(encodeTorrent(t, infoWithPath(one.parts...)))
			sub.NoError(err)
			if f != nil {
				fi, statErr := fs.Stat(f, strings.Join(one.parts, "/"))
				sub.NoError(statErr)
				sub.Equal(int64(12), fi.Size())
			}
		})
	}
}

// TestStorageIsResolvedWhenItIsNeeded verifies that a Path assigned after the virtual tree was built is the one the
// tree reads from. The tree used to cache the storage path the first time anything opened the torrent, and nothing
// invalidated it, so a caller that opened a file before the download directory was known — which fails, there being no
// storage yet — left every later read pointed at a path that StoragePath itself no longer reported. Path is exported
// and is set after construction by everything that uses this package, so the ordering was easy to fall into.
func TestStorageIsResolvedWhenItIsNeeded(t *testing.T) {
	c := check.New(t)
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, multiFileInfo()))
	c.NoError(err)

	// The first Open comes before the storage exists, which is what builds the tree.
	_, err = f.Open(fileB)
	c.HasError(err)
	c.True(errors.Is(err, fs.ErrNotExist))

	// The download directory is only known afterwards, so the storage lands where Path says once it has been set.
	f.Path = filepath.Join(t.TempDir(), f.Path)
	c.NoError(os.WriteFile(f.StoragePath(), []byte("0123456789abcdefghij"), 0o600))

	data, err := fs.ReadFile(f, fileB)
	c.NoError(err)
	c.Equal("cdefghij", string(data))
	data, err = fs.ReadFile(f, subDir+"/"+fileA)
	c.NoError(err)
	c.Equal("0123456789ab", string(data))
}

// TestStatAnswersFromTheMetadata verifies that stat-ing a node doesn't touch the storage file behind it. Without a
// Stat of its own, fs.Stat falls back to Open, which opens that file: a plain metadata query then failed with "no such
// file or directory" for every torrent whose data hadn't been downloaded yet, while fs.ReadDir of the directory
// holding it answered the same question correctly from the same tree, and cost an open and a close of it once it had.
func TestStatAnswersFromTheMetadata(t *testing.T) {
	c := check.New(t)
	f, err := tfs.NewFileFromBytes(encodeTorrent(t, multiFileInfo()))
	c.NoError(err)
	f.Path = filepath.Join(t.TempDir(), f.Path) // The storage file is never created.
	_, ok := any(f).(fs.StatFS)
	c.True(ok, "*tfs.File must implement fs.StatFS")

	// Everything a stat reports is known from the metadata, so it is reported whether the data is there or not.
	fi, err := fs.Stat(f, fileB)
	c.NoError(err)
	c.Equal(fileB, fi.Name())
	c.Equal(int64(8), fi.Size())
	c.False(fi.IsDir())
	fi, err = fs.Stat(f, subDir)
	c.NoError(err)
	c.True(fi.IsDir())

	// Which is the same answer the directory listing gives for the same node...
	entries, err := fs.ReadDir(f, subDir)
	c.NoError(err)
	c.Equal(1, len(entries))
	info, err := entries[0].Info()
	c.NoError(err)
	c.Equal(fi.ModTime(), info.ModTime())

	// ...while opening it still reports the storage that isn't there.
	_, err = f.Open(fileB)
	c.HasError(err)
	c.True(errors.Is(err, fs.ErrNotExist))

	// The fs.StatFS contract: a name that isn't valid, and one that is but names nothing.
	for _, one := range []struct {
		target error
		name   string
	}{
		{name: "../evil", target: fs.ErrInvalid},
		{name: "/b.txt", target: fs.ErrInvalid},
		{name: "nope.txt", target: fs.ErrNotExist},
		{name: subDir + "/nope.txt", target: fs.ErrNotExist},
	} {
		t.Run(one.name, func(t *testing.T) {
			sub := check.New(t)
			_, statErr := f.Stat(one.name)
			sub.True(errors.Is(statErr, one.target), "stat %q: %v", one.name, statErr)
			var pathErr *fs.PathError
			if !errors.As(statErr, &pathErr) {
				t.Fatalf("stat %q should yield an *fs.PathError, got %T", one.name, statErr)
			}
			sub.Equal("stat", pathErr.Op)
			sub.Equal(one.name, pathErr.Path)
		})
	}
}

// TestCloseWhileReadingIsSafe verifies that closing a file out from under the goroutines reading it reports itself
// rather than taking the process down. The use-after-close guards tested the file handle but then dereferenced the
// section reader, which Close nils out afterwards, so a read that had passed the guard and was descheduled panicked
// inside the section reader — and under -race the pair is a data race outright. os.File, which these files stand in
// for, is safe against a concurrent Close, so anything holding one of these is entitled to expect the same.
func TestCloseWhileReadingIsSafe(t *testing.T) {
	c := check.New(t)
	f := newPopulatedFile(t, multiFileInfo(), []byte("0123456789abcdefghij"))
	for range 25 {
		file, err := f.Open(subDir + "/" + fileA)
		c.NoError(err)
		readerAt, ok := file.(io.ReaderAt)
		c.True(ok)
		seeker, ok := file.(io.Seeker)
		c.True(ok)

		// Every goroutine waits on the same channel, so that the close lands while the reads are in flight rather than
		// before or after all of them.
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Go(func() {
				buffer := make([]byte, 4)
				<-start
				for range 32 {
					switch i % 3 {
					case 0:
						_, _ = file.Read(buffer) //nolint:errcheck // A closed file is one of the answers
					case 1:
						_, _ = readerAt.ReadAt(buffer, 0) //nolint:errcheck // A closed file is one of the answers
					default:
						_, _ = seeker.Seek(0, io.SeekStart) //nolint:errcheck // A closed file is one of the answers
					}
				}
			})
		}
		wg.Go(func() {
			<-start
			_ = file.Close() //nolint:errcheck // Whether this or a later close wins isn't what is being judged
		})
		close(start)
		wg.Wait()

		// Once it is closed, every operation says so instead of pretending to have read something.
		n, err := file.Read(make([]byte, 4))
		c.True(errors.Is(err, os.ErrClosed), "read after close: %v", err)
		c.Equal(0, n)
		_, err = readerAt.ReadAt(make([]byte, 4), 0)
		c.True(errors.Is(err, os.ErrClosed), "read at after close: %v", err)
		_, err = seeker.Seek(0, io.SeekStart)
		c.True(errors.Is(err, os.ErrClosed), "seek after close: %v", err)
		c.True(errors.Is(file.Close(), os.ErrClosed), "the second close")
	}
}

// TestCloseWhileReadingADirectoryIsSafe verifies that the directory handles Open hands out are as safe against a
// concurrent Close as the file handles alongside them. The package commits to os.File parity for that close on its
// files, and the directories came from the very same Open, but their read position and closed flag were touched with
// no synchronization at all: a Close racing a read is a data race outright, and two reads racing each other advance
// the shared position between the check and the slice it takes, which is how the same child is handed out twice.
func TestCloseWhileReadingADirectoryIsSafe(t *testing.T) {
	c := check.New(t)
	f := newPopulatedFile(t, multiFileInfo(), []byte("0123456789abcdefghij"))
	for range 25 {
		dir, err := f.Open(".")
		c.NoError(err)
		readDirFile, ok := dir.(fs.ReadDirFile)
		c.True(ok)
		legacy, ok := dir.(readdirFile)
		c.True(ok)

		// Every goroutine waits on the same channel, so that the close lands while the reads are in flight rather than
		// before or after all of them.
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Go(func() {
				<-start
				for range 32 {
					if i%2 == 0 {
						_, _ = readDirFile.ReadDir(1) //nolint:errcheck // A closed directory is one of the answers
					} else {
						_, _ = legacy.Readdir(1) //nolint:errcheck // A closed directory is one of the answers
					}
				}
			})
		}
		wg.Go(func() {
			<-start
			_ = dir.Close() //nolint:errcheck // Whether this or a later close wins isn't what is being judged
		})
		close(start)
		wg.Wait()

		// Once it is closed, every read says so instead of handing back an entry.
		entries, err := readDirFile.ReadDir(1)
		c.True(errors.Is(err, os.ErrClosed), "read dir after close: %v", err)
		c.Equal(0, len(entries))
		_, err = legacy.Readdir(1)
		c.True(errors.Is(err, os.ErrClosed), "readdir after close: %v", err)
		c.True(errors.Is(dir.Close(), os.ErrClosed), "the second close")
	}

	// Nothing above depends on a read having actually succeeded, so one is made on its own: a directory that answered
	// every read with an error would satisfy the whole of it.
	dir, err := f.Open(".")
	c.NoError(err)
	readDirFile, ok := dir.(fs.ReadDirFile)
	c.True(ok)
	entries, err := readDirFile.ReadDir(-1)
	c.NoError(err)
	c.Equal(2, len(entries))
	c.NoError(dir.Close())
}
