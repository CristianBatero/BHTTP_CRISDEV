package bridge

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bilola/internal/bhttp"
)

const (
	batchCount            = 8
	referenceDownload     = 1399
	referenceUpload       = 32768
	minimumProbe          = 10
	calibrationTimeout    = 3 * time.Second
	emptyBatchDelay       = 750 * time.Millisecond
	uploadCoalesce        = 5 * time.Millisecond
	responseFrameBytes    = 5
	dataLengthBytes       = 4
	ackInterval           = 187
	maximumResponseLength = 2 * 1024 * 1024
)

var downloadFallback = []int{1024, 768, 512, 256, 128, 64, 32, 16, minimumProbe}
var uploadSteps = []int{4096, 8192, 12288, 16384, 20480, 24576, referenceUpload}
var uploadLowFallback = []int{2048, 1024, 512, 256, 128, 64, 32, 16, minimumProbe}

type Config struct {
	Host                string
	Port                int
	UploadConnections   int
	DownloadConnections int
	ConnectTimeout      time.Duration
	ReadTimeout         time.Duration
	// ProtectSocket, when set, is invoked with the OS file descriptor of each
	// outbound TCP connection to the relay before any data is sent. On Android
	// it routes the socket to VpnService.protect so its packets do not enter the
	// tunnel and loop back into the session. It is advisory: a false result (no
	// VPN active) is not treated as a connection error.
	ProtectSocket func(fd int) bool
}

type response struct {
	status byte
	body   []byte
}

type uploadFrame struct {
	sequence  uint64
	clearLen  int
	encrypted []byte
}

type Engine struct {
	config Config
	sid    bhttp.SessionID

	running    atomic.Bool
	uploaded   atomic.Uint64
	downloaded atomic.Uint64
	lastUpload atomic.Int64

	windowStride uint64
	windowBases  []uint64
	uploadQueue  chan uploadFrame

	localListener net.Listener
	localConn     net.Conn
	localMu       sync.Mutex

	socketsMu sync.Mutex
	sockets   map[net.Conn]struct{}
	wait      sync.WaitGroup

	downstreamMu sync.Mutex
	pending      map[uint64][]byte
	nextRead     uint64
	nextACK      uint64

	wakeupMu sync.Mutex
	wakeup   chan struct{}

	logMu   sync.Mutex
	logs    []string
	errorMu sync.Mutex
	fatal   string

	downloadChunk int
	uploadChunk   int
	closeOnce     sync.Once
}

func New(config Config) (*Engine, error) {
	if strings.TrimSpace(config.Host) == "" {
		return nil, errors.New("BHTTP host is required")
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, errors.New("invalid BHTTP port")
	}
	if config.UploadConnections < 1 {
		config.UploadConnections = 1
	}
	if config.DownloadConnections < 1 {
		config.DownloadConnections = 1
	}
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 8 * time.Second
	}
	if config.ReadTimeout <= 0 {
		config.ReadTimeout = 15 * time.Second
	}
	engine := &Engine{
		config:        config,
		windowStride:  uint64(batchCount * config.DownloadConnections),
		windowBases:   make([]uint64, config.DownloadConnections),
		uploadQueue:   make(chan uploadFrame, config.UploadConnections*2),
		sockets:       make(map[net.Conn]struct{}),
		pending:       make(map[uint64][]byte),
		nextACK:       ackInterval - 1,
		wakeup:        make(chan struct{}),
		downloadChunk: referenceDownload,
		uploadChunk:   referenceUpload,
	}
	for index := range engine.windowBases {
		engine.windowBases[index] = uint64(index * batchCount)
	}
	if _, err := rand.Read(engine.sid[:]); err != nil {
		return nil, err
	}
	return engine, nil
}

func (engine *Engine) Start() (int, error) {
	if !engine.running.CompareAndSwap(false, true) {
		return 0, errors.New("BHTTP bridge already started")
	}
	var startErr error
	defer func() {
		if startErr != nil {
			engine.Close()
		}
	}()
	engine.logf("BHTTP SID %x", engine.sid[:4])
	if startErr = engine.probe(bhttp.ModeProbe, 0, engine.config.ReadTimeout); startErr != nil {
		return 0, startErr
	}
	if startErr = engine.probe(bhttp.ModeUpload, 10, engine.config.ReadTimeout); startErr != nil {
		return 0, startErr
	}
	if engine.downloadChunk, startErr = engine.calibrateDownload(); startErr != nil {
		return 0, startErr
	}
	if engine.uploadChunk, startErr = engine.calibrateUpload(); startErr != nil {
		return 0, startErr
	}
	if startErr = engine.probe(bhttp.ModeBatch, batchCount, engine.config.ReadTimeout); startErr != nil {
		return 0, startErr
	}
	if startErr = engine.probe(bhttp.ModeACK, 1, engine.config.ReadTimeout); startErr != nil {
		return 0, startErr
	}
	engine.logf("Probe BHP1: todos os modos aceitos")
	engine.logf("Perfil GoJNI 1.0.11: downloadConns=%d, uploadConns=%d, batch=8, download=%d, upload=%d, stride=%d",
		engine.config.DownloadConnections, engine.config.UploadConnections,
		engine.downloadChunk, engine.uploadChunk, engine.windowStride)
	if startErr = engine.register(); startErr != nil {
		return 0, startErr
	}
	engine.logf("Registro BHTTP seq=0 aceito")
	engine.localListener, startErr = net.Listen("tcp", "127.0.0.1:0")
	if startErr != nil {
		return 0, startErr
	}
	engine.wait.Add(1)
	go engine.acceptLocal()
	return engine.localListener.Addr().(*net.TCPAddr).Port, nil
}

func (engine *Engine) acceptLocal() {
	defer engine.wait.Done()
	connection, err := engine.localListener.Accept()
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
	engine.localConn = connection
	engine.localMu.Unlock()
	engine.logf("SSH conectado ao bridge GoJNI local")
	engine.start(engine.uploadReadLoop)
	for index := 0; index < engine.config.UploadConnections; index++ {
		engine.start(engine.uploadWorkerLoop)
	}
	for _, base := range engine.windowBases {
		base := base
		engine.start(func() { engine.downloadLoop(base) })
	}
}

func (engine *Engine) start(action func()) {
	engine.wait.Add(1)
	go func() {
		defer engine.wait.Done()
		action()
	}()
}

func (engine *Engine) makeProbe(submode byte, parameter int) []byte {
	length := 10
	if submode == bhttp.ModeUpload && parameter > length {
		length = parameter
	}
	clear := make([]byte, length)
	copy(clear[:4], []byte("BHP1"))
	clear[4] = 1
	clear[5] = submode
	binary.BigEndian.PutUint32(clear[6:10], uint32(parameter))
	for index := 10; index < len(clear); index++ {
		clear[index] = byte(index * 31)
	}
	return clear
}

func (engine *Engine) probe(submode byte, parameter int, timeout time.Duration) error {
	clear := engine.makeProbe(submode, parameter)
	encrypted := bhttp.Crypt(clear, engine.sid, bhttp.ModeProbe, 0, false)
	result, err := engine.roundTrip(bhttp.ModeProbe, 0, encrypted, timeout)
	if err != nil {
		return err
	}
	if result.status != bhttp.StatusOK {
		return fmt.Errorf("BHP1 mode %d status %d", submode, result.status)
	}
	decoded := bhttp.Crypt(result.body, engine.sid, bhttp.ModeProbe, 0, true)
	expected := 10
	if submode == bhttp.ModeDownload && parameter > expected {
		expected = parameter
	}
	if len(decoded) != expected || len(decoded) < 10 || !strings.HasPrefix(string(decoded[:4]), "BHP1") || decoded[4] != 1 || decoded[5] != submode || int(binary.BigEndian.Uint32(decoded[6:10])) != parameter {
		return fmt.Errorf("invalid BHP1 mode %d response length=%d expected=%d", submode, len(decoded), expected)
	}
	for index := 10; index < len(decoded); index++ {
		if decoded[index] != byte(index*31) {
			return fmt.Errorf("corrupt BHP1 response at byte %d", index)
		}
	}
	return nil
}

func (engine *Engine) calibrateDownload() (int, error) {
	engine.logf("Calibrando envelope batch=8 do perfil 1.0.5 (chunk máximo 1399)...")
	if engine.acceptsStableProbe(bhttp.ModeDownload, batchEnvelope(referenceDownload), "download", referenceDownload) {
		engine.logf("Calibração BHTTP: envelope completo aceito; chunk fixado em 1399 bytes")
		return referenceDownload, nil
	}
	rejected := referenceDownload
	accepted := -1
	for _, candidate := range downloadFallback {
		if engine.acceptsStableProbe(bhttp.ModeDownload, batchEnvelope(candidate), "download", candidate) {
			accepted = candidate
			break
		}
		rejected = candidate
	}
	if accepted < 0 {
		return 0, fmt.Errorf("download calibration failed down to %d bytes", minimumProbe)
	}
	low, high := accepted, rejected-1
	for low < high {
		candidate := low + (high-low+1)/2
		if engine.acceptsStableProbe(bhttp.ModeDownload, batchEnvelope(candidate), "download", candidate) {
			low = candidate
		} else {
			high = candidate - 1
		}
	}
	engine.logf("Calibração BHTTP: envelope limitado; chunk fixado em %d bytes", low)
	return low, nil
}

func batchEnvelope(chunk int) int {
	value := batchCount*(chunk+responseFrameBytes+dataLengthBytes) - responseFrameBytes
	if value < minimumProbe {
		return minimumProbe
	}
	return value
}

func (engine *Engine) acceptsStableProbe(submode byte, parameter int, direction string, candidate int) bool {
	successes, failures := 0, 0
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := engine.probe(submode, parameter, calibrationTimeout); err == nil {
			successes++
			failures = 0
			if successes == 2 {
				return true
			}
		} else {
			last = err
			failures++
			successes = 0
			if failures == 2 {
				engine.logf("Calibração %s=%d recusada 2x: %v", direction, candidate, last)
				return false
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	engine.logf("Calibração %s=%d instável: %v", direction, candidate, last)
	return false
}

func (engine *Engine) calibrateUpload() (int, error) {
	engine.logf("Calibrando upload BHTTP com %d conexões concorrentes...", engine.config.UploadConnections)
	accepted := -1
	for _, candidate := range uploadSteps {
		if engine.acceptsUpload(candidate) {
			accepted = candidate
			continue
		}
		if accepted < 0 {
			var err error
			accepted, err = engine.findUploadFallback()
			if err != nil {
				return 0, err
			}
		}
		time.Sleep(750 * time.Millisecond)
		engine.logf("Calibração upload: faixa %d instável; margem operacional fixada em %d bytes com %d conexões", candidate, accepted, engine.config.UploadConnections)
		return accepted, nil
	}
	engine.logf("Calibração upload: perfil máximo 32768 bytes aceito com %d conexões", engine.config.UploadConnections)
	return referenceUpload, nil
}

func (engine *Engine) acceptsUpload(candidate int) bool {
	var last error
	for round := 1; round <= 2; round++ {
		if err := engine.concurrentUploadProbe(candidate); err == nil {
			return true
		} else {
			last = err
		}
		if round == 1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	engine.logf("Calibração upload=%d recusada em 2 rodadas: %v", candidate, last)
	return false
}

func (engine *Engine) concurrentUploadProbe(candidate int) error {
	start := make(chan struct{})
	errorsOut := make(chan error, engine.config.UploadConnections)
	var wait sync.WaitGroup
	for index := 0; index < engine.config.UploadConnections; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsOut <- engine.probe(bhttp.ModeUpload, candidate, calibrationTimeout)
		}()
	}
	close(start)
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			return err
		}
	}
	return nil
}

func (engine *Engine) findUploadFallback() (int, error) {
	for _, candidate := range uploadLowFallback {
		if engine.acceptsUpload(candidate) {
			return candidate, nil
		}
	}
	return 0, fmt.Errorf("upload calibration failed down to %d bytes", minimumProbe)
}

func (engine *Engine) register() error {
	result, err := engine.roundTrip(bhttp.ModeUpload, 0, nil, engine.config.ReadTimeout)
	if err != nil {
		return err
	}
	if result.status != bhttp.StatusOK {
		return fmt.Errorf("registration status %d", result.status)
	}
	return nil
}

func (engine *Engine) openSocket(timeout time.Duration) (net.Conn, error) {
	dialer := net.Dialer{
		Timeout:   engine.config.ConnectTimeout,
		KeepAlive: 30 * time.Second,
	}
	if protect := engine.config.ProtectSocket; protect != nil {
		dialer.Control = func(_ string, _ string, raw syscall.RawConn) error {
			return raw.Control(func(fd uintptr) {
				protect(int(fd))
			})
		}
	}
	connection, err := dialer.Dial("tcp", net.JoinHostPort(engine.config.Host, strconv.Itoa(engine.config.Port)))
	if err != nil {
		return nil, err
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
	}
	_ = connection.SetDeadline(time.Now().Add(timeout))
	engine.socketsMu.Lock()
	if !engine.running.Load() {
		engine.socketsMu.Unlock()
		connection.Close()
		return nil, net.ErrClosed
	}
	engine.sockets[connection] = struct{}{}
	engine.socketsMu.Unlock()
	return connection, nil
}

func (engine *Engine) closeSocket(connection net.Conn) {
	engine.socketsMu.Lock()
	delete(engine.sockets, connection)
	engine.socketsMu.Unlock()
	_ = connection.Close()
}

func (engine *Engine) sendRequest(writer io.Writer, mode byte, sequence uint64, payload []byte) error {
	packet := make([]byte, bhttp.HeaderSize+len(payload))
	packet[0] = mode
	copy(packet[1:17], engine.sid[:])
	binary.BigEndian.PutUint64(packet[17:25], sequence)
	binary.BigEndian.PutUint32(packet[25:29], uint32(len(payload)))
	copy(packet[29:], payload)
	return writeFull(writer, packet)
}

func readResponse(reader io.Reader) (response, error) {
	var result response
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return result, fmt.Errorf("truncated BHTTP response: %w", err)
	}
	result.status = header[0]
	length := int(binary.BigEndian.Uint32(header[1:5]))
	if length < 0 || length > maximumResponseLength {
		return result, fmt.Errorf("invalid BHTTP response length %d", length)
	}
	result.body = make([]byte, length)
	if _, err := io.ReadFull(reader, result.body); err != nil {
		return result, err
	}
	return result, nil
}

func (engine *Engine) roundTrip(mode byte, sequence uint64, payload []byte, timeout time.Duration) (response, error) {
	connection, err := engine.openSocket(timeout)
	if err != nil {
		return response{}, err
	}
	defer engine.closeSocket(connection)
	if err := engine.sendRequest(connection, mode, sequence, payload); err != nil {
		return response{}, err
	}
	return readResponse(connection)
}

func (engine *Engine) uploadReadLoop() {
	engine.localMu.Lock()
	connection := engine.localConn
	engine.localMu.Unlock()
	buffer := make([]byte, engine.uploadChunk)
	sequence := uint64(0)
	for engine.running.Load() {
		_ = connection.SetReadDeadline(time.Time{})
		count, err := connection.Read(buffer)
		if count > 0 {
			_ = connection.SetReadDeadline(time.Now().Add(uploadCoalesce))
			for count < len(buffer) {
				more, readErr := connection.Read(buffer[count:])
				if more > 0 {
					count += more
				}
				if readErr != nil {
					if timeout, ok := readErr.(net.Error); ok && timeout.Timeout() {
						break
					}
					if !errors.Is(readErr, io.EOF) {
						engine.fail(readErr)
					}
					break
				}
			}
			clear := append([]byte(nil), buffer[:count]...)
			frame := uploadFrame{sequence: sequence, clearLen: count, encrypted: bhttp.Crypt(clear, engine.sid, bhttp.ModeUpload, sequence, false)}
			select {
			case engine.uploadQueue <- frame:
				sequence++
			case <-engine.wakeupChannel():
				return
			}
		}
		if err != nil && count == 0 {
			if engine.running.Load() && !errors.Is(err, io.EOF) {
				engine.fail(err)
			}
			return
		}
	}
}

func (engine *Engine) uploadWorkerLoop() {
	for engine.running.Load() {
		select {
		case frame := <-engine.uploadQueue:
			for retry := 0; engine.running.Load(); retry++ {
				result, err := engine.roundTrip(bhttp.ModeUpload, frame.sequence, frame.encrypted, engine.config.ReadTimeout)
				if err == nil && result.status == bhttp.StatusOK {
					engine.uploaded.Add(uint64(frame.clearLen))
					engine.lastUpload.Store(time.Now().UnixNano())
					engine.signalWakeup()
					break
				}
				if err == nil {
					err = fmt.Errorf("upload status %d", result.status)
				}
				if retry >= 10 {
					engine.fail(err)
					return
				}
				engine.logf("Retry upload seq=%d bytes=%d tentativa=%d erro=%v", frame.sequence, frame.clearLen, retry+1, err)
				time.Sleep(time.Duration(retry+1) * 250 * time.Millisecond)
			}
		case <-engine.wakeupChannel():
			return
		}
	}
}

func (engine *Engine) downloadLoop(sequence uint64) {
	retries, emptyStreak := 0, 0
	for engine.running.Load() {
		hadData, err := engine.downloadBatch(sequence)
		if err == nil {
			sequence += engine.windowStride
			retries = 0
			if hadData {
				emptyStreak = 0
			} else {
				emptyStreak++
				delay := 25 * time.Millisecond * time.Duration(1<<min(emptyStreak, 5))
				if delay > emptyBatchDelay {
					delay = emptyBatchDelay
				}
				if time.Since(time.Unix(0, engine.lastUpload.Load())) < 2*time.Second && delay > 50*time.Millisecond {
					delay = 50 * time.Millisecond
				}
				engine.waitWakeup(delay)
			}
			continue
		}
		retries++
		engine.logf("Retry batch seq=%d tentativa=%d erro=%v", sequence, retries, err)
		if retries > 10 {
			engine.fail(err)
			return
		}
		delay := time.Duration(retries) * 200 * time.Millisecond
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
		time.Sleep(delay)
	}
}

func (engine *Engine) downloadBatch(sequence uint64) (bool, error) {
	clear := make([]byte, 6)
	binary.BigEndian.PutUint32(clear[:4], uint32(engine.downloadChunk))
	binary.BigEndian.PutUint16(clear[4:6], batchCount)
	encrypted := bhttp.Crypt(clear, engine.sid, bhttp.ModeBatch, sequence, false)
	connection, err := engine.openSocket(engine.config.ReadTimeout)
	if err != nil {
		return false, err
	}
	defer engine.closeSocket(connection)
	if err := engine.sendRequest(connection, bhttp.ModeBatch, sequence, encrypted); err != nil {
		return false, err
	}
	reader := bufio.NewReader(connection)
	chunks := make([][]byte, batchCount)
	hadData := false
	for offset := 0; offset < batchCount; offset++ {
		result, err := readResponse(reader)
		if err != nil {
			return false, err
		}
		if result.status != bhttp.StatusData || len(result.body) < 4 {
			return false, fmt.Errorf("batch seq=%d status=%d", sequence+uint64(offset), result.status)
		}
		length := int(binary.BigEndian.Uint32(result.body[:4]))
		if length != len(result.body)-4 {
			return false, fmt.Errorf("invalid DATA length %d", length)
		}
		if length > 0 {
			chunks[offset] = bhttp.Crypt(result.body[4:], engine.sid, bhttp.ModeBatch, sequence+uint64(offset), true)
			hadData = true
		}
	}
	var acknowledgements []uint64
	engine.downstreamMu.Lock()
	for offset, chunk := range chunks {
		engine.pending[sequence+uint64(offset)] = chunk
	}
	engine.localMu.Lock()
	local := engine.localConn
	engine.localMu.Unlock()
	for {
		chunk, exists := engine.pending[engine.nextRead]
		if !exists {
			break
		}
		delete(engine.pending, engine.nextRead)
		if len(chunk) > 0 {
			if err := writeFull(local, chunk); err != nil {
				engine.downstreamMu.Unlock()
				return false, err
			}
			engine.downloaded.Add(uint64(len(chunk)))
		}
		engine.nextRead++
	}
	for engine.nextRead > 0 && engine.nextRead-1 >= engine.nextACK {
		acknowledgements = append(acknowledgements, engine.nextACK)
		engine.nextACK += ackInterval
	}
	engine.downstreamMu.Unlock()
	for _, acknowledgement := range acknowledgements {
		if err := engine.acknowledge(acknowledgement); err != nil && engine.running.Load() {
			engine.logf("ACK seq=%d adiado: %v", acknowledgement, err)
		}
	}
	return hadData, nil
}

func writeFull(writer io.Writer, data []byte) error {
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

func (engine *Engine) acknowledge(sequence uint64) error {
	result, err := engine.roundTrip(bhttp.ModeACK, sequence, nil, engine.config.ReadTimeout)
	if err != nil {
		return err
	}
	if result.status != bhttp.StatusOK {
		return fmt.Errorf("ACK status %d", result.status)
	}
	return nil
}

func (engine *Engine) signalWakeup() {
	engine.wakeupMu.Lock()
	close(engine.wakeup)
	engine.wakeup = make(chan struct{})
	engine.wakeupMu.Unlock()
}

func (engine *Engine) wakeupChannel() <-chan struct{} {
	engine.wakeupMu.Lock()
	defer engine.wakeupMu.Unlock()
	return engine.wakeup
}

func (engine *Engine) waitWakeup(delay time.Duration) {
	engine.wakeupMu.Lock()
	signal := engine.wakeup
	engine.wakeupMu.Unlock()
	select {
	case <-signal:
	case <-time.After(delay):
	}
}

func (engine *Engine) fail(err error) {
	if err == nil || !engine.running.Load() {
		return
	}
	engine.errorMu.Lock()
	if engine.fatal == "" {
		engine.fatal = err.Error()
		engine.logf("BHTTP fatal: %v", err)
		engine.localMu.Lock()
		if engine.localConn != nil {
			_ = engine.localConn.Close()
		}
		engine.localMu.Unlock()
	}
	engine.errorMu.Unlock()
}

func (engine *Engine) logf(format string, arguments ...any) {
	engine.logMu.Lock()
	engine.logs = append(engine.logs, fmt.Sprintf(format, arguments...))
	if len(engine.logs) > 2000 {
		engine.logs = append([]string(nil), engine.logs[len(engine.logs)-2000:]...)
	}
	engine.logMu.Unlock()
}

func (engine *Engine) DrainLogs() string {
	engine.logMu.Lock()
	defer engine.logMu.Unlock()
	joined := strings.Join(engine.logs, "\n")
	engine.logs = engine.logs[:0]
	return joined
}

func (engine *Engine) LastError() string {
	engine.errorMu.Lock()
	defer engine.errorMu.Unlock()
	return engine.fatal
}

func (engine *Engine) UploadedBytes() uint64   { return engine.uploaded.Load() }
func (engine *Engine) DownloadedBytes() uint64 { return engine.downloaded.Load() }

func (engine *Engine) Close() {
	engine.closeOnce.Do(func() {
		engine.running.Store(false)
		engine.signalWakeup()
		if engine.localListener != nil {
			_ = engine.localListener.Close()
		}
		engine.localMu.Lock()
		if engine.localConn != nil {
			_ = engine.localConn.Close()
		}
		engine.localMu.Unlock()
		engine.socketsMu.Lock()
		for connection := range engine.sockets {
			_ = connection.Close()
		}
		engine.sockets = make(map[net.Conn]struct{})
		engine.socketsMu.Unlock()
	})
}

func (engine *Engine) Wait() { engine.wait.Wait() }

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
