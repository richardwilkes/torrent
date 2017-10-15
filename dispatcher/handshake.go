package dispatcher

import (
	"bytes"
	"crypto/sha1"
	"io"
	"net"
	"time"

	"github.com/richardwilkes/torrent/tfs"
	"github.com/richardwilkes/torrent/tio"
)

const (
	// HandshakeDeadline is the maximum amount of time allowed for a handshake
	// read or write.
	HandshakeDeadline = 5 * time.Second
	// ExtensionsSize is the number of bytes comprising the extension data.
	ExtensionsSize = 8
	// PeerIDSize is the number of bytes comprising a peer ID.
	PeerIDSize = 20
)

var protocolIdentifier = []byte{19, 'B', 'i', 't', 'T', 'o', 'r', 'r', 'e', 'n', 't', ' ', 'p', 'r', 'o', 't', 'o', 'c', 'o', 'l'}

// ReceiveTorrentHandshake reads the torrent protocol handshake.
func ReceiveTorrentHandshake(conn net.Conn) (extensions [ExtensionsSize]byte, infoHash tfs.InfoHash, err error) {
	buffer := make([]byte, len(protocolIdentifier))
	if err = tio.ReadWithDeadline(conn, buffer, HandshakeDeadline); err != nil {
		return
	}
	if !bytes.Equal(buffer, protocolIdentifier) {
		err = io.EOF // Invalid protocol identifier; just return EOF to indicate failure.
		return
	}
	if err = tio.ReadWithDeadline(conn, extensions[:], HandshakeDeadline); err != nil {
		return
	}
	err = tio.ReadWithDeadline(conn, infoHash[:], HandshakeDeadline)
	return
}

// SendTorrentHandshake sends the torrent protocol handshake.
func SendTorrentHandshake(conn net.Conn, extensions [ExtensionsSize]byte, infoHash tfs.InfoHash, clientID [PeerIDSize]byte) error {
	buffer := make([]byte, len(protocolIdentifier)+ExtensionsSize+sha1.Size+PeerIDSize)
	copy(buffer[:len(protocolIdentifier)], protocolIdentifier)
	copy(buffer[len(protocolIdentifier):len(protocolIdentifier)+ExtensionsSize], extensions[:])
	copy(buffer[len(protocolIdentifier)+ExtensionsSize:len(protocolIdentifier)+ExtensionsSize+sha1.Size], infoHash[:])
	copy(buffer[len(protocolIdentifier)+ExtensionsSize+sha1.Size:], clientID[:])
	return tio.WriteWithDeadline(conn, buffer, HandshakeDeadline)
}
