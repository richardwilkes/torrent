package dispatcher

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/richardwilkes/errs"
	"github.com/richardwilkes/fileutil"
	"github.com/richardwilkes/logadapter"
	"github.com/richardwilkes/natpmp"
	"github.com/richardwilkes/rate"
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

// ConnectionHandler defines the interface for handling torrent connections.
type ConnectionHandler interface {
	HandleConnection(conn net.Conn, log logadapter.Logger, extensions [ExtensionsSize]byte, infoHash [sha1.Size]byte, sendHandshake bool)
}

// Dispatcher holds a dispatcher for bit torrent connections.
type Dispatcher struct {
	InRate       rate.Limiter
	OutRate      rate.Limiter
	listener     net.Listener
	logger       logadapter.Logger
	natpmpChan   chan interface{}
	portLock     sync.RWMutex
	internalPort int
	externalPort int
	routerLock   sync.RWMutex
	clients      map[[sha1.Size]byte]ConnectionHandler
	gatekeeper   *GateKeeper
}

// GlobalDownloadCap sets the maximum download speed of the dispatcher.
// Default is no limit.
func GlobalDownloadCap(bytesPerSecond int) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if bytesPerSecond < 1 {
			return errs.New("DownloadCap must be at least 1")
		}
		d.InRate.SetCap(bytesPerSecond)
		return nil
	}
}

// GlobalUploadCap sets the maximum upload speed of the dispatcher. Default
// is no limit.
func GlobalUploadCap(bytesPerSecond int) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if bytesPerSecond < 1 {
			return errs.New("UploadCap must be at least 1")
		}
		d.OutRate.SetCap(bytesPerSecond)
		return nil
	}
}

// LogTo sets the logger the dispatcher should use. Default discards logs.
func LogTo(logger logadapter.Logger) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		d.logger = logger
		return nil
	}
}

// UseNATPMP sets the dispatcher to use NatPMP to route traffic through the
// external gateway.
func UseNATPMP(d *Dispatcher) error {
	d.natpmpChan = make(chan interface{}, 1)
	return nil
}

// PortRange sets the available ports to listen on to the specified range.
// Default is to let the system choose a random port.
func PortRange(from, to int) func(*Dispatcher) error {
	return func(d *Dispatcher) error {
		if from < 1 || from > 65536 || to < 1 || to > 65536 {
			return errs.New("Ports must be in the range 1 to 65535")
		}
		if from > to {
			d.internalPort = to
			d.externalPort = from
		} else {
			d.internalPort = from
			d.externalPort = to
		}
		return nil
	}
}

// NewDispatcher creates a new dispatcher and starts listening for
// connections.
func NewDispatcher(options ...func(*Dispatcher) error) (*Dispatcher, error) {
	d := &Dispatcher{
		InRate:     rate.New(math.MaxInt32, time.Second),
		OutRate:    rate.New(math.MaxInt32, time.Second),
		logger:     &logadapter.Discarder{},
		clients:    make(map[[sha1.Size]byte]ConnectionHandler),
		gatekeeper: NewGateKeeper(),
	}
	var err error
	for _, option := range options {
		if err = option(d); err != nil {
			return nil, err
		}
	}
	if d.internalPort == 0 {
		if d.listener, err = net.Listen("tcp", ":0"); err != nil {
			return nil, errs.Wrap(err)
		}
		_, portStr, lerr := net.SplitHostPort(d.listener.Addr().String())
		if lerr != nil {
			lerr = errs.Wrap(err)
			if err = d.listener.Close(); err != nil {
				lerr = errs.Append(lerr, err)
			}
			return nil, lerr
		}
		if d.internalPort, lerr = strconv.Atoi(portStr); lerr != nil {
			lerr = errs.Wrap(err)
			if err = d.listener.Close(); err != nil {
				lerr = errs.Append(lerr, err)
			}
			return nil, lerr
		}
	} else {
		success := false
		for port := d.internalPort; port <= d.externalPort; port++ {
			if d.listener, err = net.Listen("tcp", fmt.Sprintf(":%d", port)); err == nil {
				success = true
				d.internalPort = port
				break
			}
		}
		if !success {
			return nil, errs.Newf("Unable to listen on any port in the range %d to %d", d.internalPort, d.externalPort)
		}
	}
	if d.natpmpChan != nil {
		if d.externalPort, err = natpmp.MapTCP(d.internalPort, d.natpmpChan); err != nil {
			if lerr := d.listener.Close(); lerr != nil {
				err = errs.Append(err, lerr)
			}
			return nil, err
		}
		go d.monitorNatPMP()
	} else {
		d.externalPort = d.internalPort
	}
	go d.listen()
	return d, nil
}

// Logger returns the logger being used by this dispatcher.
func (d *Dispatcher) Logger() logadapter.Logger {
	return d.logger
}

// GateKeeper returns the GateKeeper being used by this dispatcher.
func (d *Dispatcher) GateKeeper() *GateKeeper {
	return d.gatekeeper
}

// Ports returns the internal and external ports that we're listening on.
func (d *Dispatcher) Ports() (internal int, external int) {
	d.portLock.RLock()
	defer d.portLock.RUnlock()
	return d.internalPort, d.externalPort
}

// Stop terminates all connections made through this dispatcher and shuts it
// down.
func (d *Dispatcher) Stop() {
	d.gatekeeper.Close()
	if err := d.listener.Close(); err != nil {
		d.logger.Warn(err)
	}
}

// Register a connection handler with this dispatcher.
func (d *Dispatcher) Register(infoHash [sha1.Size]byte, handler ConnectionHandler) {
	d.routerLock.Lock()
	d.clients[infoHash] = handler
	d.routerLock.Unlock()
}

// Deregister a connection handler from this dispatcher.
func (d *Dispatcher) Deregister(infoHash [sha1.Size]byte) {
	d.routerLock.Lock()
	delete(d.clients, infoHash)
	d.routerLock.Unlock()
}

func (d *Dispatcher) listen() {
	in, ex := d.Ports()
	var ipStr string
	if ip, err := natpmp.ExternalAddress(); err != nil {
		ipStr = "<unknown>"
	} else {
		ipStr = ip.String()
	}
	d.logger.Infof("Listening on port %d (external: %s:%d)", in, ipStr, ex)
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if d.natpmpChan != nil {
				d.natpmpChan <- nil
			}
			d.logger.Infof("Stopped listening on port %d", in)
			return
		}
		go d.dispatch(conn)
	}
}

func (d *Dispatcher) dispatch(conn net.Conn) {
	log := &logadapter.Prefixer{Logger: d.logger, Prefix: conn.RemoteAddr().String() + " | "}
	defer fileutil.CloseIgnoringErrors(conn)
	if d.gatekeeper.IsAddressBlocked(conn.RemoteAddr()) {
		return
	}
	extensions, infoHash, err := ReceiveTorrentHandshake(conn)
	if err != nil {
		if tio.ShouldLogIOError(err) {
			log.Warn(err)
		}
		return
	}
	d.routerLock.RLock()
	client, ok := d.clients[infoHash]
	d.routerLock.RUnlock()
	if ok {
		client.HandleConnection(conn, log, extensions, infoHash, true)
	}
}

func (d *Dispatcher) monitorNatPMP() {
	for data := range d.natpmpChan {
		switch value := data.(type) {
		case int:
			d.portLock.Lock()
			old := d.externalPort
			d.externalPort = value
			d.portLock.Unlock()
			d.logger.Infof("External port changed from %d to %d", old, value)
		case error:
			d.logger.Warn(value)
		default:
			return
		}
	}
}

// ReceiveTorrentHandshake reads the torrent protocol handshake.
func ReceiveTorrentHandshake(conn net.Conn) (extensions [ExtensionsSize]byte, infoHash [sha1.Size]byte, err error) {
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
func SendTorrentHandshake(conn net.Conn, extensions [ExtensionsSize]byte, infoHash [sha1.Size]byte, clientID [PeerIDSize]byte) error {
	buffer := make([]byte, len(protocolIdentifier)+ExtensionsSize+sha1.Size+PeerIDSize)
	copy(buffer[:len(protocolIdentifier)], protocolIdentifier)
	copy(buffer[len(protocolIdentifier):len(protocolIdentifier)+ExtensionsSize], extensions[:])
	copy(buffer[len(protocolIdentifier)+ExtensionsSize:len(protocolIdentifier)+ExtensionsSize+sha1.Size], infoHash[:])
	copy(buffer[len(protocolIdentifier)+ExtensionsSize+sha1.Size:], clientID[:])
	return tio.WriteWithDeadline(conn, buffer, HandshakeDeadline)
}
