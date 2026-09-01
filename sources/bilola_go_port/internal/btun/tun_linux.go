//go:build linux

package btun

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	tunSetIFF = 0x400454ca
	iffTUN    = 0x0001
	iffNoPI   = 0x1000
)

type linuxTUN struct {
	file       *os.File
	fd         int
	name       string
	closed     chan struct{}
	readerDone chan struct{}
	packets    chan []byte
	readErrors chan error
	fdMu       sync.RWMutex
	closeOnce  sync.Once
	closeErr   error
}

func OpenTUN(requestedName string) (PacketDevice, error) {
	if len(requestedName) == 0 || len(requestedName) >= 16 {
		return nil, fmt.Errorf("invalid TUN interface name %q", requestedName)
	}
	file, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var request [40]byte
	copy(request[:16], requestedName)
	*(*uint16)(unsafe.Pointer(&request[16])) = iffTUN | iffNoPI
	fd := int(file.Fd())
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tunSetIFF, uintptr(unsafe.Pointer(&request[0])))
	if errno != 0 {
		file.Close()
		return nil, fmt.Errorf("TUNSETIFF %s: %w", requestedName, errno)
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		file.Close()
		return nil, fmt.Errorf("set TUN nonblocking: %w", err)
	}
	nameLength := bytes.IndexByte(request[:16], 0)
	if nameLength < 0 {
		nameLength = 16
	}
	actual := string(request[:nameLength])
	tun := &linuxTUN{
		file: file, fd: fd, name: actual, closed: make(chan struct{}),
		readerDone: make(chan struct{}), packets: make(chan []byte, 64),
		readErrors: make(chan error, 1),
	}
	go tun.readLoop()
	return tun, nil
}

func (tun *linuxTUN) Read(buffer []byte) (int, error) {
	select {
	case packet := <-tun.packets:
		if len(packet) > len(buffer) {
			return 0, io.ErrShortBuffer
		}
		return copy(buffer, packet), nil
	case err := <-tun.readErrors:
		return 0, err
	case <-tun.closed:
		return 0, os.ErrClosed
	}
}

func (tun *linuxTUN) readLoop() {
	defer close(tun.readerDone)
	buffer := make([]byte, DefaultMaxPacket)
	for {
		select {
		case <-tun.closed:
			return
		default:
		}
		tun.fdMu.RLock()
		length, err := syscall.Read(tun.fd, buffer)
		tun.fdMu.RUnlock()
		if err == nil && length > 0 {
			packet := append([]byte(nil), buffer[:length]...)
			select {
			case tun.packets <- packet:
			case <-tun.closed:
				return
			}
			continue
		}
		if err == nil {
			continue
		}
		if err == syscall.EINTR {
			continue
		}
		if err != syscall.EAGAIN && err != syscall.EWOULDBLOCK {
			select {
			case tun.readErrors <- err:
			case <-tun.closed:
			}
			return
		}
		select {
		case <-tun.closed:
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (tun *linuxTUN) Write(buffer []byte) (int, error) {
	for {
		select {
		case <-tun.closed:
			return 0, os.ErrClosed
		default:
		}
		tun.fdMu.RLock()
		length, err := syscall.Write(tun.fd, buffer)
		tun.fdMu.RUnlock()
		if err == nil {
			return length, nil
		}
		if err == syscall.EINTR {
			continue
		}
		if err != syscall.EAGAIN && err != syscall.EWOULDBLOCK {
			return 0, err
		}
		select {
		case <-tun.closed:
			return 0, os.ErrClosed
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (tun *linuxTUN) Close() error {
	tun.closeOnce.Do(func() {
		close(tun.closed)
		<-tun.readerDone
		tun.fdMu.Lock()
		tun.closeErr = tun.file.Close()
		tun.fdMu.Unlock()
	})
	return tun.closeErr
}

func (tun *linuxTUN) Name() string { return tun.name }
