package xhttpbridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bilola/internal/xhttp"
)

const (
	uploadWorkers       = 4
	uploadChunkSize     = 256 << 10
	maximumFrameSize    = 4 << 20
	requestRetries      = 5
	proxyProbeTimeout   = 3500 * time.Millisecond
	initialACKThreshold = 256 << 10
	maximumACKThreshold = 1 << 20
)

type Config struct {
	Host               string
	Port               int
	ServerName         string
	HostHeader         string
	InsecureSkipVerify bool
	ConnectTimeout     time.Duration
	// ProtectSocket, when set, is invoked with the OS file descriptor of each
	// outbound TCP connection to the relay before any data is sent. On Android
	// it routes the socket to VpnService.protect so its packets do not enter the
	// tunnel and loop back into the session. It is advisory: a false result (no
	// VPN active) is not treated as a connection error.
	ProtectSocket func(fd int) bool
}

type uploadFrame struct {
	sequence uint64
	payload  []byte
}

type Engine struct {
	config Config
	sid    string
	client *http.Client

	proxyMu    sync.Mutex
	proxyHosts []string
	proxyIndex int

	context context.Context
	cancel  context.CancelFunc
	running atomic.Bool

	listener net.Listener
	localMu  sync.Mutex
	local    net.Conn
	uploads  chan uploadFrame
	wait     sync.WaitGroup

	uploaded   atomic.Uint64
	downloaded atomic.Uint64
	nextUpload atomic.Uint64
	nextDown   atomic.Uint64

	logMu    sync.Mutex
	logs     []string
	retryMu  sync.Mutex
	retryAt  time.Time
	retryKey string
	errorMu  sync.Mutex
	fatal    string
	close    sync.Once
}

func New(config Config) (*Engine, error) {
	config.Host = strings.TrimSpace(config.Host)
	config.ServerName = strings.TrimSpace(config.ServerName)
	config.HostHeader = strings.TrimSpace(config.HostHeader)
	if config.Host == "" {
		return nil, errors.New("XHTTP host is required")
	}
	proxyHosts, err := parseProxyHosts(config.Host)
	if err != nil {
		return nil, err
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("invalid XHTTP port")
	}
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 10 * time.Second
	}
	if config.HostHeader == "" {
		config.HostHeader = config.ServerName
	}
	if config.HostHeader == "" {
		config.HostHeader = config.Host
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	dialer := &net.Dialer{
		Timeout:   config.ConnectTimeout,
		KeepAlive: 30 * time.Second,
	}
	if protect := config.ProtectSocket; protect != nil {
		dialer.Control = func(_ string, _ string, raw syscall.RawConn) error {
			return raw.Control(func(fd uintptr) {
				protect(int(fd))
			})
		}
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		DisableCompression:  true,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ServerName:         config.ServerName,
			InsecureSkipVerify: config.InsecureSkipVerify, // explicit profile option
			NextProtos:         []string{"h2"},
		},
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: config.ConnectTimeout,
	}
	return &Engine{
		config:     config,
		sid:        hex.EncodeToString(random),
		proxyHosts: proxyHosts,
		client: &http.Client{
			Transport: transport,
		},
		context: ctx,
		cancel:  cancel,
		uploads: make(chan uploadFrame, uploadWorkers*2),
	}, nil
}

func (engine *Engine) Start() (int, error) {
	if !engine.running.CompareAndSwap(false, true) {
		return 0, errors.New("XHTTP bridge already started")
	}
	engine.logf("SSH_XHTTP SID %s", engine.sid[:8])
	engine.logf("SSH_XHTTP: %d proxy(s), porta=%d Host=%s SNI=%s",
		len(engine.proxyHosts), engine.config.Port, engine.config.HostHeader,
		displaySNI(engine.config.ServerName))
	if err := engine.selectWorkingProxies(); err != nil {
		engine.fail(err)
		return 0, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		engine.running.Store(false)
		return 0, err
	}
	engine.listener = listener
	engine.logf("Proxy XHTTP selecionado: %s:%d", engine.currentProxy(), engine.config.Port)
	engine.wait.Add(1)
	go engine.acceptLocal()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func displaySNI(value string) string {
	if value == "" {
		return "automático"
	}
	return value
}

func (engine *Engine) acceptLocal() {
	defer engine.wait.Done()
	connection, err := engine.listener.Accept()
	if err != nil {
		if engine.running.Load() {
			engine.fail(err)
		}
		return
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	engine.localMu.Lock()
	engine.local = connection
	engine.localMu.Unlock()
	engine.logf("SSH conectado ao bridge SSH_XHTTP local")
	engine.start(engine.uploadReadLoop)
	for index := 0; index < uploadWorkers; index++ {
		engine.start(engine.uploadWorker)
	}
	engine.start(engine.downloadLoop)
}

func (engine *Engine) start(action func()) {
	engine.wait.Add(1)
	go func() {
		defer engine.wait.Done()
		action()
	}()
}

func parseProxyHosts(value string) ([]string, error) {
	seen := make(map[string]struct{})
	hosts := make([]string, 0)
	for _, candidate := range strings.Split(value, "#") {
		host := strings.TrimSpace(candidate)
		if host == "" {
			continue
		}
		if strings.ContainsAny(host, "/?#") {
			return nil, fmt.Errorf("invalid XHTTP proxy host %q", host)
		}
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		}
		if _, exists := seen[host]; exists {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return nil, errors.New("at least one XHTTP proxy host is required")
	}
	return hosts, nil
}

func (engine *Engine) currentProxy() string {
	engine.proxyMu.Lock()
	defer engine.proxyMu.Unlock()
	return engine.proxyHosts[engine.proxyIndex]
}

func (engine *Engine) rotateProxy(failed string) (string, bool) {
	engine.proxyMu.Lock()
	defer engine.proxyMu.Unlock()
	current := engine.proxyHosts[engine.proxyIndex]
	if len(engine.proxyHosts) < 2 || current != failed {
		return current, false
	}
	engine.proxyIndex = (engine.proxyIndex + 1) % len(engine.proxyHosts)
	return engine.proxyHosts[engine.proxyIndex], true
}

func (engine *Engine) endpoint(proxyHost string) string {
	return "https://" + net.JoinHostPort(proxyHost, strconv.Itoa(engine.config.Port)) + "/"
}

func (engine *Engine) newRequest(ctx context.Context, mode string, body []byte) (*http.Request, error) {
	return engine.newRequestForProxy(ctx, mode, body, engine.currentProxy())
}

func (engine *Engine) newRequestForProxy(ctx context.Context, mode string, body []byte, proxyHost string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		engine.endpoint(proxyHost), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Host = engine.config.HostHeader
	request.Header.Set(xhttp.HeaderSID, engine.sid)
	request.Header.Set(xhttp.HeaderMode, mode)
	request.Header.Set(xhttp.HeaderVersion, xhttp.ProtocolVersion)
	request.Header.Set("Cache-Control", "no-store, no-transform")
	request.Header.Set("Pragma", "no-cache")
	return request, nil
}

func (engine *Engine) selectWorkingProxies() error {
	engine.proxyMu.Lock()
	candidates := append([]string(nil), engine.proxyHosts...)
	engine.proxyMu.Unlock()
	working := make([]string, 0, len(candidates))
	engine.logf("Proxies testados: 0/%d — iniciando teste sequencial", len(candidates))
	for index, proxyHost := range candidates {
		err := engine.probeProxy(proxyHost)
		if err == nil {
			working = append(working, proxyHost)
			engine.logf("Proxies testados: %d/%d — %s OK", index+1, len(candidates), proxyHost)
		} else {
			engine.logf("Proxies testados: %d/%d — %s indisponível (%s)",
				index+1, len(candidates), proxyHost, engine.briefError(err))
		}
		if !engine.running.Load() {
			return net.ErrClosed
		}
	}
	if len(working) == 0 {
		return fmt.Errorf("nenhum dos %d proxies respondeu ao XHTTP", len(candidates))
	}
	engine.proxyMu.Lock()
	engine.proxyHosts = working
	engine.proxyIndex = 0
	engine.proxyMu.Unlock()
	engine.logf("Teste concluído: %d/%d proxy(s) ativo(s)", len(working), len(candidates))
	return nil
}

func (engine *Engine) probeProxy(proxyHost string) error {
	ctx, cancel := context.WithTimeout(engine.context, proxyProbeTimeout)
	defer cancel()
	request, err := engine.newRequestForProxy(ctx, "ack", nil, proxyHost)
	if err != nil {
		return err
	}
	// An intentionally invalid SID exercises TCP, TLS, HTTP/2, SNI, Host and
	// CDN-to-origin routing without opening an SSH target session on the server.
	request.Header.Set(xhttp.HeaderSID, "probe")
	request.Header.Set(xhttp.HeaderDownloadACK, "0")
	response, err := engine.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return err
	}
	if response.ProtoMajor != 2 {
		return fmt.Errorf("HTTP/2 não negociado (%s)", response.Proto)
	}
	if response.StatusCode != http.StatusBadRequest ||
		!strings.Contains(strings.ToLower(string(body)), "invalid xhttp session") {
		return fmt.Errorf("endpoint não reconheceu XHTTP (HTTP %d)", response.StatusCode)
	}
	return nil
}

func (engine *Engine) uploadReadLoop() {
	buffer := make([]byte, uploadChunkSize)
	for engine.running.Load() {
		count, err := engine.local.Read(buffer)
		if count > 0 {
			frame := uploadFrame{
				sequence: engine.nextUpload.Add(1) - 1,
				payload:  append([]byte(nil), buffer[:count]...),
			}
			select {
			case engine.uploads <- frame:
			case <-engine.context.Done():
				return
			}
		}
		if err != nil {
			if engine.running.Load() && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				engine.fail(err)
			}
			return
		}
	}
}

func (engine *Engine) uploadWorker() {
	for {
		select {
		case frame := <-engine.uploads:
			if err := engine.sendUpload(frame); err != nil {
				engine.fail(err)
				return
			}
		case <-engine.context.Done():
			return
		}
	}
}

func (engine *Engine) sendUpload(frame uploadFrame) error {
	var last error
	for attempt := 0; attempt <= requestRetries && engine.running.Load(); attempt++ {
		request, err := engine.newRequest(engine.context, "upload", frame.payload)
		if err == nil {
			request.Header.Set(xhttp.HeaderSequence, strconv.FormatUint(frame.sequence, 10))
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set(xhttp.HeaderDownloadACK, strconv.FormatUint(engine.nextDown.Load(), 10))
			var response *http.Response
			response, err = engine.client.Do(request)
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.ProtoMajor != 2 {
					err = fmt.Errorf("XHTTP upload requires HTTP/2, got %s", response.Proto)
				} else if response.StatusCode < 200 || response.StatusCode >= 300 {
					err = fmt.Errorf("XHTTP upload status %d", response.StatusCode)
				} else {
					engine.uploaded.Add(uint64(len(frame.payload)))
					return nil
				}
			}
		}
		last = err
		if attempt < requestRetries {
			engine.retry(requestProxy(request), "upload", attempt, err)
			if !engine.pause(time.Duration(attempt+1) * 50 * time.Millisecond) {
				return net.ErrClosed
			}
		}
	}
	return fmt.Errorf("XHTTP upload seq=%d: %w", frame.sequence, last)
}

func (engine *Engine) downloadLoop() {
	for attempt := 0; attempt <= requestRetries && engine.running.Load(); attempt++ {
		failedProxy, err := engine.downloadOnce()
		if err == nil || !engine.running.Load() {
			return
		}
		if attempt == requestRetries {
			engine.fail(fmt.Errorf("XHTTP download: %w", err))
			return
		}
		engine.retry(failedProxy, "download", attempt, err)
		if !engine.pause(time.Duration(attempt+1) * 50 * time.Millisecond) {
			return
		}
	}
}

func (engine *Engine) downloadOnce() (string, error) {
	request, err := engine.newRequest(engine.context, "download", nil)
	proxy := requestProxy(request)
	if err != nil {
		return proxy, err
	}
	start := engine.nextDown.Load()
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set(xhttp.HeaderDownloadFormat, "frame")
	if start != 0 {
		request.Header.Set(xhttp.HeaderDownloadACK, strconv.FormatUint(start, 10))
	}
	response, err := engine.client.Do(request)
	if err != nil {
		return proxy, err
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		return proxy, fmt.Errorf("XHTTP download requires HTTP/2, got %s", response.Proto)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return proxy, fmt.Errorf("XHTTP download status %d", response.StatusCode)
	}
	if response.Header.Get(xhttp.HeaderVersion) != xhttp.ProtocolVersion {
		return proxy, errors.New("XHTTP server did not negotiate version 2")
	}
	prefix := make([]byte, len(xhttp.DownloadPrefix))
	if _, err := io.ReadFull(response.Body, prefix); err != nil {
		return proxy, err
	}
	if string(prefix) != xhttp.DownloadPrefix {
		return proxy, errors.New("invalid XHTTP stream prefix")
	}
	engine.logf("SSH_XHTTP download HTTP/2 ativo; retomada seq=%d", start)
	bytesSinceACK := 0
	ackThreshold := initialACKThreshold
	for engine.running.Load() {
		var header [12]byte
		if _, err := io.ReadFull(response.Body, header[:]); err != nil {
			return proxy, err
		}
		sequence := binary.BigEndian.Uint64(header[:8])
		length := binary.BigEndian.Uint32(header[8:])
		expected := engine.nextDown.Load()
		if sequence != expected {
			return proxy, fmt.Errorf("XHTTP frame sequence=%d, expected=%d", sequence, expected)
		}
		if length > maximumFrameSize {
			return proxy, fmt.Errorf("XHTTP frame too large: %d", length)
		}
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(response.Body, payload); err != nil {
			return proxy, err
		}
		if err := writeAll(engine.local, payload); err != nil {
			return proxy, err
		}
		engine.downloaded.Add(uint64(len(payload)))
		next := engine.nextDown.Add(1)
		bytesSinceACK += len(payload)
		if bytesSinceACK >= ackThreshold {
			if err := engine.sendACK(next); err != nil {
				return proxy, err
			}
			bytesSinceACK = 0
			if ackThreshold < maximumACKThreshold {
				ackThreshold *= 2
				if ackThreshold > maximumACKThreshold {
					ackThreshold = maximumACKThreshold
				}
			}
		}
	}
	return proxy, nil
}

func (engine *Engine) sendACK(next uint64) error {
	request, err := engine.newRequest(engine.context, "ack", nil)
	if err != nil {
		return err
	}
	request.Header.Set(xhttp.HeaderDownloadACK, strconv.FormatUint(next, 10))
	response, err := engine.client.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.ProtoMajor != 2 || response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("XHTTP ACK rejected: %s status=%d", response.Proto, response.StatusCode)
	}
	return nil
}

func (engine *Engine) pause(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-engine.context.Done():
		return false
	}
}

func requestProxy(request *http.Request) string {
	if request == nil || request.URL == nil {
		return ""
	}
	return request.URL.Hostname()
}

func (engine *Engine) retry(failedProxy, direction string, attempt int, err error) {
	if failedProxy == "" {
		failedProxy = engine.currentProxy()
	}
	next, changed := engine.rotateProxy(failedProxy)
	if attempt != 0 {
		return
	}
	brief := engine.briefError(err)
	message := "XHTTP " + direction + " em nova tentativa: " + brief
	if changed {
		message = "Proxy " + failedProxy + " falhou (" + brief + "); alternando para " + next
	}
	engine.retryMu.Lock()
	now := time.Now()
	if engine.retryKey == brief && now.Sub(engine.retryAt) < 3*time.Second {
		engine.retryMu.Unlock()
		return
	}
	engine.retryKey = brief
	engine.retryAt = now
	engine.retryMu.Unlock()
	engine.logf("%s", message)
}

func (engine *Engine) briefError(err error) string {
	if err == nil {
		return "erro desconhecido"
	}
	message := err.Error()
	if strings.Contains(message, "certificate is valid for") ||
		strings.Contains(message, "certificate is not valid for") {
		return "certificado TLS não corresponde ao SNI " + displaySNI(engine.config.ServerName)
	}
	if strings.Contains(message, "no such host") {
		return "host não encontrado"
	}
	const maximum = 180
	if len(message) > maximum {
		return message[:maximum] + "..."
	}
	return message
}

func (engine *Engine) fail(err error) {
	if err == nil || !engine.running.Load() {
		return
	}
	engine.errorMu.Lock()
	first := engine.fatal == ""
	if engine.fatal == "" {
		engine.fatal = engine.briefError(err)
	}
	engine.errorMu.Unlock()
	if first {
		engine.logf("SSH_XHTTP encerrado: %s", engine.briefError(err))
	}
	engine.Close()
}

func (engine *Engine) logf(format string, values ...any) {
	engine.logMu.Lock()
	engine.logs = append(engine.logs, fmt.Sprintf(format, values...))
	engine.logMu.Unlock()
}

func (engine *Engine) DrainLogs() string {
	engine.logMu.Lock()
	defer engine.logMu.Unlock()
	text := strings.Join(engine.logs, "\n")
	engine.logs = nil
	return text
}

func (engine *Engine) LastError() string {
	engine.errorMu.Lock()
	defer engine.errorMu.Unlock()
	return engine.fatal
}

func (engine *Engine) UploadedBytes() uint64   { return engine.uploaded.Load() }
func (engine *Engine) DownloadedBytes() uint64 { return engine.downloaded.Load() }

func (engine *Engine) Close() {
	engine.close.Do(func() {
		engine.running.Store(false)
		engine.cancel()
		if engine.listener != nil {
			_ = engine.listener.Close()
		}
		engine.localMu.Lock()
		if engine.local != nil {
			_ = engine.local.Close()
		}
		engine.localMu.Unlock()
		if transport, ok := engine.client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	})
}

func (engine *Engine) Wait() { engine.wait.Wait() }

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		count, err := writer.Write(data)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		data = data[count:]
	}
	return nil
}
