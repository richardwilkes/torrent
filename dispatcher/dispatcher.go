package dispatcher

import (
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richardwilkes/toolbox/errs"
	"github.com/richardwilkes/toolbox/log/logadapter"
	"github.com/richardwilkes/toolbox/rate"
	"github.com/richardwilkes/toolbox/xio"
	"github.com/richardwilkes/toolbox/xio/network"
	"github.com/richardwilkes/toolbox/xio/network/natpmp"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/richardwilkes/torrent/tio"
)

// ConnectionHandler defines the interface for handling torrent connections.
type ConnectionHandler interface {
	HandleConnection(conn net.Conn, log logadapter.Logger, extensions ProtocolExtensions, infoHash tfs.InfoHash, sendHandshake bool)
}

// Dispatcher holds a dispatcher for bit torrent connections.
type Dispatcher struct {
	InRate              rate.Limiter
	OutRate             rate.Limiter
	listener            net.Listener
	logger              logadapter.Logger
	natpmpChan          chan interface{}
	handlers            sync.Map
	gatekeeper          *GateKeeper
	internalPort        uint32
	externalPort        uint32
	lock                sync.Mutex
	externalIP          string
	lastExternalIPCheck time.Time
}

// NewDispatcher creates a new dispatcher and starts listening for
// connections.
func NewDispatcher(options ...func(*Dispatcher) error) (*Dispatcher, error) {
	d := &Dispatcher{
		InRate:     rate.New(math.MaxInt32, time.Second),
		OutRate:    rate.New(math.MaxInt32, time.Second),
		logger:     &logadapter.Discarder{},
		gatekeeper: NewGateKeeper(),
	}
	var err error
	for _, option := range options {
		if err = option(d); err != nil {
			return nil, err
		}
	}
	if d.internalPort == 0 {
		if d.listener, err = net.Listen("tcp", ":0"); err != nil { //nolint:gosec
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
		port, lerr := strconv.Atoi(portStr)
		if lerr != nil {
			lerr = errs.Wrap(err)
			if err = d.listener.Close(); err != nil {
				lerr = errs.Append(lerr, err)
			}
			return nil, lerr
		}
		d.internalPort = uint32(port)
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
		port, err := natpmp.MapTCP(int(d.internalPort), d.natpmpChan)
		if err != nil {
			if lerr := d.listener.Close(); lerr != nil {
				err = errs.Append(err, lerr)
			}
			return nil, err
		}
		d.externalPort = uint32(port)
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

// InternalPort returns the internal port that we're listening on.
func (d *Dispatcher) InternalPort() uint32 {
	return d.internalPort
}

// ExternalPort returns the external port that we're listening on.
func (d *Dispatcher) ExternalPort() uint32 {
	return atomic.LoadUint32(&d.externalPort)
}

// Stop accepting connections and shutdown.
func (d *Dispatcher) Stop() {
	d.gatekeeper.Close()
	if err := d.listener.Close(); err != nil {
		d.logger.Warn(err)
	}
}

// Register a connection handler with this dispatcher.
func (d *Dispatcher) Register(infoHash tfs.InfoHash, handler ConnectionHandler) {
	d.handlers.Store(infoHash, handler)
}

// Deregister a connection handler from this dispatcher.
func (d *Dispatcher) Deregister(infoHash tfs.InfoHash) {
	d.handlers.Delete(infoHash)
}

// ExternalIP returns our external IP address.
func (d *Dispatcher) ExternalIP() string {
	const unknownIP = "<unknown>"
	d.lock.Lock()
	defer d.lock.Unlock()
	if time.Since(d.lastExternalIPCheck) < time.Hour && d.externalIP != "" && d.externalIP != unknownIP {
		return d.externalIP
	}
	d.externalIP = ""
	if d.natpmpChan != nil {
		if ip, err := natpmp.ExternalAddress(); err == nil {
			d.externalIP = ip.String()
		}
	} else {
		d.externalIP = network.ExternalIP(5 * time.Second)
	}
	if d.externalIP == "" {
		d.externalIP = unknownIP
	}
	d.lastExternalIPCheck = time.Now()
	return d.externalIP
}

func (d *Dispatcher) listen() {
	d.logger.Infof("Listening on port %d (external: %s:%d)", d.InternalPort(), d.ExternalIP(), d.ExternalPort())
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if d.natpmpChan != nil {
				d.natpmpChan <- nil
			}
			d.logger.Infof("Stopped listening on port %d", d.InternalPort())
			return
		}
		go d.dispatch(conn)
	}
}

func (d *Dispatcher) dispatch(conn net.Conn) {
	log := &logadapter.Prefixer{Logger: d.logger, Prefix: conn.RemoteAddr().String() + " | "}
	defer xio.CloseIgnoringErrors(conn)
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
	if handler, ok := d.handlers.Load(infoHash); ok {
		handler.(ConnectionHandler).HandleConnection(conn, log, extensions, infoHash, true)
	}
}

func (d *Dispatcher) monitorNatPMP() {
	for data := range d.natpmpChan {
		switch value := data.(type) {
		case int:
			old := d.ExternalPort()
			atomic.StoreUint32(&d.externalPort, uint32(value))
			d.logger.Infof("External port changed from %d to %d", old, value)
		case error:
			d.logger.Warn(value)
		default:
			return
		}
	}
}
