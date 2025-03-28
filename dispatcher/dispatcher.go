package dispatcher

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richardwilkes/toolbox/errs"
	"github.com/richardwilkes/toolbox/rate"
	"github.com/richardwilkes/toolbox/xio"
	"github.com/richardwilkes/toolbox/xio/network"
	"github.com/richardwilkes/toolbox/xio/network/natpmp"
	"github.com/richardwilkes/torrent/tfs"
	"github.com/richardwilkes/torrent/tio"
)

// ConnectionHandler defines the interface for handling torrent connections.
type ConnectionHandler interface {
	HandleConnection(conn net.Conn, log *slog.Logger, extensions ProtocolExtensions, infoHash tfs.InfoHash, sendHandshake bool)
}

// Dispatcher holds a dispatcher for bit torrent connections.
type Dispatcher struct {
	InRate              rate.Limiter
	OutRate             rate.Limiter
	listener            net.Listener
	logger              *slog.Logger
	natpmpChan          chan any
	gatekeeper          *GateKeeper
	handlers            sync.Map
	lastExternalIPCheck time.Time // protected by lock
	externalIP          string    // protected by lock
	internalPort        uint32
	externalPort        uint32
	lock                sync.Mutex
}

// NewDispatcher creates a new dispatcher and starts listening for
// connections.
func NewDispatcher(options ...func(*Dispatcher) error) (*Dispatcher, error) {
	d := &Dispatcher{
		InRate:     rate.New(math.MaxInt32, time.Second),
		OutRate:    rate.New(math.MaxInt32, time.Second),
		logger:     slog.Default(),
		gatekeeper: NewGateKeeper(),
	}
	var err error
	for _, option := range options {
		if err = option(d); err != nil {
			return nil, err
		}
	}
	if d.internalPort == 0 {
		if d.listener, err = net.Listen("tcp", ":0"); err != nil { //nolint:gosec // We intentionally want all network interfaces
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
			return nil, errs.Newf("unable to listen on any port in the range %d to %d", d.internalPort, d.externalPort)
		}
	}
	if d.natpmpChan != nil {
		var port int
		if port, err = natpmp.MapTCP(int(d.internalPort), d.natpmpChan); err != nil {
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
func (d *Dispatcher) Logger() *slog.Logger {
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
		errs.LogTo(d.logger, err)
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
	d.logger.Info("listening", "port", d.InternalPort(), "external_ip", d.ExternalIP(), "external_port", d.ExternalPort())
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			if d.natpmpChan != nil {
				d.natpmpChan <- nil
			}
			d.logger.Info("stopped listening", "port", d.InternalPort())
			return
		}
		go d.dispatch(conn)
	}
}

func (d *Dispatcher) dispatch(conn net.Conn) {
	logger := d.logger.With("remote_addr", conn.RemoteAddr().String())
	defer xio.CloseIgnoringErrors(conn)
	if d.gatekeeper.IsAddressBlocked(conn.RemoteAddr()) {
		return
	}
	extensions, infoHash, err := ReceiveTorrentHandshake(conn)
	if err != nil {
		if tio.ShouldLogIOError(err) {
			errs.LogTo(logger, err)
		}
		return
	}
	if handler, ok := d.handlers.Load(infoHash); ok {
		if connHandler, ok2 := handler.(ConnectionHandler); ok2 {
			connHandler.HandleConnection(conn, logger, extensions, infoHash, true)
		}
	}
}

func (d *Dispatcher) monitorNatPMP() {
	for data := range d.natpmpChan {
		switch value := data.(type) {
		case int:
			old := d.ExternalPort()
			atomic.StoreUint32(&d.externalPort, uint32(value))
			d.logger.Info("external port changed", "from", old, "to", value)
		case error:
			errs.LogTo(d.logger, value)
		default:
			return
		}
	}
}
