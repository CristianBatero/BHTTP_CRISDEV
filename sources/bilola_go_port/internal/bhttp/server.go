package bhttp

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

type Config struct {
	TargetAddress  string
	SessionTimeout time.Duration
	DialTimeout    time.Duration
	MaxV2Lanes     int
	Logger         *log.Logger
	DialTarget     func(string) (net.Conn, error)
}

type Server struct {
	config   Config
	sessions *SessionManager
	closed   chan struct{}
	once     sync.Once
	wait     sync.WaitGroup
	connMu   sync.Mutex
	active   map[net.Conn]struct{}
}

const (
	bhttpV2Version       = 2
	bhttpV2FeatureLanes  = 1 << 0
	bhttpV2FeatureResume = 1 << 1
	bhttpV2FeatureAck    = 1 << 2
	bhttpV2MaxLanes      = 128
	laneUpload           = 1
	laneDownload         = 2
	laneDuplex           = 3
)

func NewServer(config Config) *Server {
	if config.TargetAddress == "" {
		config.TargetAddress = "127.0.0.1:22"
	}
	if config.SessionTimeout <= 0 {
		config.SessionTimeout = 2 * time.Minute
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = 10 * time.Second
	}
	if config.MaxV2Lanes <= 0 {
		config.MaxV2Lanes = bhttpV2MaxLanes
	}
	if config.Logger == nil {
		config.Logger = log.Default()
	}
	if config.DialTarget == nil {
		dialer := net.Dialer{Timeout: config.DialTimeout, KeepAlive: 30 * time.Second}
		config.DialTarget = func(address string) (net.Conn, error) {
			connection, err := dialer.Dial("tcp", address)
			if err == nil {
				if tcp, ok := connection.(*net.TCPConn); ok {
					_ = tcp.SetNoDelay(true)
				}
			}
			return connection, err
		}
	}
	server := &Server{
		config:   config,
		sessions: NewSessionManager(config.SessionTimeout),
		closed:   make(chan struct{}),
		active:   make(map[net.Conn]struct{}),
	}
	server.wait.Add(1)
	go server.cleanupLoop()
	return server
}

func (server *Server) cleanupLoop() {
	defer server.wait.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			removed := server.sessions.Cleanup(now)
			if removed > 0 {
				server.config.Logger.Printf("cleaned %d stale BHTTP sessions", removed)
			}
		case <-server.closed:
			return
		}
	}
}

func (server *Server) Serve(listener net.Listener) error {
	for {
		connection, err := listener.Accept()
		if err != nil {
			select {
			case <-server.closed:
				return nil
			default:
				return err
			}
		}
		if tcp, ok := connection.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		server.connMu.Lock()
		server.active[connection] = struct{}{}
		server.connMu.Unlock()
		server.wait.Add(1)
		go func() {
			defer server.wait.Done()
			defer func() {
				server.connMu.Lock()
				delete(server.active, connection)
				server.connMu.Unlock()
				_ = connection.Close()
			}()
			if err := server.handleConnection(connection); err != nil && !isNormalClose(err) {
				server.config.Logger.Printf("client %s: %v", connection.RemoteAddr(), err)
			}
		}()
	}
}

func isNormalClose(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "connection reset")
}

func (server *Server) handleConnection(connection net.Conn) error {
	reader := bufio.NewReader(connection)
	first, err := reader.Peek(1)
	if err != nil {
		return err
	}
	if first[0] == ModeProbe {
		request, err := ReadRequest(reader)
		if err != nil {
			return err
		}
		return handleProbeLimit(connection, request, server.config.MaxV2Lanes)
	}
	if first[0] > ModeLane {
		if err := handleHTTPUpgrade(reader, connection); err != nil {
			return err
		}
	}
	var laneRole byte
	var laneLease *LaneLease
	defer func() {
		if laneLease != nil {
			laneLease.ReleaseLane()
		}
	}()
	for {
		request, err := ReadRequest(reader)
		if err != nil {
			return err
		}
		if request.Mode == ModeLane {
			if laneLease != nil {
				return errors.New("BHTTP v2 lane already attached")
			}
			role, attached, attachErr := server.attachLane(connection, request)
			if attachErr != nil {
				return attachErr
			}
			laneRole = role
			laneLease = attached
			continue
		}
		if laneRole == laneUpload && request.Mode != ModeUpload {
			return errors.New("BHTTP v2 upload lane received non-upload request")
		}
		if laneRole == laneDownload && request.Mode != ModeDownload &&
			request.Mode != ModeBatch && request.Mode != ModeACK {
			return errors.New("BHTTP v2 download lane received invalid request")
		}
		session := server.sessions.GetOrCreate(request.SID)
		session.touch()
		if err := server.handleRequest(connection, session, request, laneRole != 0); err != nil {
			return err
		}
	}
}

func handleHTTPUpgrade(reader *bufio.Reader, writer io.Writer) error {
	var request bytes.Buffer
	for request.Len() < 64*1024 {
		value, err := reader.ReadByte()
		if err != nil {
			return err
		}
		request.WriteByte(value)
		if bytes.HasSuffix(request.Bytes(), []byte("\r\n\r\n")) {
			return writeAll(writer, []byte("HTTP/1.1 101 DTunnel\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nHTTP/1.1 200 DTunnel\r\n\r\n"))
		}
	}
	return errors.New("HTTP upgrade header is too large")
}

func handleProbe(writer io.Writer, request Request) error {
	return handleProbeLimit(writer, request, bhttpV2MaxLanes)
}

func handleProbeLimit(writer io.Writer, request Request, maxLanes int) error {
	clear := Crypt(request.Payload, request.SID, ModeProbe, request.Seq, false)
	if len(clear) == 10 && bytes.Equal(clear[:4], []byte("BHP2")) && clear[4] == bhttpV2Version {
		response := make([]byte, 14)
		copy(response[:4], []byte("BHP2"))
		response[4] = bhttpV2Version
		binary.BigEndian.PutUint32(response[6:10], bhttpV2FeatureLanes|bhttpV2FeatureResume|bhttpV2FeatureAck)
		binary.BigEndian.PutUint32(response[10:14], uint32(maxLanes))
		return WriteStatus(writer, StatusOK, Crypt(response, request.SID, ModeProbe, request.Seq, true))
	}
	if len(clear) < 10 || !bytes.Equal(clear[:4], []byte("BHP1")) || clear[4] != 1 {
		return WriteStatus(writer, StatusError, []byte("invalid BHTTP v1 probe"))
	}
	submode := clear[5]
	parameter := int(binary.BigEndian.Uint32(clear[6:10]))
	if submode > ModeACK {
		return WriteStatus(writer, StatusError, []byte("unsupported BHTTP probe mode"))
	}
	expected := 10
	if submode == ModeUpload && parameter >= 10 {
		expected = parameter
	}
	if len(clear) != expected {
		return WriteStatus(writer, StatusError, []byte("invalid BHTTP v1 probe length"))
	}
	for index := 10; index < len(clear); index++ {
		if clear[index] != byte(index*31) {
			return WriteStatus(writer, StatusError, []byte("invalid BHTTP v1 probe pattern"))
		}
	}
	responseSize := 10
	if submode == ModeDownload && parameter > responseSize {
		responseSize = parameter
	}
	if responseSize > MaxPayload {
		return WriteStatus(writer, StatusError, []byte("BHTTP v1 probe too large"))
	}
	response := make([]byte, responseSize)
	copy(response[:4], []byte("BHP1"))
	response[4] = 1
	response[5] = submode
	binary.BigEndian.PutUint32(response[6:10], uint32(parameter))
	for index := 10; index < len(response); index++ {
		response[index] = byte(index * 31)
	}
	encrypted := Crypt(response, request.SID, ModeProbe, request.Seq, true)
	count := 1
	if submode == ModeACK {
		count = parameter
		if count < 1 {
			count = 1
		}
		if count > 256 {
			count = 256
		}
	}
	for index := 0; index < count; index++ {
		if err := WriteStatus(writer, StatusOK, encrypted); err != nil {
			return err
		}
	}
	return nil
}

func (server *Server) handleRequest(writer io.Writer, session *Session, request Request, version2 bool) error {
	switch request.Mode {
	case ModeUpload:
		payload := request.Payload
		if len(payload) > 0 {
			payload = Crypt(payload, request.SID, request.Mode, request.Seq, false)
		}
		if request.Seq == 0 && len(payload) == 0 {
			if err := session.OpenTarget(server.config.TargetAddress, server.config.DialTarget); err != nil {
				return WriteStatus(writer, StatusError, []byte("SSH target unavailable"))
			}
			return WriteStatus(writer, StatusOK, nil)
		}
		if err := session.OpenTarget(server.config.TargetAddress, server.config.DialTarget); err != nil {
			return WriteStatus(writer, StatusError, []byte("SSH target unavailable"))
		}
		if err := session.WriteUpload(request.Seq, payload); err != nil {
			return WriteStatus(writer, StatusError, []byte("SSH upload failed"))
		}
		if !version2 {
			return WriteStatus(writer, StatusOK, nil)
		}
		ack := make([]byte, 9)
		if sequence, ok := session.UploadCommitted(); ok {
			ack[0] = 1
			binary.BigEndian.PutUint64(ack[1:], sequence)
		}
		return WriteStatus(writer, StatusOK, ack)

	case ModeDownload:
		chunks := session.AssignDownload(request.Seq, 1, 65536)
		return WriteDownload(writer, request.SID, request.Mode, request.Seq, chunks[0])

	case ModeBatch:
		clear := Crypt(request.Payload, request.SID, request.Mode, request.Seq, false)
		if len(clear) != 6 {
			return WriteStatus(writer, StatusError, []byte("invalid BHTTP batch request"))
		}
		limit := int(binary.BigEndian.Uint32(clear[:4]))
		count := int(binary.BigEndian.Uint16(clear[4:6]))
		if limit < 1 {
			limit = 1
		}
		if limit > MaxDownloadSize {
			limit = MaxDownloadSize
		}
		if count < 1 {
			count = 1
		}
		if count > 256 {
			count = 256
		}
		chunks := session.AssignDownload(request.Seq, count, limit)
		for offset, chunk := range chunks {
			if err := WriteDownload(writer, request.SID, request.Mode, request.Seq+uint64(offset), chunk); err != nil {
				return err
			}
		}
		return nil

	case ModeACK:
		session.Acknowledge(request.Seq)
		return WriteStatus(writer, StatusOK, nil)
	}
	return fmt.Errorf("unsupported BHTTP mode %d", request.Mode)
}

func (server *Server) attachLane(writer io.Writer, request Request) (byte, *LaneLease, error) {
	clear := Crypt(request.Payload, request.SID, ModeLane, request.Seq, false)
	if len(clear) != 12 || !bytes.Equal(clear[:4], []byte("BLN2")) || clear[4] != bhttpV2Version {
		_ = WriteStatus(writer, StatusError, []byte("invalid BHTTP v2 lane hello"))
		return 0, nil, errors.New("invalid BHTTP v2 lane hello")
	}
	role := clear[5]
	if role != laneUpload && role != laneDownload && role != laneDuplex {
		_ = WriteStatus(writer, StatusError, []byte("invalid BHTTP v2 lane role"))
		return 0, nil, errors.New("invalid BHTTP v2 lane role")
	}
	requireResume := binary.BigEndian.Uint16(clear[6:8])&1 != 0
	laneID := binary.BigEndian.Uint32(clear[8:12])
	session, resumed := server.sessions.Get(request.SID)
	if requireResume && !resumed {
		_ = WriteStatus(writer, StatusError, []byte("BHTTP v2 session expired"))
		return 0, nil, errors.New("BHTTP v2 resume rejected: session expired")
	}
	if !resumed {
		session, _ = server.sessions.GetOrCreateState(request.SID)
	}
	lease, acquired := session.AcquireLane(laneID, server.config.MaxV2Lanes)
	if !acquired {
		_ = WriteStatus(writer, StatusError, []byte("BHTTP v2 lane limit exceeded"))
		return 0, nil, errors.New("BHTTP v2 lane limit exceeded")
	}
	attached := true
	defer func() {
		if attached {
			lease.ReleaseLane()
		}
	}()
	if err := session.OpenTarget(server.config.TargetAddress, server.config.DialTarget); err != nil {
		_ = WriteStatus(writer, StatusError, []byte("SSH target unavailable"))
		return 0, nil, err
	}
	response := make([]byte, 18)
	copy(response[:4], []byte("BOK2"))
	response[4] = bhttpV2Version
	if resumed {
		response[5] = 1
	}
	binary.BigEndian.PutUint32(response[6:10], bhttpV2FeatureLanes|bhttpV2FeatureResume|bhttpV2FeatureAck)
	if sequence, ok := session.UploadCommitted(); ok {
		binary.BigEndian.PutUint64(response[10:18], sequence+1)
	}
	encoded := Crypt(response, request.SID, ModeLane, request.Seq, true)
	if err := WriteStatus(writer, StatusOK, encoded); err != nil {
		return 0, nil, err
	}
	session.touch()
	attached = false
	return role, lease, nil
}

func (server *Server) Close() {
	server.once.Do(func() {
		close(server.closed)
		server.connMu.Lock()
		connections := make([]net.Conn, 0, len(server.active))
		for connection := range server.active {
			connections = append(connections, connection)
		}
		server.connMu.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
		server.sessions.Close()
	})
}

func (server *Server) Wait() {
	server.wait.Wait()
}
