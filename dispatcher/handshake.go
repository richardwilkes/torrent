package dispatcher

import (
	"bytes"
	"crypto/sha1" //nolint:gosec
	"io"
	"net"
	"time"

	"github.com/richardwilkes/torrent/tfs"
	"github.com/richardwilkes/torrent/tio"
)

// HandshakeDeadline is the maximum amount of time allowed for a handshake
// read or write.
const HandshakeDeadline = 5 * time.Second

// PeerID holds a peer's ID.
type PeerID [20]byte

// ProtocolExtensions holds any protocol extensions.
type ProtocolExtensions [8]byte

var protocolIdentifier = []byte{19, 'B', 'i', 't', 'T', 'o', 'r', 'r', 'e', 'n', 't', ' ', 'p', 'r', 'o', 't', 'o', 'c', 'o', 'l'}

// ReceiveTorrentHandshake reads the torrent protocol handshake.
func ReceiveTorrentHandshake(conn net.Conn) (extensions ProtocolExtensions, infoHash tfs.InfoHash, err error) {
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
func SendTorrentHandshake(conn net.Conn, extensions ProtocolExtensions, infoHash tfs.InfoHash, clientID PeerID) error {
	buffer := make([]byte, len(protocolIdentifier)+len(extensions)+sha1.Size+len(clientID))
	copy(buffer[:len(protocolIdentifier)], protocolIdentifier)
	copy(buffer[len(protocolIdentifier):len(protocolIdentifier)+len(extensions)], extensions[:])
	copy(buffer[len(protocolIdentifier)+len(extensions):len(protocolIdentifier)+len(extensions)+sha1.Size], infoHash[:])
	copy(buffer[len(protocolIdentifier)+len(extensions)+sha1.Size:], clientID[:])
	return tio.WriteWithDeadline(conn, buffer, HandshakeDeadline)
}
