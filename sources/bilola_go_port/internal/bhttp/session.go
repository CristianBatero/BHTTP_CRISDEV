package bhttp

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type byteQueue struct {
	data   []byte
	offset int
}

func (queue *byteQueue) append(data []byte) {
	queue.data = append(queue.data, data...)
}

func (queue *byteQueue) available() int {
	return len(queue.data) - queue.offset
}

func (queue *byteQueue) take(limit int) []byte {
	if queue.available() == 0 {
		return nil
	}
	end := queue.offset + limit
	if end > len(queue.data) {
		end = len(queue.data)
	}
	out := append([]byte(nil), queue.data[queue.offset:end]...)
	queue.offset = end
	if queue.offset == len(queue.data) {
		queue.data = queue.data[:0]
		queue.offset = 0
	} else if queue.offset >= 1<<20 && queue.offset*2 >= len(queue.data) {
		copy(queue.data, queue.data[queue.offset:])
		queue.data = queue.data[:len(queue.data)-queue.offset]
		queue.offset = 0
	}
	return out
}

type Session struct {
	sid SessionID

	openOnce sync.Once
	openErr  error
	target   net.Conn

	uploadMu      sync.Mutex
	uploadPending map[uint64][]byte
	uploadNext    uint64
	laneMu        sync.Mutex
	activeLanes   map[uint32]uint64
	nextLaneLease uint64

	readyMu         sync.Mutex
	readySignal     chan struct{}
	downloadBuffer  byteQueue
	bannerDelivered bool
	targetEOF       bool
	targetError     error

	downloadMu     sync.Mutex
	downloadSignal chan struct{}
	downloadNext   uint64
	downloadAcked  uint64
	hasACK         bool
	downloadClosed bool
	downloadFrames map[uint64][]byte

	lastSeen  atomic.Int64
	closeOnce sync.Once
}

type LaneLease struct {
	session    *Session
	laneID     uint32
	generation uint64
	once       sync.Once
}

// AcquireLane gives a stable lane ID a new generation. Reconnecting the same
// logical lane replaces its previous socket immediately instead of consuming
// another slot until the old handler notices the close.
func (session *Session) AcquireLane(laneID uint32, maximum int) (*LaneLease, bool) {
	session.laneMu.Lock()
	defer session.laneMu.Unlock()
	if _, replacing := session.activeLanes[laneID]; !replacing &&
		maximum > 0 && len(session.activeLanes) >= maximum {
		return nil, false
	}
	session.nextLaneLease++
	generation := session.nextLaneLease
	session.activeLanes[laneID] = generation
	return &LaneLease{session: session, laneID: laneID, generation: generation}, true
}

func (lease *LaneLease) ReleaseLane() {
	if lease == nil || lease.session == nil {
		return
	}
	lease.once.Do(func() {
		lease.session.laneMu.Lock()
		if lease.session.activeLanes[lease.laneID] == lease.generation {
			delete(lease.session.activeLanes, lease.laneID)
		}
		lease.session.laneMu.Unlock()
	})
}

func NewSession(sid SessionID) *Session {
	session := &Session{
		sid:            sid,
		uploadPending:  make(map[uint64][]byte),
		activeLanes:    make(map[uint32]uint64),
		readySignal:    make(chan struct{}),
		downloadSignal: make(chan struct{}),
		downloadFrames: make(map[uint64][]byte),
	}
	session.touch()
	return session
}

func (session *Session) touch() {
	session.lastSeen.Store(time.Now().UnixNano())
}

func (session *Session) LastSeen() time.Time {
	return time.Unix(0, session.lastSeen.Load())
}

func (session *Session) signalReadyLocked() {
	close(session.readySignal)
	session.readySignal = make(chan struct{})
}

func (session *Session) signalDownloadLocked() {
	close(session.downloadSignal)
	session.downloadSignal = make(chan struct{})
}

func (session *Session) OpenTarget(address string, dialer func(string) (net.Conn, error)) error {
	session.openOnce.Do(func() {
		target, err := dialer(address)
		if err != nil {
			session.openErr = err
			session.readyMu.Lock()
			session.targetError = err
			session.targetEOF = true
			session.signalReadyLocked()
			session.readyMu.Unlock()
			return
		}
		session.target = target
		go session.readTarget()
	})
	return session.openErr
}

func (session *Session) readTarget() {
	buffer := make([]byte, 65536)
	for {
		count, err := session.target.Read(buffer)
		if count > 0 {
			session.readyMu.Lock()
			session.downloadBuffer.append(buffer[:count])
			session.touch()
			session.signalReadyLocked()
			session.readyMu.Unlock()
		}
		if err != nil {
			session.readyMu.Lock()
			if !errors.Is(err, io.EOF) {
				session.targetError = err
			}
			session.targetEOF = true
			session.signalReadyLocked()
			session.readyMu.Unlock()

			session.downloadMu.Lock()
			session.downloadClosed = true
			session.signalDownloadLocked()
			session.downloadMu.Unlock()
			return
		}
	}
}

func (session *Session) WriteUpload(sequence uint64, payload []byte) error {
	session.uploadMu.Lock()
	defer session.uploadMu.Unlock()
	if sequence < session.uploadNext {
		return nil
	}
	if _, exists := session.uploadPending[sequence]; !exists {
		session.uploadPending[sequence] = append([]byte(nil), payload...)
	}
	for {
		chunk, exists := session.uploadPending[session.uploadNext]
		if !exists {
			return nil
		}
		delete(session.uploadPending, session.uploadNext)
		if len(chunk) > 0 && session.target != nil {
			if err := writeAll(session.target, chunk); err != nil {
				return err
			}
		}
		session.uploadNext++
		session.touch()
	}
}

// UploadCommitted reports the highest contiguous upload sequence written to
// the target. Receipt alone is not a commit when lanes arrive out of order.
func (session *Session) UploadCommitted() (uint64, bool) {
	session.uploadMu.Lock()
	defer session.uploadMu.Unlock()
	if session.uploadNext == 0 {
		return 0, false
	}
	return session.uploadNext - 1, true
}

func (session *Session) TakeDownload(sequence uint64, limit int, bannerTimeout time.Duration) []byte {
	if limit < 1 {
		limit = 1
	}
	if limit > MaxDownloadSize {
		limit = MaxDownloadSize
	}
	deadline := time.NewTimer(bannerTimeout)
	defer deadline.Stop()

	session.readyMu.Lock()
	defer session.readyMu.Unlock()
	if !session.bannerDelivered && sequence != 0 {
		return nil
	}
	for session.downloadBuffer.available() == 0 && !session.targetEOF {
		if session.bannerDelivered {
			return nil
		}
		signal := session.readySignal
		session.readyMu.Unlock()
		select {
		case <-signal:
		case <-deadline.C:
			session.readyMu.Lock()
			return nil
		}
		session.readyMu.Lock()
	}
	chunk := session.downloadBuffer.take(limit)
	if len(chunk) == 0 {
		return nil
	}
	session.bannerDelivered = true
	session.touch()
	return chunk
}

func (session *Session) AssignDownload(sequence uint64, count int, limit int) [][]byte {
	session.downloadMu.Lock()
	for sequence > session.downloadNext && !session.downloadClosed {
		signal := session.downloadSignal
		session.downloadMu.Unlock()
		<-signal
		session.downloadMu.Lock()
	}
	isNew := sequence == session.downloadNext
	chunks := make([][]byte, count)
	for offset := 0; offset < count; offset++ {
		current := sequence + uint64(offset)
		if isNew {
			chunk := session.TakeDownload(current, limit, 5*time.Second)
			if len(chunk) > 0 {
				session.downloadFrames[current] = chunk
			}
			chunks[offset] = chunk
		} else {
			chunks[offset] = append([]byte(nil), session.downloadFrames[current]...)
		}
	}
	if isNew {
		session.downloadNext += uint64(count)
		session.signalDownloadLocked()
	}
	session.downloadMu.Unlock()
	return chunks
}

func (session *Session) Acknowledge(sequence uint64) {
	session.downloadMu.Lock()
	defer session.downloadMu.Unlock()
	if session.hasACK && sequence <= session.downloadAcked {
		return
	}
	session.hasACK = true
	session.downloadAcked = sequence
	for number := range session.downloadFrames {
		if number <= sequence {
			delete(session.downloadFrames, number)
		}
	}
}

func (session *Session) Close() {
	session.closeOnce.Do(func() {
		if session.target != nil {
			_ = session.target.Close()
		}
		session.readyMu.Lock()
		session.targetEOF = true
		session.signalReadyLocked()
		session.readyMu.Unlock()
		session.downloadMu.Lock()
		session.downloadClosed = true
		session.signalDownloadLocked()
		session.downloadMu.Unlock()
	})
}

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[SessionID]*Session
	timeout  time.Duration
}

func NewSessionManager(timeout time.Duration) *SessionManager {
	return &SessionManager{sessions: make(map[SessionID]*Session), timeout: timeout}
}

func (manager *SessionManager) GetOrCreate(sid SessionID) *Session {
	session, _ := manager.GetOrCreateState(sid)
	return session
}

func (manager *SessionManager) GetOrCreateState(sid SessionID) (*Session, bool) {
	manager.mu.RLock()
	session := manager.sessions[sid]
	manager.mu.RUnlock()
	if session != nil {
		return session, true
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if session = manager.sessions[sid]; session != nil {
		return session, true
	}
	if session == nil {
		session = NewSession(sid)
		manager.sessions[sid] = session
	}
	return session, false
}

func (manager *SessionManager) Get(sid SessionID) (*Session, bool) {
	manager.mu.RLock()
	session := manager.sessions[sid]
	manager.mu.RUnlock()
	return session, session != nil
}

func (manager *SessionManager) Cleanup(now time.Time) int {
	manager.mu.Lock()
	var stale []*Session
	for sid, session := range manager.sessions {
		if now.Sub(session.LastSeen()) > manager.timeout {
			delete(manager.sessions, sid)
			stale = append(stale, session)
		}
	}
	manager.mu.Unlock()
	for _, session := range stale {
		session.Close()
	}
	return len(stale)
}

func (manager *SessionManager) Close() {
	manager.mu.Lock()
	sessions := make([]*Session, 0, len(manager.sessions))
	for sid, session := range manager.sessions {
		delete(manager.sessions, sid)
		sessions = append(sessions, session)
	}
	manager.mu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}
