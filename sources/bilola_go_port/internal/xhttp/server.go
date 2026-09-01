package xhttp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	HeaderSID            = "x-http-sid"
	HeaderMode           = "x-http-mode"
	HeaderSequence       = "x-http-seq"
	HeaderVersion        = "x-http-version"
	HeaderDownloadACK    = "x-http-download-ack"
	HeaderDownloadFormat = "x-http-download-format"
	HeaderUploadACK      = "x-http-ack"
	ProtocolVersion      = "2"
	DownloadPrefix       = ":\r\n\r\n"
	maxUploadBody        = 4 << 20
	maxDownloadFrame     = 4 << 20
)

type Config struct {
	TargetAddress           string
	TargetAddressForRequest func(*http.Request) string
	DeferTargetForRequest   func(*http.Request) bool
	TargetAddressForPayload func([]byte) string
	DeferredTargetDelay     time.Duration
	SessionTimeout          time.Duration
	DialTimeout             time.Duration
	Logger                  *log.Logger
	DialTarget              func(string) (net.Conn, error)
}

type Server struct {
	config   Config
	mu       sync.Mutex
	sessions map[string]*session
	closed   chan struct{}
	close    sync.Once
	wait     sync.WaitGroup
}

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
	if config.DeferredTargetDelay <= 0 {
		config.DeferredTargetDelay = time.Second
	}
	if config.Logger == nil {
		config.Logger = log.Default()
	}
	if config.DialTarget == nil {
		dialer := net.Dialer{Timeout: config.DialTimeout, KeepAlive: 30 * time.Second}
		config.DialTarget = func(address string) (net.Conn, error) {
			connection, err := dialer.Dial("tcp", address)
			if tcp, ok := connection.(*net.TCPConn); err == nil && ok {
				_ = tcp.SetNoDelay(true)
			}
			return connection, err
		}
	}
	server := &Server{config: config, sessions: make(map[string]*session), closed: make(chan struct{})}
	server.wait.Add(1)
	go server.cleanupLoop()
	return server
}

func validSID(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func (server *Server) getSession(sid, targetAddress string, deferred bool) (*session, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	select {
	case <-server.closed:
		return nil, net.ErrClosed
	default:
	}
	current := server.sessions[sid]
	if current == nil {
		if strings.TrimSpace(targetAddress) == "" {
			targetAddress = server.config.TargetAddress
		}
		current = newSession(sid)
		if deferred {
			server.sessions[sid] = current
			server.config.Logger.Printf("XHTTP session %s awaiting protocol detection", shortSID(sid))
			go server.openDeferredTarget(current, targetAddress)
		} else {
			if _, err := current.open(targetAddress, server.config.DialTarget); err != nil {
				return nil, err
			}
			server.sessions[sid] = current
			server.config.Logger.Printf("XHTTP session %s opened -> %s", shortSID(sid), targetAddress)
		}
	}
	current.touch()
	return current, nil
}

func (server *Server) openDeferredTarget(current *session, fallbackAddress string) {
	timer := time.NewTimer(server.config.DeferredTargetDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		server.openSessionTarget(current, fallbackAddress, "fallback")
	case <-current.done:
	}
}

func (server *Server) openSessionTarget(current *session, address, reason string) error {
	opened, err := current.open(address, server.config.DialTarget)
	if opened {
		if err != nil {
			server.config.Logger.Printf("XHTTP session %s target %s failed (%s): %v",
				shortSID(current.sid), address, reason, err)
		} else {
			server.config.Logger.Printf("XHTTP session %s opened -> %s (%s)",
				shortSID(current.sid), address, reason)
		}
	}
	return err
}

func shortSID(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}
	// The DTunnel client requires HTTP/2 on the client-facing hop. A CDN may
	// legitimately proxy that stream to the origin over HTTP/1.1, however.
	// Framing, sequencing and resume semantics are independent of the HTTP
	// version used on this origin hop, so both versions must be accepted here.
	sid := request.Header.Get(HeaderSID)
	if !validSID(sid) {
		http.Error(writer, "invalid xhttp session", http.StatusBadRequest)
		return
	}
	mode := strings.ToLower(request.Header.Get(HeaderMode))
	if mode != "upload" && mode != "download" && mode != "ack" {
		http.Error(writer, "invalid xhttp mode", http.StatusBadRequest)
		return
	}
	targetAddress := server.config.TargetAddress
	if server.config.TargetAddressForRequest != nil {
		targetAddress = server.config.TargetAddressForRequest(request)
	}
	deferred := server.config.DeferTargetForRequest != nil && server.config.DeferTargetForRequest(request)
	current, err := server.getSession(sid, targetAddress, deferred)
	if err != nil {
		http.Error(writer, "SSH target unavailable", http.StatusBadGateway)
		return
	}
	writer.Header().Set(HeaderVersion, ProtocolVersion)
	writer.Header().Set("Cache-Control", "no-store, no-transform")
	writer.Header().Set("Pragma", "no-cache")

	switch mode {
	case "upload":
		server.handleUpload(writer, request, current)
	case "download":
		server.handleDownload(writer, request, current)
	case "ack":
		server.handleACK(writer, request, current)
	}
}

func parseUintHeader(request *http.Request, name string, required bool) (uint64, error) {
	value := request.Header.Get(name)
	if value == "" && !required {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return parsed, nil
}

func (server *Server) handleUpload(writer http.ResponseWriter, request *http.Request, current *session) {
	sequence, err := parseUintHeader(request, HeaderSequence, true)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxUploadBody+1))
	if err != nil || len(body) > maxUploadBody {
		http.Error(writer, "invalid xhttp upload body", http.StatusRequestEntityTooLarge)
		return
	}
	if current.targetPending() && len(body) > 0 {
		targetAddress := server.config.TargetAddress
		if server.config.TargetAddressForPayload != nil {
			targetAddress = server.config.TargetAddressForPayload(body)
		}
		if err := server.openSessionTarget(current, targetAddress, "handshake"); err != nil {
			http.Error(writer, "xhttp target unavailable", http.StatusBadGateway)
			return
		}
	}
	if ack, err := parseUintHeader(request, HeaderDownloadACK, false); err == nil && request.Header.Get(HeaderDownloadACK) != "" {
		current.acknowledgeDownload(ack)
	}
	uploadACK, err := current.writeUpload(sequence, body)
	if err != nil {
		http.Error(writer, "xhttp upload failed", http.StatusBadGateway)
		return
	}
	writer.Header().Set(HeaderUploadACK, strconv.FormatUint(uploadACK, 10))
	writer.Header().Set(HeaderDownloadACK, strconv.FormatUint(current.downloadACK(), 10))
	writer.WriteHeader(http.StatusOK)
}

func (server *Server) handleACK(writer http.ResponseWriter, request *http.Request, current *session) {
	ack, err := parseUintHeader(request, HeaderDownloadACK, true)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	current.acknowledgeDownload(ack)
	writer.Header().Set(HeaderDownloadACK, strconv.FormatUint(ack, 10))
	writer.WriteHeader(http.StatusOK)
}

func (server *Server) handleDownload(writer http.ResponseWriter, request *http.Request, current *session) {
	start, err := parseUintHeader(request, HeaderDownloadACK, false)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if request.Header.Get(HeaderDownloadACK) != "" {
		current.acknowledgeDownload(start)
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Content-Encoding", "identity")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.Header().Set(HeaderDownloadFormat, "frame")
	writer.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(writer, DownloadPrefix); err != nil {
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
	sequence := start
	for {
		payload, err := current.waitDownload(request.Context(), sequence)
		if err != nil {
			return
		}
		var header [12]byte
		binary.BigEndian.PutUint64(header[:8], sequence)
		binary.BigEndian.PutUint32(header[8:], uint32(len(payload)))
		if _, err := writer.Write(header[:]); err != nil {
			return
		}
		if len(payload) > 0 {
			if _, err := writer.Write(payload); err != nil {
				return
			}
		}
		flusher.Flush()
		sequence++
	}
}

func (server *Server) cleanupLoop() {
	defer server.wait.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			server.mu.Lock()
			for sid, current := range server.sessions {
				if now.Sub(current.lastActivity()) > server.config.SessionTimeout {
					delete(server.sessions, sid)
					current.Close()
				}
			}
			server.mu.Unlock()
		case <-server.closed:
			return
		}
	}
}

func (server *Server) Close() {
	server.close.Do(func() {
		close(server.closed)
		server.mu.Lock()
		for sid, current := range server.sessions {
			delete(server.sessions, sid)
			current.Close()
		}
		server.mu.Unlock()
	})
	server.wait.Wait()
}

type session struct {
	sid string

	targetMu      sync.Mutex
	target        net.Conn
	targetOnce    sync.Once
	targetReady   chan struct{}
	targetOpenErr error
	done          chan struct{}

	uploadMu      sync.Mutex
	uploadNext    uint64
	uploadPending map[uint64][]byte

	downloadMu     sync.Mutex
	downloadNext   uint64
	downloadAcked  uint64
	downloadFrames map[uint64][]byte
	downloadSignal chan struct{}
	downloadClosed bool
	downloadErr    error

	lastSeen atomic.Int64
	close    sync.Once
}

func newSession(sid string) *session {
	current := &session{
		sid: sid, uploadPending: make(map[uint64][]byte),
		downloadFrames: make(map[uint64][]byte), downloadSignal: make(chan struct{}),
		targetReady: make(chan struct{}), done: make(chan struct{}),
	}
	current.touch()
	return current
}

func (current *session) open(address string, dial func(string) (net.Conn, error)) (bool, error) {
	opened := false
	current.targetOnce.Do(func() {
		opened = true
		target, err := dial(address)
		if err == nil {
			select {
			case <-current.done:
				_ = target.Close()
				err = net.ErrClosed
			default:
			}
		}
		current.targetMu.Lock()
		current.target = target
		current.targetOpenErr = err
		current.targetMu.Unlock()
		close(current.targetReady)
		if err == nil {
			go current.readTarget(target)
		}
	})
	<-current.targetReady
	current.targetMu.Lock()
	err := current.targetOpenErr
	current.targetMu.Unlock()
	return opened, err
}

func (current *session) targetPending() bool {
	select {
	case <-current.targetReady:
		return false
	default:
		return true
	}
}

func (current *session) touch()                  { current.lastSeen.Store(time.Now().UnixNano()) }
func (current *session) lastActivity() time.Time { return time.Unix(0, current.lastSeen.Load()) }

func (current *session) signalDownloadLocked() {
	close(current.downloadSignal)
	current.downloadSignal = make(chan struct{})
}

func (current *session) readTarget(target net.Conn) {
	buffer := make([]byte, 256<<10)
	for {
		count, err := target.Read(buffer)
		if count > 0 {
			payload := append([]byte(nil), buffer[:count]...)
			current.downloadMu.Lock()
			sequence := current.downloadNext
			current.downloadNext++
			current.downloadFrames[sequence] = payload
			current.touch()
			current.signalDownloadLocked()
			current.downloadMu.Unlock()
		}
		if err != nil {
			current.downloadMu.Lock()
			current.downloadClosed = true
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				current.downloadErr = err
			}
			current.signalDownloadLocked()
			current.downloadMu.Unlock()
			return
		}
	}
}

func (current *session) writeUpload(sequence uint64, payload []byte) (uint64, error) {
	current.uploadMu.Lock()
	defer current.uploadMu.Unlock()
	current.targetMu.Lock()
	target := current.target
	targetErr := current.targetOpenErr
	current.targetMu.Unlock()
	if targetErr != nil {
		return current.uploadNext, targetErr
	}
	if target == nil {
		return current.uploadNext, errors.New("xhttp target is not ready")
	}
	if sequence < current.uploadNext {
		return current.uploadNext, nil
	}
	if _, exists := current.uploadPending[sequence]; !exists {
		current.uploadPending[sequence] = append([]byte(nil), payload...)
	}
	for {
		chunk, exists := current.uploadPending[current.uploadNext]
		if !exists {
			return current.uploadNext, nil
		}
		delete(current.uploadPending, current.uploadNext)
		if len(chunk) > 0 {
			if err := writeAll(target, chunk); err != nil {
				return current.uploadNext, err
			}
		}
		current.uploadNext++
		current.touch()
	}
}

func (current *session) acknowledgeDownload(next uint64) {
	current.downloadMu.Lock()
	if next > current.downloadAcked {
		current.downloadAcked = next
		for sequence := range current.downloadFrames {
			if sequence < next {
				delete(current.downloadFrames, sequence)
			}
		}
	}
	current.touch()
	current.downloadMu.Unlock()
}

func (current *session) downloadACK() uint64 {
	current.downloadMu.Lock()
	defer current.downloadMu.Unlock()
	return current.downloadAcked
}

func (current *session) waitDownload(ctx context.Context, sequence uint64) ([]byte, error) {
	for {
		current.downloadMu.Lock()
		if payload, exists := current.downloadFrames[sequence]; exists {
			out := append([]byte(nil), payload...)
			current.downloadMu.Unlock()
			return out, nil
		}
		if current.downloadClosed {
			err := current.downloadErr
			if err == nil {
				err = io.EOF
			}
			current.downloadMu.Unlock()
			return nil, err
		}
		signal := current.downloadSignal
		current.downloadMu.Unlock()
		select {
		case <-signal:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (current *session) Close() {
	current.close.Do(func() {
		close(current.done)
		current.targetMu.Lock()
		target := current.target
		current.targetMu.Unlock()
		if target != nil {
			_ = target.Close()
		}
		current.downloadMu.Lock()
		current.downloadClosed = true
		current.signalDownloadLocked()
		current.downloadMu.Unlock()
	})
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		count, err := writer.Write(data)
		if err != nil {
			return err
		}
		if count <= 0 {
			return io.ErrShortWrite
		}
		data = data[count:]
	}
	return nil
}
