package memberlist

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	sockaddr "github.com/hashicorp/go-sockaddr"
	"github.com/hashicorp/memberlist"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/atomic"

	dstls "github.com/grafana/dskit/crypto/tls"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/netutil"
)

// QuicTransportConfig is a configuration structure for creating new QuicTransport.
type QuicTransportConfig struct {
	// BindAddrs is a list of IP addresses to bind to.
	BindAddrs flagext.StringSlice `yaml:"bind_addr"`

	// BindPort is the port to listen on, for each address above.
	BindPort int `yaml:"bind_port"`

	// Timeout used when making connections to other nodes to send packet.
	// Zero = no timeout
	PacketDialTimeout time.Duration `yaml:"packet_dial_timeout" category:"advanced"`

	// Timeout for writing packet data. Zero = no timeout.
	PacketWriteTimeout time.Duration `yaml:"packet_write_timeout" category:"advanced"`

	// Transport logs lot of messages at debug level, so it deserves an extra flag for turning it on
	TransportDebug bool `yaml:"-" category:"advanced"`

	// Where to put custom metrics. nil = don't register.
	MetricsNamespace string `yaml:"-"`

	// TLS is required for quic
	// TLSEnabled bool               `yaml:"tls_enabled" category:"advanced"`
	TLS dstls.ClientConfig `yaml:",inline"`
}

func (cfg *QuicTransportConfig) RegisterFlags(f *flag.FlagSet) {
	cfg.RegisterFlagsWithPrefix(f, "")
}

// RegisterFlagsWithPrefix registers flags with prefix.
func (cfg *QuicTransportConfig) RegisterFlagsWithPrefix(f *flag.FlagSet, prefix string) {
	// "Defaults to hostname" -- memberlist sets it to hostname by default.
	f.Var(&cfg.BindAddrs, prefix+"memberlist.bind-addr", "IP address to listen on for gossip messages. Multiple addresses may be specified. Defaults to 0.0.0.0")
	f.IntVar(&cfg.BindPort, prefix+"memberlist.bind-port", 7946, "Port to listen on for gossip messages.")
	f.DurationVar(&cfg.PacketDialTimeout, prefix+"memberlist.packet-dial-timeout", 2*time.Second, "Timeout used when connecting to other nodes to send packet.")
	f.DurationVar(&cfg.PacketWriteTimeout, prefix+"memberlist.packet-write-timeout", 5*time.Second, "Timeout for writing 'packet' data.")
	f.BoolVar(&cfg.TransportDebug, prefix+"memberlist.transport-debug", false, "Log debug transport messages. Note: global log.level must be at debug level as well.")

	// f.BoolVar(&cfg.TLSEnabled, prefix+"memberlist.tls-enabled", false, "Enable TLS on the memberlist transport layer.")
	cfg.TLS.RegisterFlagsWithPrefix(prefix+"memberlist", f)
}

// QuicTransport is a memberlist.Transport implementation that uses Quic for both packet and stream
// operations ("packet" and "stream" are terms used by memberlist).
// It uses a new Quic connections for each operation. There is no connection reuse.
type QuicTransport struct {
	cfg           QuicTransportConfig
	logger        log.Logger
	packetCh      chan *memberlist.Packet
	connCh        chan quic.Connection
	wg            sync.WaitGroup
	quicListeners []quic.Listener
	tlsConfig     *tls.Config

	shutdown atomic.Int32

	advertiseMu   sync.RWMutex
	advertiseAddr string

	// metrics
	incomingStreams      prometheus.Counter
	outgoingStreams      prometheus.Counter
	outgoingStreamErrors prometheus.Counter

	receivedPackets       prometheus.Counter
	receivedPacketsBytes  prometheus.Counter
	receivedPacketsErrors prometheus.Counter
	sentPackets           prometheus.Counter
	sentPacketsBytes      prometheus.Counter
	sentPacketsErrors     prometheus.Counter
	unknownConnections    prometheus.Counter
}

// NewQuicTransport returns a new quic-based transport with the given configuration. On
// success all the network listeners will be created and listening.
func NewQuicTransport(config QuicTransportConfig, logger log.Logger, registerer prometheus.Registerer) (*QuicTransport, error) {
	if len(config.BindAddrs) == 0 {
		config.BindAddrs = []string{zeroZeroZeroZero}
	}

	// Build out the new transport.
	var ok bool
	t := QuicTransport{
		cfg:      config,
		logger:   log.With(logger, "component", "memberlist QuicTransport"),
		packetCh: make(chan *memberlist.Packet),
		connCh:   make(chan quic.Connection),
	}

	var err error
	t.tlsConfig, err = config.TLS.GetTLSConfig()
	if err != nil {
		return nil, errors.Wrap(err, "unable to create TLS config")
	}

	t.registerMetrics(registerer)

	// Clean up listeners if there's an error.
	defer func() {
		if !ok {
			_ = t.Shutdown()
		}
	}()

	// Build all the Quic and UDP listeners.
	port := config.BindPort
	for _, addr := range config.BindAddrs {
		ip := net.ParseIP(addr)
		if ip == nil {
			return nil, fmt.Errorf("could not parse bind addr %q as IP address", addr)
		}

		var quickLn *quic.Listener
		// TODO: support "udp6"
		udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to start UDP listener on %q port %d", addr, port)
		}

		tr := quic.Transport{
			Conn: udpConn,
		}
		quickLn, err = tr.Listen(t.tlsConfig, &quic.Config{})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to start TLS Quic listener on %q port %d", addr, port)
		}

		// TODO: why aren't we storing a pointer?
		t.quicListeners = append(t.quicListeners, *quickLn)

		// If the config port given was zero, use the first Quic listener
		// to pick an available port and then apply that to everything
		// else.
		if port == 0 {
			port = quickLn.Addr().(*net.UDPAddr).Port
		}
	}

	// Fire them up now that we've been able to create them all.
	for i := 0; i < len(config.BindAddrs); i++ {
		t.wg.Add(1)
		go t.quicListen(t.quicListeners[i])
	}

	ok = true
	return &t, nil
}

// quicListen is a long running goroutine that accepts incoming Quic connections
// and spawns new go routine to handle each connection. This transport uses Quic connections
// for both packet sending and streams.
// (copied from Memberlist net_transport.go)
func (t *QuicTransport) quicListen(quicLn quic.Listener) {
	defer t.wg.Done()

	// baseDelay is the initial delay after an AcceptQuic() error before attempting again
	const baseDelay = 5 * time.Millisecond

	// maxDelay is the maximum delay after an AcceptQuic() error before attempting again.
	// In the case that quicListen() is error-looping, it will delay the shutdown check.
	// Therefore, changes to maxDelay may have an effect on the latency of shutdown.
	const maxDelay = 1 * time.Second

	var loopDelay time.Duration
	for {
		conn, err := quicLn.Accept(context.TODO())
		if err != nil {
			if s := t.shutdown.Load(); s == 1 {
				break
			}

			if loopDelay == 0 {
				loopDelay = baseDelay
			} else {
				loopDelay *= 2
			}

			if loopDelay > maxDelay {
				loopDelay = maxDelay
			}

			level.Error(t.logger).Log("msg", "Error accepting Quic connection", "err", err)
			time.Sleep(loopDelay)
			continue
		}
		// No error, reset loop delay
		loopDelay = 0

		go t.handleConnection(conn)
	}
}

func (t *QuicTransport) debugLog() log.Logger {
	if t.cfg.TransportDebug {
		return level.Debug(t.logger)
	}
	return noopLogger
}

func (t *QuicTransport) handleConnection(conn quic.Connection) {
	t.debugLog().Log("msg", "New connection", "addr", conn.RemoteAddr())

	closeConn := true
	defer func() {
		if closeConn {
			// _ = conn.Close()
		}
	}()

	str, err := conn.AcceptStream(context.TODO())
	if err != nil {
		level.Warn(t.logger).Log("msg", "failed to accept stream", "err", err, "remote", conn.RemoteAddr())
		return
	}

	// let's read first byte, and determine what to do about this connection
	msgType := []byte{0}
	_, err = io.ReadFull(str, msgType)
	if err != nil {
		level.Warn(t.logger).Log("msg", "failed to read message type", "err", err, "remote", conn.RemoteAddr())
		return
	}

	if messageType(msgType[0]) == stream {
		t.incomingStreams.Inc()

		// hand over this connection to memberlist
		closeConn = false
		t.connCh <- conn
	} else if messageType(msgType[0]) == packet {
		// it's a memberlist "packet", which contains an address and data.
		t.receivedPackets.Inc()

		// before reading packet, read the address
		addrLengthBuf := []byte{0}
		_, err := io.ReadFull(str, addrLengthBuf)
		if err != nil {
			t.receivedPacketsErrors.Inc()
			level.Warn(t.logger).Log("msg", "error while reading node address length from packet", "err", err, "remote", conn.RemoteAddr())
			return
		}

		addrBuf := make([]byte, addrLengthBuf[0])
		_, err = io.ReadFull(str, addrBuf)
		if err != nil {
			t.receivedPacketsErrors.Inc()
			level.Warn(t.logger).Log("msg", "error while reading node address from packet", "err", err, "remote", conn.RemoteAddr())
			return
		}

		// read the rest to buffer -- this is the "packet" itself
		buf, err := io.ReadAll(str)
		if err != nil {
			t.receivedPacketsErrors.Inc()
			level.Warn(t.logger).Log("msg", "error while reading packet data", "err", err, "remote", conn.RemoteAddr())
			return
		}

		if len(buf) < md5.Size {
			t.receivedPacketsErrors.Inc()
			level.Warn(t.logger).Log("msg", "not enough data received", "data_length", len(buf), "remote", conn.RemoteAddr())
			return
		}

		receivedDigest := buf[len(buf)-md5.Size:]
		buf = buf[:len(buf)-md5.Size]

		expectedDigest := md5.Sum(buf)

		if !bytes.Equal(receivedDigest, expectedDigest[:]) {
			t.receivedPacketsErrors.Inc()
			level.Warn(t.logger).Log("msg", "packet digest mismatch", "expected", fmt.Sprintf("%x", expectedDigest), "received", fmt.Sprintf("%x", receivedDigest), "data_length", len(buf), "remote", conn.RemoteAddr())
		}

		t.debugLog().Log("msg", "Received packet", "addr", addr(addrBuf), "size", len(buf), "hash", fmt.Sprintf("%x", receivedDigest))

		t.receivedPacketsBytes.Add(float64(len(buf)))

		t.packetCh <- &memberlist.Packet{
			Buf:       buf,
			From:      addr(addrBuf),
			Timestamp: time.Now(),
		}
	} else {
		t.unknownConnections.Inc()
		level.Error(t.logger).Log("msg", "unknown message type", "msgType", msgType, "remote", conn.RemoteAddr())
	}
}

func (t *QuicTransport) getConnection(addr string, timeout time.Duration) (net.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "udp", addr, t.tlsConfig)
}

// GetAutoBindPort returns the bind port that was automatically given by the
// kernel, if a bind port of 0 was given.
func (t *QuicTransport) GetAutoBindPort() int {
	// We made sure there's at least one Quic listener, and that one's
	// port was applied to all the others for the dynamic bind case.
	return t.quicListeners[0].Addr().(*net.UDPAddr).Port
}

// FinalAdvertiseAddr is given the user's configured values (which
// might be empty) and returns the desired IP and port to advertise to
// the rest of the cluster.
// (Copied from memberlist' net_transport.go)
func (t *QuicTransport) FinalAdvertiseAddr(ip string, port int) (net.IP, int, error) {
	var advertiseAddr net.IP
	var advertisePort int
	if ip != "" {
		// If they've supplied an address, use that.
		advertiseAddr = net.ParseIP(ip)
		if advertiseAddr == nil {
			return nil, 0, fmt.Errorf("failed to parse advertise address %q", ip)
		}

		advertisePort = port
	} else {

		switch t.cfg.BindAddrs[0] {
		case zeroZeroZeroZero:
			// Otherwise, if we're not bound to a specific IP, let's
			// use a suitable private IP address.
			var err error
			ip, err = sockaddr.GetPrivateIP()
			if err != nil {
				return nil, 0, fmt.Errorf("failed to get interface addresses: %v", err)
			}
			if ip == "" {
				return nil, 0, fmt.Errorf("no private IP address found, and explicit IP not provided")
			}

			advertiseAddr = net.ParseIP(ip)
			if advertiseAddr == nil {
				return nil, 0, fmt.Errorf("failed to parse advertise address %q", ip)
			}
		case colonColon:
			inet6Ip, err := netutil.GetFirstAddressOf(nil, t.logger, true)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to get private inet6 address: %w", err)
			}

			advertiseAddr = net.ParseIP(inet6Ip)
			if advertiseAddr == nil {
				return nil, 0, fmt.Errorf("failed to parse inet6 advertise address %q", ip)
			}
		default:
			// Use the IP that we're bound to, based on the first
			// Quic listener, which we already ensure is there.
			advertiseAddr = t.quicListeners[0].Addr().(*net.UDPAddr).IP
		}

		// Use the port we are bound to.
		advertisePort = t.GetAutoBindPort()
	}

	level.Debug(t.logger).Log("msg", "FinalAdvertiseAddr", "advertiseAddr", advertiseAddr.String(), "advertisePort", advertisePort)

	t.setAdvertisedAddr(advertiseAddr, advertisePort)
	return advertiseAddr, advertisePort, nil
}

func (t *QuicTransport) setAdvertisedAddr(advertiseAddr net.IP, advertisePort int) {
	t.advertiseMu.Lock()
	defer t.advertiseMu.Unlock()
	addr := net.UDPAddr{IP: advertiseAddr, Port: advertisePort}
	t.advertiseAddr = addr.String()
}

func (t *QuicTransport) getAdvertisedAddr() string {
	t.advertiseMu.RLock()
	defer t.advertiseMu.RUnlock()
	return t.advertiseAddr
}

// WriteTo is a packet-oriented interface that fires off the given
// payload to the given address.
func (t *QuicTransport) WriteTo(b []byte, addr string) (time.Time, error) {
	t.sentPackets.Inc()
	t.sentPacketsBytes.Add(float64(len(b)))

	err := t.writeTo(b, addr)
	if err != nil {
		t.sentPacketsErrors.Inc()

		logLevel := level.Warn(t.logger)
		if strings.Contains(err.Error(), "connection refused") {
			// The connection refused is a common error that could happen during normal operations when a node
			// shutdown (or crash). It shouldn't be considered a warning condition on the sender side.
			logLevel = t.debugLog()
		}
		logLevel.Log("msg", "WriteTo failed", "addr", addr, "err", err)

		// WriteTo is used to send "UDP" packets. Since we use Quic, we can detect more errors,
		// but memberlist library doesn't seem to cope with that very well. That is why we return nil instead.
		return time.Now(), nil
	}

	return time.Now(), nil
}

func (t *QuicTransport) writeTo(b []byte, addr string) error {
	// Open connection, write packet header and data, data hash, close. Simple.
	c, err := t.getConnection(addr, t.cfg.PacketDialTimeout)
	if err != nil {
		return err
	}

	closed := false
	defer func() {
		if !closed {
			// If we still need to close, then there was another error. Ignore this one.
			_ = c.Close()
		}
	}()

	// Compute the digest *before* setting the deadline on the connection (so that the time
	// it takes to compute the digest is not taken in account).
	// We use md5 as quick and relatively short hash, not in cryptographic context.
	// It's also used to detect if the whole packet has been received on the receiver side.
	digest := md5.Sum(b)

	// Prepare the header *before* setting the deadline on the connection.
	headerBuf := bytes.Buffer{}
	headerBuf.WriteByte(byte(packet))

	// We need to send our address to the other side, otherwise other side can only see IP and port from Quic header.
	// But that doesn't match our node address (new Quic connection has new random port), which confuses memberlist.
	// So we send our advertised address, so that memberlist on the receiving side can match it with correct node.
	// This seems to be important for node probes (pings) done by memberlist.
	ourAddr := t.getAdvertisedAddr()
	if len(ourAddr) > 255 {
		return fmt.Errorf("local address too long")
	}

	headerBuf.WriteByte(byte(len(ourAddr)))
	headerBuf.WriteString(ourAddr)

	if t.cfg.PacketWriteTimeout > 0 {
		deadline := time.Now().Add(t.cfg.PacketWriteTimeout)
		err := c.SetDeadline(deadline)
		if err != nil {
			return fmt.Errorf("setting deadline: %v", err)
		}
	}

	_, err = c.Write(headerBuf.Bytes())
	if err != nil {
		return fmt.Errorf("sending local address: %v", err)
	}

	n, err := c.Write(b)
	if err != nil {
		return fmt.Errorf("sending data: %v", err)
	}
	if n != len(b) {
		return fmt.Errorf("sending data: short write")
	}

	// Append digest.
	n, err = c.Write(digest[:])
	if err != nil {
		return fmt.Errorf("digest: %v", err)
	}
	if n != len(digest) {
		return fmt.Errorf("digest: short write")
	}

	closed = true
	err = c.Close()
	if err != nil {
		return fmt.Errorf("close: %v", err)
	}

	t.debugLog().Log("msg", "WriteTo: packet sent", "addr", addr, "size", len(b), "hash", fmt.Sprintf("%x", digest))
	return nil
}

// PacketCh returns a channel that can be read to receive incoming
// packets from other peers.
func (t *QuicTransport) PacketCh() <-chan *memberlist.Packet {
	return t.packetCh
}

// DialTimeout is used to create a connection that allows memberlist to perform
// two-way communication with a peer.
func (t *QuicTransport) DialTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	t.outgoingStreams.Inc()
	c, err := t.getConnection(addr, timeout)
	if err != nil {
		t.outgoingStreamErrors.Inc()
		return nil, err
	}

	_, err = c.Write([]byte{byte(stream)})
	if err != nil {
		t.outgoingStreamErrors.Inc()
		_ = c.Close()
		return nil, err
	}

	return c, nil
}

// StreamCh returns a channel that can be read to handle incoming stream
// connections from other peers.
func (t *QuicTransport) StreamCh() <-chan quic.Connection {
	return t.connCh
}

// Shutdown is called when memberlist is shutting down; this gives the
// transport a chance to clean up any listeners.
func (t *QuicTransport) Shutdown() error {
	// This will avoid log spam about errors when we shut down.
	t.shutdown.Store(1)

	// Rip through all the connections and shut them down.
	for _, conn := range t.quicListeners {
		_ = conn.Close()
	}

	// Block until all the listener threads have died.
	t.wg.Wait()
	return nil
}

func (t *QuicTransport) registerMetrics(registerer prometheus.Registerer) {
	const subsystem = "memberlist_quic_transport"

	t.incomingStreams = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "incoming_streams_total",
		Help:      "Number of incoming memberlist streams",
	})

	t.outgoingStreams = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "outgoing_streams_total",
		Help:      "Number of outgoing streams",
	})

	t.outgoingStreamErrors = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "outgoing_stream_errors_total",
		Help:      "Number of errors when opening memberlist stream to another node",
	})

	t.receivedPackets = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "packets_received_total",
		Help:      "Number of received memberlist packets",
	})

	t.receivedPacketsBytes = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "packets_received_bytes_total",
		Help:      "Total bytes received as packets",
	})

	t.receivedPacketsErrors = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "packets_received_errors_total",
		Help:      "Number of errors when receiving memberlist packets",
	})

	t.sentPackets = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "packets_sent_total",
		Help:      "Number of memberlist packets sent",
	})

	t.sentPacketsBytes = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "packets_sent_bytes_total",
		Help:      "Total bytes sent as packets",
	})

	t.sentPacketsErrors = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "packets_sent_errors_total",
		Help:      "Number of errors when sending memberlist packets",
	})

	t.unknownConnections = promauto.With(registerer).NewCounter(prometheus.CounterOpts{
		Namespace: t.cfg.MetricsNamespace,
		Subsystem: subsystem,
		Name:      "unknown_connections_total",
		Help:      "Number of unknown Quic connections (not a packet or stream)",
	})
}