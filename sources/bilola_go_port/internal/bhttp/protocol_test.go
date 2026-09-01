package bhttp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"testing"
	"time"
)

func TestCryptMatchesRestoredPythonServer(t *testing.T) {
	var sid SessionID
	for index := range sid {
		sid[index] = byte(index)
	}
	tests := []struct {
		clearHex string
		mode     byte
		seq      uint64
		response bool
		wantHex  string
	}{
		{"42696c6f6c612d4248545450", 1, 0, false, "7ca4d6c962b839cf1fb68990"},
		{"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f", 3, 123456789, true, "ccd73e4b14777caaa138d17c7016770da44fe50c4d7322a759298098a47230347567293f767bb2f8069a2ede76ac97ec5e031832b023b44f69918e02c6f313ea"},
		{"42485031010300000008", 0, 0, false, "1185931614521a40b889"},
	}
	for _, test := range tests {
		clear, _ := hex.DecodeString(test.clearHex)
		want, _ := hex.DecodeString(test.wantHex)
		got := Crypt(clear, sid, test.mode, test.seq, test.response)
		if !bytes.Equal(got, want) {
			t.Fatalf("mode=%d seq=%d got=%x want=%x", test.mode, test.seq, got, want)
		}
		if decoded := Crypt(got, sid, test.mode, test.seq, test.response); !bytes.Equal(decoded, clear) {
			t.Fatalf("cipher did not round-trip mode=%d seq=%d", test.mode, test.seq)
		}
	}
}

func writeTestRequest(writer io.Writer, mode byte, sid SessionID, sequence uint64, payload []byte, value uint32) error {
	if mode != ModeDownload && mode != ModeACK {
		value = uint32(len(payload))
	}
	packet := make([]byte, HeaderSize+len(payload))
	packet[0] = mode
	copy(packet[1:17], sid[:])
	binary.BigEndian.PutUint64(packet[17:25], sequence)
	binary.BigEndian.PutUint32(packet[25:29], value)
	copy(packet[29:], payload)
	return writeAll(writer, packet)
}

func readTestResponse(reader io.Reader) (byte, []byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:5])
	body := make([]byte, int(size))
	if _, err := io.ReadFull(reader, body); err != nil {
		return 0, nil, nil, err
	}
	raw := append(append([]byte(nil), header[:]...), body...)
	return header[0], body, raw, nil
}

func waitBuffered(t *testing.T, session *Session, minimum int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session.readyMu.Lock()
		available := session.downloadBuffer.available()
		session.readyMu.Unlock()
		if available >= minimum {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("target bytes were not buffered")
}

func TestServerSSHRelayBatchRetryAndACK(t *testing.T) {
	targetServer, targetPeer := net.Pipe()
	defer targetPeer.Close()
	dialed := false
	server := NewServer(Config{
		TargetAddress: "test:22",
		DialTarget: func(address string) (net.Conn, error) {
			if address != "test:22" || dialed {
				t.Fatalf("unexpected target dial %q repeated=%v", address, dialed)
			}
			dialed = true
			return targetServer, nil
		},
	})
	defer server.Close()

	accepted, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- server.handleConnection(accepted)
		accepted.Close()
	}()
	defer func() {
		client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("connection handler did not stop")
		}
	}()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))

	var sid SessionID
	copy(sid[:], []byte("bilola-test-sid!"))
	if err := writeTestRequest(client, ModeUpload, sid, 0, nil, 0); err != nil {
		t.Fatal(err)
	}
	status, _, _, err := readTestResponse(client)
	if err != nil || status != StatusOK {
		t.Fatalf("registration status=%d err=%v", status, err)
	}
	session := server.sessions.GetOrCreate(sid)

	banner := []byte("SSH-2.0-BilolaGo\r\n")
	go targetPeer.Write(banner)
	waitBuffered(t, session, len(banner))
	if err := writeTestRequest(client, ModeDownload, sid, 0, nil, 65536); err != nil {
		t.Fatal(err)
	}
	status, body, _, err := readTestResponse(client)
	if err != nil || status != StatusData || len(body) < 4 {
		t.Fatalf("banner response status=%d len=%d err=%v", status, len(body), err)
	}
	plainSize := int(binary.BigEndian.Uint32(body[:4]))
	decoded := Crypt(body[4:], sid, ModeDownload, 0, true)
	if plainSize != len(banner) || !bytes.Equal(decoded, banner) {
		t.Fatalf("banner got=%q size=%d", decoded, plainSize)
	}

	upload := []byte("SSH upload sequence zero")
	encryptedUpload := Crypt(upload, sid, ModeUpload, 0, false)
	targetUpload := make(chan []byte, 1)
	go func() {
		read := make([]byte, len(upload))
		if _, readErr := io.ReadFull(targetPeer, read); readErr != nil {
			targetUpload <- nil
			return
		}
		targetUpload <- read
	}()
	if err := writeTestRequest(client, ModeUpload, sid, 0, encryptedUpload, 0); err != nil {
		t.Fatal(err)
	}
	status, _, _, err = readTestResponse(client)
	if err != nil || status != StatusOK {
		t.Fatalf("upload status=%d err=%v", status, err)
	}
	targetRead := <-targetUpload
	if !bytes.Equal(targetRead, upload) {
		t.Fatalf("target upload=%q", targetRead)
	}

	downstream := make([]byte, 3000)
	for index := range downstream {
		downstream[index] = byte(index * 17)
	}
	go targetPeer.Write(downstream)
	waitBuffered(t, session, len(downstream))
	batchClear := make([]byte, 6)
	binary.BigEndian.PutUint32(batchClear[:4], 1399)
	binary.BigEndian.PutUint16(batchClear[4:6], 3)
	batchRequest := Crypt(batchClear, sid, ModeBatch, 1, false)
	if err := writeTestRequest(client, ModeBatch, sid, 1, batchRequest, 0); err != nil {
		t.Fatal(err)
	}
	var firstRaw [][]byte
	var downloaded []byte
	var raw []byte
	for offset := 0; offset < 3; offset++ {
		status, body, raw, err = readTestResponse(client)
		if err != nil || status != StatusData || len(body) < 4 {
			t.Fatalf("batch %d status=%d len=%d err=%v", offset, status, len(body), err)
		}
		plainSize = int(binary.BigEndian.Uint32(body[:4]))
		decoded = Crypt(body[4:], sid, ModeBatch, 1+uint64(offset), true)
		if len(decoded) != plainSize {
			t.Fatalf("batch %d declared=%d decoded=%d", offset, plainSize, len(decoded))
		}
		downloaded = append(downloaded, decoded...)
		firstRaw = append(firstRaw, raw)
	}
	if !bytes.Equal(downloaded, downstream) {
		t.Fatalf("batch data mismatch got=%d want=%d", len(downloaded), len(downstream))
	}

	if err := writeTestRequest(client, ModeBatch, sid, 1, batchRequest, 0); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < 3; offset++ {
		_, _, raw, err := readTestResponse(client)
		if err != nil || !bytes.Equal(raw, firstRaw[offset]) {
			t.Fatalf("retry frame %d changed err=%v", offset, err)
		}
	}

	if err := writeTestRequest(client, ModeACK, sid, 3, nil, 0); err != nil {
		t.Fatal(err)
	}
	status, _, _, err = readTestResponse(client)
	if err != nil || status != StatusOK {
		t.Fatalf("ACK status=%d err=%v", status, err)
	}
	session.downloadMu.Lock()
	remaining := len(session.downloadFrames)
	session.downloadMu.Unlock()
	if remaining != 0 {
		t.Fatalf("ACK retained %d replay frames", remaining)
	}
}

func TestProbeEchoMatchesBHP1(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		request, err := ReadRequest(server)
		if err == nil {
			err = handleProbe(server, request)
		}
		done <- err
		server.Close()
	}()
	defer client.Close()
	var sid SessionID
	for index := range sid {
		sid[index] = byte(index)
	}
	clear := []byte{'B', 'H', 'P', '1', 1, ModeDownload, 0, 0, 2, 0}
	encrypted := Crypt(clear, sid, ModeProbe, 7, false)
	if err := writeTestRequest(client, ModeProbe, sid, 7, encrypted, 0); err != nil {
		t.Fatal(err)
	}
	status, body, _, err := readTestResponse(client)
	if err != nil || status != StatusOK {
		t.Fatalf("probe status=%d err=%v", status, err)
	}
	decoded := Crypt(body, sid, ModeProbe, 7, true)
	if len(decoded) != 512 || !bytes.Equal(decoded[:10], clear) {
		t.Fatalf("probe response len=%d prefix=%x", len(decoded), decoded[:10])
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
