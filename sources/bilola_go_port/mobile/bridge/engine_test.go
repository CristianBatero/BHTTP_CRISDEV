package bridge

import (
	"bytes"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"bilola/internal/bhttp"
)

func TestEngineRelaysSSHThroughGoServer(t *testing.T) {
	targetServer, targetPeer := net.Pipe()
	defer targetPeer.Close()
	dialed := make(chan struct{}, 1)
	server := bhttp.NewServer(bhttp.Config{
		TargetAddress: "fake:22",
		Logger:        log.New(io.Discard, "", 0),
		DialTarget: func(address string) (net.Conn, error) {
			if address != "fake:22" {
				t.Fatalf("unexpected target %q", address)
			}
			select {
			case dialed <- struct{}{}:
			default:
				t.Fatal("target dialed more than once")
			}
			return targetServer, nil
		},
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		listener.Close()
		server.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Fatal("test BHTTP server did not stop")
		}
	}()

	address := listener.Addr().(*net.TCPAddr)
	engine, err := New(Config{
		Host:                "127.0.0.1",
		Port:                address.Port,
		UploadConnections:   2,
		DownloadConnections: 4,
		ConnectTimeout:      2 * time.Second,
		ReadTimeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	localPort, err := engine.Start()
	if err != nil {
		t.Fatalf("engine start: %v\nlogs:\n%s", err, engine.DrainLogs())
	}
	defer func() {
		engine.Close()
		engine.Wait()
	}()
	select {
	case <-dialed:
	case <-time.After(time.Second):
		t.Fatal("registration did not open target")
	}

	local, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1",
		fmtInt(localPort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	_ = local.SetDeadline(time.Now().Add(4 * time.Second))

	banner := []byte("SSH-2.0-GoJNI-test\r\n")
	go targetPeer.Write(banner)
	gotBanner := make([]byte, len(banner))
	if _, err := io.ReadFull(local, gotBanner); err != nil {
		t.Fatalf("read banner: %v\nlogs:\n%s", err, engine.DrainLogs())
	}
	if !bytes.Equal(gotBanner, banner) {
		t.Fatalf("banner=%q want=%q", gotBanner, banner)
	}

	upload := []byte("SSH-2.0-GoJNI-client\r\n")
	readUpload := make(chan []byte, 1)
	go func() {
		buffer := make([]byte, len(upload))
		if _, readErr := io.ReadFull(targetPeer, buffer); readErr != nil {
			readUpload <- nil
			return
		}
		readUpload <- buffer
	}()
	if _, err := local.Write(upload); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-readUpload:
		if !bytes.Equal(got, upload) {
			t.Fatalf("upload=%q want=%q", got, upload)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("upload timed out\nlogs:\n%s", engine.DrainLogs())
	}
	deadline := time.Now().Add(time.Second)
	for engine.UploadedBytes() != uint64(len(upload)) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if engine.UploadedBytes() != uint64(len(upload)) {
		t.Fatalf("uploaded counter=%d", engine.UploadedBytes())
	}
	if engine.DownloadedBytes() != uint64(len(banner)) {
		t.Fatalf("downloaded counter=%d", engine.DownloadedBytes())
	}
}

func TestSlotCountsAreNotCapped(t *testing.T) {
	engine, err := New(Config{
		Host: "example.invalid", Port: 80,
		UploadConnections: 40, DownloadConnections: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if engine.config.UploadConnections != 40 || engine.config.DownloadConnections != 64 {
		t.Fatalf("slot counts were capped: upload=%d download=%d",
			engine.config.UploadConnections, engine.config.DownloadConnections)
	}
}

func fmtInt(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
