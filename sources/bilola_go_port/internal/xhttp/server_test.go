package xhttp

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

const testSID = "0123456789abcdef0123456789abcdef"

type testHarness struct {
	t      *testing.T
	server *Server
	http   *httptest.Server
	client *http.Client
	target net.Conn
	close  sync.Once
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	serverTarget, testTarget := net.Pipe()
	var dialOnce sync.Once
	server := NewServer(Config{
		Logger: log.New(io.Discard, "", 0),
		DialTarget: func(string) (net.Conn, error) {
			var connection net.Conn
			dialOnce.Do(func() { connection = serverTarget })
			if connection == nil {
				return nil, fmt.Errorf("unexpected second target connection")
			}
			return connection, nil
		},
	})
	httpServer := httptest.NewUnstartedServer(server)
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	harness := &testHarness{t: t, server: server, http: httpServer, client: httpServer.Client(), target: testTarget}
	t.Cleanup(harness.Close)
	return harness
}

func (harness *testHarness) Close() {
	harness.close.Do(func() {
		_ = harness.target.Close()
		harness.http.CloseClientConnections()
		harness.http.Close()
		harness.server.Close()
	})
}

func (harness *testHarness) request(ctx context.Context, mode string, sequence *uint64, ack *uint64, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, harness.http.URL+"/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set(HeaderSID, testSID)
	request.Header.Set(HeaderMode, mode)
	request.Header.Set(HeaderVersion, ProtocolVersion)
	if sequence != nil {
		request.Header.Set(HeaderSequence, strconv.FormatUint(*sequence, 10))
	}
	if ack != nil {
		request.Header.Set(HeaderDownloadACK, strconv.FormatUint(*ack, 10))
	}
	if mode == "download" {
		request.Header.Set("Accept", "text/event-stream")
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set(HeaderDownloadFormat, "frame")
	}
	return harness.client.Do(request)
}

func readFrame(t *testing.T, reader io.Reader) (uint64, []byte) {
	t.Helper()
	header := make([]byte, 12)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	sequence := binary.BigEndian.Uint64(header[:8])
	length := binary.BigEndian.Uint32(header[8:])
	if length > maxDownloadFrame {
		t.Fatalf("frame too large: %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	return sequence, payload
}

func TestXHTTPUploadAndFramedDownload(t *testing.T) {
	harness := newTestHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	download, err := harness.request(ctx, "download", nil, nil, nil)
	if err != nil {
		t.Fatalf("start download: %v", err)
	}
	defer download.Body.Close()
	if download.ProtoMajor != 2 || download.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response: protocol=%s status=%d", download.Proto, download.StatusCode)
	}
	if got := download.Header.Get(HeaderVersion); got != ProtocolVersion {
		t.Fatalf("unexpected protocol version %q", got)
	}
	prefix := make([]byte, len(DownloadPrefix))
	if _, err := io.ReadFull(download.Body, prefix); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	if string(prefix) != DownloadPrefix {
		t.Fatalf("unexpected prefix %q", prefix)
	}

	downstream := []byte("SSH-2.0-OpenSSH_test\r\n")
	if _, err := harness.target.Write(downstream); err != nil {
		t.Fatalf("write target download: %v", err)
	}
	sequence, payload := readFrame(t, download.Body)
	if sequence != 0 || !bytes.Equal(payload, downstream) {
		t.Fatalf("unexpected frame: sequence=%d payload=%q", sequence, payload)
	}

	upload := []byte("SSH client key exchange")
	readResult := make(chan []byte, 1)
	go func() {
		buffer := make([]byte, len(upload))
		_, _ = io.ReadFull(harness.target, buffer)
		readResult <- buffer
	}()
	zero := uint64(0)
	response, err := harness.request(context.Background(), "upload", &zero, nil, upload)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get(HeaderUploadACK) != "1" {
		t.Fatalf("unexpected upload response: status=%d ack=%q", response.StatusCode, response.Header.Get(HeaderUploadACK))
	}
	select {
	case got := <-readResult:
		if !bytes.Equal(got, upload) {
			t.Fatalf("target received %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("target did not receive upload")
	}
}

func TestXHTTPAcceptsHTTP1OriginProxy(t *testing.T) {
	serverTarget, testTarget := net.Pipe()
	defer testTarget.Close()
	server := NewServer(Config{
		Logger: log.New(io.Discard, "", 0),
		DialTarget: func(string) (net.Conn, error) {
			return serverTarget, nil
		},
	})
	defer server.Close()
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(HeaderSID, testSID)
	request.Header.Set(HeaderMode, "ack")
	request.Header.Set(HeaderVersion, ProtocolVersion)
	request.Header.Set(HeaderDownloadACK, "0")
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatalf("HTTP/1.1 origin request: %v", err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 1 || response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected origin response: protocol=%s status=%d", response.Proto, response.StatusCode)
	}
	if got := response.Header.Get(HeaderVersion); got != ProtocolVersion {
		t.Fatalf("unexpected XHTTP version %q", got)
	}
}

func TestDeferredTargetSelectsBTUNOrSSHFromFirstUpload(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		payload     []byte
		wantAddress string
	}{
		{name: "BTUN", payload: []byte("DTUNNEL/1.1 CLIENT_HELLO\r\n"), wantAddress: "127.0.0.1:7300"},
		{name: "SSH", payload: []byte("SSH-2.0-test-client\r\n"), wantAddress: "127.0.0.1:22"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			serverTarget, testTarget := net.Pipe()
			defer testTarget.Close()
			dialed := make(chan string, 1)
			server := NewServer(Config{
				TargetAddress: "127.0.0.1:22",
				DeferTargetForRequest: func(*http.Request) bool {
					return true
				},
				TargetAddressForPayload: func(payload []byte) string {
					if bytes.Contains(payload, []byte("DTUNNEL/1.1 CLIENT_HELLO\r\n")) {
						return "127.0.0.1:7300"
					}
					return "127.0.0.1:22"
				},
				DeferredTargetDelay: time.Second,
				Logger:              log.New(io.Discard, "", 0),
				DialTarget: func(address string) (net.Conn, error) {
					dialed <- address
					return serverTarget, nil
				},
			})
			defer server.Close()
			httpServer := httptest.NewServer(server)
			defer httpServer.Close()

			received := make(chan []byte, 1)
			go func() {
				buffer := make([]byte, len(testCase.payload))
				_, _ = io.ReadFull(testTarget, buffer)
				received <- buffer
			}()
			request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/", bytes.NewReader(testCase.payload))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set(HeaderSID, testSID)
			request.Header.Set(HeaderMode, "upload")
			request.Header.Set(HeaderSequence, "0")
			response, err := httpServer.Client().Do(request)
			if err != nil {
				t.Fatalf("upload: %v", err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("upload status=%d", response.StatusCode)
			}
			select {
			case address := <-dialed:
				if address != testCase.wantAddress {
					t.Fatalf("dialed %q, want %q", address, testCase.wantAddress)
				}
			case <-time.After(time.Second):
				t.Fatal("target was not opened")
			}
			select {
			case payload := <-received:
				if !bytes.Equal(payload, testCase.payload) {
					t.Fatalf("target received %q", payload)
				}
			case <-time.After(time.Second):
				t.Fatal("target did not receive upload")
			}
		})
	}
}

func TestDeferredTargetFallsBackToSSHForDownloadFirst(t *testing.T) {
	serverTarget, testTarget := net.Pipe()
	defer testTarget.Close()
	dialed := make(chan string, 1)
	server := NewServer(Config{
		TargetAddress: "127.0.0.1:22",
		DeferTargetForRequest: func(*http.Request) bool {
			return true
		},
		DeferredTargetDelay: 10 * time.Millisecond,
		Logger:              log.New(io.Discard, "", 0),
		DialTarget: func(address string) (net.Conn, error) {
			dialed <- address
			return serverTarget, nil
		},
	})
	defer server.Close()
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(HeaderSID, testSID)
	request.Header.Set(HeaderMode, "ack")
	request.Header.Set(HeaderDownloadACK, "0")
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	_ = response.Body.Close()
	select {
	case address := <-dialed:
		if address != "127.0.0.1:22" {
			t.Fatalf("fallback dialed %q", address)
		}
	case <-time.After(time.Second):
		t.Fatal("SSH fallback did not open")
	}
}

func TestXHTTPOrdersConcurrentUploads(t *testing.T) {
	harness := newTestHarness(t)
	readResult := make(chan []byte, 1)
	go func() {
		buffer := make([]byte, 6)
		_, _ = io.ReadFull(harness.target, buffer)
		readResult <- buffer
	}()

	one := uint64(1)
	response, err := harness.request(context.Background(), "upload", &one, nil, []byte("def"))
	if err != nil {
		t.Fatalf("upload sequence 1: %v", err)
	}
	_ = response.Body.Close()
	if got := response.Header.Get(HeaderUploadACK); got != "0" {
		t.Fatalf("out-of-order ack=%q, want 0", got)
	}
	zero := uint64(0)
	response, err = harness.request(context.Background(), "upload", &zero, nil, []byte("abc"))
	if err != nil {
		t.Fatalf("upload sequence 0: %v", err)
	}
	_ = response.Body.Close()
	if got := response.Header.Get(HeaderUploadACK); got != "2" {
		t.Fatalf("ordered ack=%q, want 2", got)
	}
	select {
	case got := <-readResult:
		if string(got) != "abcdef" {
			t.Fatalf("target received %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordered upload was not delivered")
	}
}

func TestXHTTPDownloadResumeAndACK(t *testing.T) {
	harness := newTestHarness(t)
	firstContext, cancelFirst := context.WithCancel(context.Background())
	first, err := harness.request(firstContext, "download", nil, nil, nil)
	if err != nil {
		t.Fatalf("first download: %v", err)
	}
	prefix := make([]byte, len(DownloadPrefix))
	_, _ = io.ReadFull(first.Body, prefix)
	if _, err := harness.target.Write([]byte("first")); err != nil {
		t.Fatalf("write first frame: %v", err)
	}
	sequence, payload := readFrame(t, first.Body)
	if sequence != 0 || string(payload) != "first" {
		t.Fatalf("first frame sequence=%d payload=%q", sequence, payload)
	}
	cancelFirst()
	_ = first.Body.Close()

	one := uint64(1)
	secondContext, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	second, err := harness.request(secondContext, "download", nil, &one, nil)
	if err != nil {
		t.Fatalf("resumed download: %v", err)
	}
	defer second.Body.Close()
	_, _ = io.ReadFull(second.Body, prefix)
	if _, err := harness.target.Write([]byte("second")); err != nil {
		t.Fatalf("write second frame: %v", err)
	}
	sequence, payload = readFrame(t, second.Body)
	if sequence != 1 || string(payload) != "second" {
		t.Fatalf("resumed frame sequence=%d payload=%q", sequence, payload)
	}

	two := uint64(2)
	ackResponse, err := harness.request(context.Background(), "ack", nil, &two, nil)
	if err != nil {
		t.Fatalf("ack request: %v", err)
	}
	_ = ackResponse.Body.Close()
	if ackResponse.StatusCode != http.StatusOK || ackResponse.Header.Get(HeaderDownloadACK) != "2" {
		t.Fatalf("unexpected ACK response: status=%d ack=%q", ackResponse.StatusCode, ackResponse.Header.Get(HeaderDownloadACK))
	}

	harness.server.mu.Lock()
	current := harness.server.sessions[testSID]
	harness.server.mu.Unlock()
	current.downloadMu.Lock()
	defer current.downloadMu.Unlock()
	if current.downloadAcked != 2 || len(current.downloadFrames) != 0 {
		t.Fatalf("frames not released: ack=%d retained=%d", current.downloadAcked, len(current.downloadFrames))
	}
}
