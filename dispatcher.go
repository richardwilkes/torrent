package torrent

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
)

const (
	handshakeDeadline = 5 * time.Second
	extensionsSize    = 8
	peerIDSize        = 20
)

var protocolIdentifier []byte

func init() {
	const id = "BitTorrent protocol"
	protocolIdentifier = make([]byte, 1+len(id))
	protocolIdentifier[0] = byte(len(id))
	copy(protocolIdentifier[1:], []byte(id))
}

// Dispatcher holds a dispatcher for bit torrent connections.
type Dispatcher struct {
	InRate          rate.Limiter
	OutRate         rate.Limiter
	listener        net.Listener
	logger          logadapter.Logger
	natpmpChan      chan interface{}
	portLock        sync.RWMutex
	internalPort    int
	externalPort    int
	routerLock      sync.RWMutex
	clients         map[[sha1.Size]byte]*Client
	rejectLock      sync.RWMutex
	rejectAddresses map[string]time.Time
	rejectDone      chan bool
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
		InRate:          rate.New(math.MaxInt32, time.Second),
		OutRate:         rate.New(math.MaxInt32, time.Second),
		logger:          &logadapter.Discarder{},
		clients:         make(map[[sha1.Size]byte]*Client),
		rejectAddresses: make(map[string]time.Time),
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

func (d *Dispatcher) rejectAddress(addr net.Addr) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return
	}
	d.rejectLock.Lock()
	d.rejectAddresses[host] = time.Now().Add(15 * time.Minute)
	if d.rejectDone == nil {
		d.rejectDone = make(chan bool)
		go d.pruneRejectedAddresses()
	}
	d.rejectLock.Unlock()
}

func (d *Dispatcher) isAddressAcceptable(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	return d.isAddressStringAcceptable(host)
}

func (d *Dispatcher) isAddressStringAcceptable(addr string) bool {
	d.rejectLock.RLock()
	defer d.rejectLock.RUnlock()
	expires, ok := d.rejectAddresses[addr]
	return !ok || expires.Before(time.Now())
}

func (d *Dispatcher) pruneRejectedAddresses() {
	for {
		select {
		case <-time.After(15 * time.Minute):
			d.rejectLock.Lock()
			for k, v := range d.rejectAddresses {
				if v.Before(time.Now()) {
					delete(d.rejectAddresses, k)
				}
			}
			d.rejectLock.Unlock()
		case <-d.rejectDone:
			return
		}
	}
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
	d.rejectLock.Lock()
	if d.rejectDone != nil {
		d.rejectDone <- true
	}
	d.rejectLock.Unlock()
	if err := d.listener.Close(); err != nil {
		d.logger.Warn(err)
	}
}

func (d *Dispatcher) register(infoHash [sha1.Size]byte, client *Client) {
	d.routerLock.Lock()
	d.clients[infoHash] = client
	d.routerLock.Unlock()
}

func (d *Dispatcher) deregister(infoHash [sha1.Size]byte) {
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
	if !d.isAddressAcceptable(conn.RemoteAddr()) {
		return
	}
	extensions, infoHash, err := receiveTorrentHandshake(conn)
	if err != nil {
		if shouldLogIOError(err) {
			log.Warn(err)
		}
		return
	}
	d.routerLock.RLock()
	client, ok := d.clients[infoHash]
	d.routerLock.RUnlock()
	if ok {
		client.handleConnection(conn, log, extensions, infoHash, true)
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

func receiveTorrentHandshake(conn net.Conn) (extensions [extensionsSize]byte, infoHash [sha1.Size]byte, err error) {
	buffer := make([]byte, len(protocolIdentifier))
	if err = readWithDeadline(conn, buffer, handshakeDeadline); err != nil {
		return
	}
	if !bytes.Equal(buffer, protocolIdentifier) {
		err = io.EOF // Invalid protocol identifier; just return EOF to indicate failure.
		return
	}
	if err = readWithDeadline(conn, extensions[:], handshakeDeadline); err != nil {
		return
	}
	err = readWithDeadline(conn, infoHash[:], handshakeDeadline)
	return
}

func sendTorrentHandshake(conn net.Conn, extensions [extensionsSize]byte, infoHash [sha1.Size]byte, clientID [peerIDSize]byte) error {
	buffer := make([]byte, len(protocolIdentifier)+extensionsSize+sha1.Size+peerIDSize)
	copy(buffer[:len(protocolIdentifier)], protocolIdentifier)
	copy(buffer[len(protocolIdentifier):len(protocolIdentifier)+extensionsSize], extensions[:])
	copy(buffer[len(protocolIdentifier)+extensionsSize:len(protocolIdentifier)+extensionsSize+sha1.Size], infoHash[:])
	copy(buffer[len(protocolIdentifier)+extensionsSize+sha1.Size:], clientID[:])
	return writeWithDeadline(conn, buffer, handshakeDeadline)
}
