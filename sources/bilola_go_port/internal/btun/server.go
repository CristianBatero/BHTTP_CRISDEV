package btun

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type PacketDevice interface {
	io.ReadWriteCloser
	Name() string
}

type Config struct {
	Subnet           string
	Authenticator    Authenticator
	Logger           *log.Logger
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	MaxPacketSize    int
	MaxCoverBytes    int
}

type StatsSnapshot struct {
	StartedAt       time.Time `json:"startedAt"`
	ActiveSessions  uint64    `json:"activeSessions"`
	Accepted        uint64    `json:"accepted"`
	AuthSucceeded   uint64    `json:"authSucceeded"`
	AuthRejected    uint64    `json:"authRejected"`
	ClientPackets   uint64    `json:"clientPackets"`
	InternetPackets uint64    `json:"internetPackets"`
	ClientBytes     uint64    `json:"clientBytes"`
	InternetBytes   uint64    `json:"internetBytes"`
	DroppedPackets  uint64    `json:"droppedPackets"`
}

type counters struct {
	activeSessions  atomic.Uint64
	accepted        atomic.Uint64
	authSucceeded   atomic.Uint64
	authRejected    atomic.Uint64
	clientPackets   atomic.Uint64
	internetPackets atomic.Uint64
	clientBytes     atomic.Uint64
	internetBytes   atomic.Uint64
	droppedPackets  atomic.Uint64
}

type Server struct {
	config  Config
	tun     PacketDevice
	pool    *AddressPool
	started time.Time
	stats   counters

	mu        sync.RWMutex
	sessions  map[*session]struct{}
	byIP      map[[4]byte]*session
	udpByAddr map[string]*session
	closers   []io.Closer

	tunWriteMu sync.Mutex
	closed     chan struct{}
	closeOnce  sync.Once
	wait       sync.WaitGroup
}

type session struct {
	server    *Server
	transport sessionTransport
	udpKey    string
	user      string

	mu            sync.Mutex
	authenticated bool
	assignedIP    net.IP
	lastActive    time.Time
	handleMu      sync.Mutex
	closeOnce     sync.Once
	invalidFrames uint64
}

type sessionTransport interface {
	Send(Packet, int) (int, error)
	Close() error
	Remote() string
}

type tcpTransport struct {
	connection net.Conn
	writeMu    sync.Mutex
}

func (transport *tcpTransport) Send(packet Packet, maxPayload int) (int, error) {
	encoded, err := MarshalPacket(packet, maxPayload)
	if err != nil {
		return 0, err
	}
	transport.writeMu.Lock()
	defer transport.writeMu.Unlock()
	return len(encoded), writeFull(transport.connection, encoded)
}

func (transport *tcpTransport) Close() error   { return transport.connection.Close() }
func (transport *tcpTransport) Remote() string { return transport.connection.RemoteAddr().String() }

type udpTransport struct {
	connection net.PacketConn
	address    net.Addr
	writeMu    sync.Mutex
}

func (transport *udpTransport) Send(packet Packet, maxPayload int) (int, error) {
	encoded, err := MarshalPacket(packet, maxPayload)
	if err != nil {
		return 0, err
	}
	transport.writeMu.Lock()
	defer transport.writeMu.Unlock()
	written, err := transport.connection.WriteTo(encoded, transport.address)
	if err == nil && written != len(encoded) {
		err = io.ErrShortWrite
	}
	return written, err
}

func (transport *udpTransport) Close() error   { return nil }
func (transport *udpTransport) Remote() string { return transport.address.String() }

func NewServer(config Config, tun PacketDevice) (*Server, error) {
	if tun == nil {
		return nil, errors.New("TUN device is required")
	}
	if config.Authenticator == nil {
		return nil, errors.New("authenticator is required")
	}
	if config.Subnet == "" {
		config.Subnet = "10.77.0.0/16"
	}
	if config.Logger == nil {
		config.Logger = log.Default()
	}
	if config.HandshakeTimeout <= 0 {
		config.HandshakeTimeout = 15 * time.Second
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = 2 * time.Minute
	}
	if config.MaxPacketSize <= 0 {
		config.MaxPacketSize = DefaultMaxPacket
	}
	if config.MaxPacketSize > 4*1024*1024 {
		return nil, errors.New("maximum packet size cannot exceed 4 MiB")
	}
	if config.MaxCoverBytes <= 0 {
		config.MaxCoverBytes = 64 * 1024
	}
	pool, err := NewAddressPool(config.Subnet)
	if err != nil {
		return nil, err
	}
	server := &Server{
		config: config, tun: tun, pool: pool, started: time.Now(),
		sessions: make(map[*session]struct{}), byIP: make(map[[4]byte]*session),
		udpByAddr: make(map[string]*session), closed: make(chan struct{}),
	}
	server.wait.Add(2)
	go server.tunLoop()
	go server.cleanupLoop()
	return server, nil
}

func (server *Server) ServeTCP(listener net.Listener) error {
	server.addCloser(listener)
	server.config.Logger.Printf("BTUN TCP listening on %s", listener.Addr())
	for {
		connection, err := listener.Accept()
		if err != nil {
			if server.isClosed() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		server.stats.accepted.Add(1)
		if tcp, ok := connection.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
			_ = tcp.SetKeepAlive(true)
			_ = tcp.SetKeepAlivePeriod(30 * time.Second)
		}
		server.wait.Add(1)
		go func() {
			defer server.wait.Done()
			if err := server.handleTCP(connection); err != nil && !normalClose(err) {
				server.config.Logger.Printf("BTUN TCP %s: %v", connection.RemoteAddr(), err)
			}
		}()
	}
}

func (server *Server) handleTCP(connection net.Conn) error {
	transport := &tcpTransport{connection: connection}
	current := server.newSession(transport, "")
	defer current.close()
	_ = connection.SetDeadline(time.Now().Add(server.config.HandshakeTimeout))
	coverBytes, err := ReadClientHello(connection, server.config.MaxCoverBytes)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if coverBytes > 0 {
		server.config.Logger.Printf("BTUN TCP %s accepted %d cover bytes", transport.Remote(), coverBytes)
	}
	if err := writeFull(connection, ServerHello); err != nil {
		return fmt.Errorf("write server hello: %w", err)
	}
	authPacket, err := ReadPacket(connection, server.config.MaxPacketSize)
	if err != nil {
		return fmt.Errorf("read authentication: %w", err)
	}
	if err := server.authenticate(current, authPacket); err != nil {
		return err
	}
	_ = connection.SetDeadline(time.Time{})
	for {
		if server.config.IdleTimeout > 0 {
			_ = connection.SetReadDeadline(time.Now().Add(server.config.IdleTimeout))
		}
		packet, err := ReadPacket(connection, server.config.MaxPacketSize)
		if err != nil {
			return err
		}
		if err := server.handleClientPacket(current, packet); err != nil {
			return err
		}
	}
}

func (server *Server) ServeUDP(connection net.PacketConn) error {
	server.addCloser(connection)
	server.config.Logger.Printf("BTUN UDP listening on %s", connection.LocalAddr())
	buffer := make([]byte, server.config.MaxPacketSize+HeaderSize)
	for {
		length, address, err := connection.ReadFrom(buffer)
		if err != nil {
			if server.isClosed() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		datagram := append([]byte(nil), buffer[:length]...)
		if bytes.Equal(datagram, ClientHello) {
			server.startUDPSession(connection, address)
			continue
		}
		server.mu.RLock()
		current := server.udpByAddr[address.String()]
		server.mu.RUnlock()
		if current == nil {
			server.stats.droppedPackets.Add(1)
			continue
		}
		packet, err := ParseDatagram(datagram, server.config.MaxPacketSize)
		if err != nil {
			server.stats.droppedPackets.Add(1)
			continue
		}
		current.handleMu.Lock()
		if !current.isAuthenticated() {
			err = server.authenticate(current, packet)
		} else {
			err = server.handleClientPacket(current, packet)
		}
		current.handleMu.Unlock()
		if err != nil {
			server.config.Logger.Printf("BTUN UDP %s: %v", address, err)
			current.close()
		}
	}
}

func (server *Server) startUDPSession(connection net.PacketConn, address net.Addr) {
	server.stats.accepted.Add(1)
	key := address.String()
	transport := &udpTransport{connection: connection, address: address}
	current := server.newSession(transport, key)
	server.mu.Lock()
	previous := server.udpByAddr[key]
	server.udpByAddr[key] = current
	server.mu.Unlock()
	if previous != nil && previous != current {
		previous.close()
	}
	if _, err := connection.WriteTo(ServerHello, address); err != nil {
		current.close()
		return
	}
	server.config.Logger.Printf("BTUN UDP handshake from %s", address)
}

func (server *Server) authenticate(current *session, packet Packet) error {
	if packet.Type != PacketAuth || packet.Flags != 0 {
		server.stats.authRejected.Add(1)
		_ = current.send(AuthResponse(false, "authentication required"))
		return errors.New("authentication packet required")
	}
	username, password, err := ParseCredential(packet.Payload)
	if err != nil {
		server.stats.authRejected.Add(1)
		_ = current.send(AuthResponse(false, "invalid credential"))
		return err
	}
	if err := server.config.Authenticator.Authenticate(username, password); err != nil {
		server.stats.authRejected.Add(1)
		_ = current.send(AuthResponse(false, "invalid username or password"))
		return errors.New("authentication rejected")
	}
	current.mu.Lock()
	current.authenticated = true
	current.user = username
	current.lastActive = time.Now()
	current.mu.Unlock()
	server.stats.authSucceeded.Add(1)
	if err := current.send(AuthResponse(true, "authenticated")); err != nil {
		return fmt.Errorf("write authentication response: %w", err)
	}
	server.config.Logger.Printf("BTUN %s authenticated user=%s via %s", current.transport.Remote(), username, server.config.Authenticator.Name())
	return nil
}

func (server *Server) handleClientPacket(current *session, packet Packet) error {
	current.touch()
	if packet.Flags != 0 {
		return errors.New("unsupported packet flags")
	}
	switch packet.Type {
	case PacketIP:
		if len(packet.Payload) != 0 {
			return errors.New("invalid IP request")
		}
		ip, err := server.assignIP(current)
		if err != nil {
			return err
		}
		return current.send(Packet{Type: PacketIP, Payload: append([]byte(nil), ip.To4()...)})
	case PacketData:
		source, _, totalLength, ok := parseIPv4Packet(packet.Payload)
		assigned, assignedOK := current.ipKey()
		if !ok {
			server.stats.droppedPackets.Add(1)
			server.logDroppedFrame(current, "invalid IPv4", packet.Payload, assigned, assignedOK)
			return nil
		}
		if !assignedOK || source != assigned {
			server.stats.droppedPackets.Add(1)
			server.logDroppedFrame(current, "source mismatch", packet.Payload, assigned, assignedOK)
			return nil
		}
		payload := packet.Payload[:totalLength]
		server.tunWriteMu.Lock()
		written, err := server.tun.Write(payload)
		server.tunWriteMu.Unlock()
		if err != nil {
			return fmt.Errorf("write TUN: %w", err)
		}
		if written != len(payload) {
			return io.ErrShortWrite
		}
		server.stats.clientPackets.Add(1)
		server.stats.clientBytes.Add(uint64(written))
		return nil
	case PacketKeepAlive:
		if len(packet.Payload) != 0 {
			return errors.New("invalid keep-alive packet")
		}
		return nil
	default:
		return fmt.Errorf("unknown packet type %d", packet.Type)
	}
}

func (server *Server) logDroppedFrame(current *session, reason string, payload []byte,
	assigned [4]byte, assignedOK bool) {
	current.mu.Lock()
	current.invalidFrames++
	count := current.invalidFrames
	current.mu.Unlock()
	if count > 3 {
		return
	}
	version := byte(0)
	declared := 0
	if len(payload) > 0 {
		version = payload[0] >> 4
	}
	if len(payload) >= 4 {
		declared = int(binary.BigEndian.Uint16(payload[2:4]))
	}
	source := "n/a"
	if len(payload) >= 16 {
		source = net.IP(payload[12:16]).String()
	}
	assignedText := "none"
	if assignedOK {
		assignedText = net.IP(assigned[:]).String()
	}
	server.config.Logger.Printf(
		"BTUN %s dropped data frame (%s): frameLen=%d ipVersion=%d ipLen=%d source=%s assigned=%s",
		current.transport.Remote(), reason, len(payload), version, declared, source, assignedText)
}

func (server *Server) assignIP(current *session) (net.IP, error) {
	current.mu.Lock()
	if current.assignedIP != nil {
		ip := append(net.IP(nil), current.assignedIP...)
		current.mu.Unlock()
		return ip, nil
	}
	current.mu.Unlock()
	ip, err := server.pool.Acquire()
	if err != nil {
		return nil, err
	}
	key, _ := ipKey(ip)
	server.mu.Lock()
	if existing := server.byIP[key]; existing != nil && existing != current {
		server.mu.Unlock()
		server.pool.Release(ip)
		return nil, errors.New("address collision")
	}
	current.mu.Lock()
	if current.assignedIP == nil {
		current.assignedIP = append(net.IP(nil), ip.To4()...)
		server.byIP[key] = current
	} else {
		server.pool.Release(ip)
		ip = append(net.IP(nil), current.assignedIP...)
	}
	current.mu.Unlock()
	server.mu.Unlock()
	server.config.Logger.Printf("BTUN %s assigned %s", current.transport.Remote(), ip)
	return ip, nil
}

func (server *Server) tunLoop() {
	defer server.wait.Done()
	buffer := make([]byte, server.config.MaxPacketSize)
	for {
		length, err := server.tun.Read(buffer)
		if err != nil {
			if !server.isClosed() && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				server.config.Logger.Printf("BTUN TUN read stopped: %v", err)
			}
			return
		}
		packet := append([]byte(nil), buffer[:length]...)
		_, destination, ok := ParseIPv4Packet(packet)
		if !ok {
			server.stats.droppedPackets.Add(1)
			continue
		}
		server.mu.RLock()
		current := server.byIP[destination]
		server.mu.RUnlock()
		if current == nil {
			server.stats.droppedPackets.Add(1)
			continue
		}
		if err := current.send(Packet{Type: PacketData, Payload: packet}); err != nil {
			current.close()
			continue
		}
		server.stats.internetPackets.Add(1)
		server.stats.internetBytes.Add(uint64(length))
	}
}

func (server *Server) cleanupLoop() {
	defer server.wait.Done()
	interval := server.config.IdleTimeout / 2
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			server.mu.RLock()
			candidates := make([]*session, 0, len(server.sessions))
			for current := range server.sessions {
				candidates = append(candidates, current)
			}
			server.mu.RUnlock()
			for _, current := range candidates {
				if now.Sub(current.lastSeen()) > server.config.IdleTimeout {
					current.close()
				}
			}
		case <-server.closed:
			return
		}
	}
}

func (server *Server) newSession(transport sessionTransport, udpKey string) *session {
	current := &session{server: server, transport: transport, udpKey: udpKey, lastActive: time.Now()}
	server.mu.Lock()
	server.sessions[current] = struct{}{}
	server.mu.Unlock()
	server.stats.activeSessions.Add(1)
	return current
}

func (current *session) send(packet Packet) error {
	written, err := current.transport.Send(packet, current.server.config.MaxPacketSize)
	if err == nil {
		current.touch()
		_ = written
	}
	return err
}

func (current *session) touch() {
	current.mu.Lock()
	current.lastActive = time.Now()
	current.mu.Unlock()
}

func (current *session) lastSeen() time.Time {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.lastActive
}

func (current *session) isAuthenticated() bool {
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.authenticated
}

func (current *session) ipKey() ([4]byte, bool) {
	current.mu.Lock()
	defer current.mu.Unlock()
	return ipKey(current.assignedIP)
}

func (current *session) close() {
	current.closeOnce.Do(func() {
		_ = current.transport.Close()
		server := current.server
		server.mu.Lock()
		delete(server.sessions, current)
		if current.udpKey != "" && server.udpByAddr[current.udpKey] == current {
			delete(server.udpByAddr, current.udpKey)
		}
		current.mu.Lock()
		assigned := append(net.IP(nil), current.assignedIP...)
		current.assignedIP = nil
		current.mu.Unlock()
		if key, ok := ipKey(assigned); ok && server.byIP[key] == current {
			delete(server.byIP, key)
		}
		server.mu.Unlock()
		if assigned != nil {
			server.pool.Release(assigned)
		}
		server.stats.activeSessions.Add(^uint64(0))
	})
}

func (server *Server) addCloser(closer io.Closer) {
	server.mu.Lock()
	server.closers = append(server.closers, closer)
	server.mu.Unlock()
}

func (server *Server) isClosed() bool {
	select {
	case <-server.closed:
		return true
	default:
		return false
	}
}

func (server *Server) Close() error {
	var closeError error
	server.closeOnce.Do(func() {
		close(server.closed)
		server.mu.RLock()
		closers := append([]io.Closer(nil), server.closers...)
		sessions := make([]*session, 0, len(server.sessions))
		for current := range server.sessions {
			sessions = append(sessions, current)
		}
		server.mu.RUnlock()
		for _, closer := range closers {
			if err := closer.Close(); err != nil && closeError == nil && !errors.Is(err, net.ErrClosed) {
				closeError = err
			}
		}
		for _, current := range sessions {
			current.close()
		}
		if err := server.tun.Close(); err != nil && closeError == nil && !errors.Is(err, os.ErrClosed) {
			closeError = err
		}
	})
	return closeError
}

func (server *Server) Wait() { server.wait.Wait() }

func (server *Server) Stats() StatsSnapshot {
	return StatsSnapshot{
		StartedAt: server.started, ActiveSessions: server.stats.activeSessions.Load(),
		Accepted: server.stats.accepted.Load(), AuthSucceeded: server.stats.authSucceeded.Load(),
		AuthRejected: server.stats.authRejected.Load(), ClientPackets: server.stats.clientPackets.Load(),
		InternetPackets: server.stats.internetPackets.Load(), ClientBytes: server.stats.clientBytes.Load(),
		InternetBytes: server.stats.internetBytes.Load(), DroppedPackets: server.stats.droppedPackets.Load(),
	}
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func normalClose(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "connection reset") || strings.Contains(message, "broken pipe")
}
