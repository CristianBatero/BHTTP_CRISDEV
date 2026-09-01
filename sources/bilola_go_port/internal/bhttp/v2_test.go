package bhttp

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func v2Request(mode byte, sid SessionID, sequence uint64, clear []byte) Request {
	return Request{
		Mode: mode, SID: sid, Seq: sequence,
		Payload: Crypt(clear, sid, mode, sequence, false),
	}
}

func TestBHTTPV2CapabilityIsExplicitAndVersioned(t *testing.T) {
	var sid SessionID
	copy(sid[:], "capability-v2")
	clear := make([]byte, 10)
	copy(clear[:4], "BHP2")
	clear[4] = bhttpV2Version
	request := v2Request(ModeProbe, sid, 0, clear)
	var response bytes.Buffer
	if err := handleProbe(&response, request); err != nil {
		t.Fatal(err)
	}
	status, body, _, err := readTestResponse(&response)
	if err != nil || status != StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	decoded := Crypt(body, sid, ModeProbe, 0, true)
	if len(decoded) != 14 || string(decoded[:4]) != "BHP2" || decoded[4] != 2 {
		t.Fatalf("invalid capability response %x", decoded)
	}
	features := binary.BigEndian.Uint32(decoded[6:10])
	if features != bhttpV2FeatureLanes|bhttpV2FeatureResume|bhttpV2FeatureAck {
		t.Fatalf("features=%x", features)
	}
	if maximum := binary.BigEndian.Uint32(decoded[10:14]); maximum != bhttpV2MaxLanes {
		t.Fatalf("max lanes=%d", maximum)
	}
}

func TestBHTTPV2LaneResumeAndCommittedUploadACK(t *testing.T) {
	targetServer, targetPeer := net.Pipe()
	defer targetPeer.Close()
	server := NewServer(Config{DialTarget: func(string) (net.Conn, error) {
		return targetServer, nil
	}})
	defer server.Close()
	var sid SessionID
	copy(sid[:], "lane-resume-v2!!")

	hello := make([]byte, 12)
	copy(hello[:4], "BLN2")
	hello[4] = 2
	hello[5] = laneUpload
	binary.BigEndian.PutUint32(hello[8:], 7)
	request := v2Request(ModeLane, sid, 7, hello)
	var first bytes.Buffer
	role, firstSession, err := server.attachLane(&first, request)
	if err != nil || role != laneUpload {
		t.Fatalf("first attach role=%d err=%v", role, err)
	}
	defer firstSession.ReleaseLane()
	_, firstBody, _, err := readTestResponse(&first)
	if err != nil {
		t.Fatal(err)
	}
	firstDecoded := Crypt(firstBody, sid, ModeLane, 7, true)
	if firstDecoded[5] != 0 {
		t.Fatal("first lane unexpectedly resumed")
	}

	hello[7] = 1
	request = v2Request(ModeLane, sid, 8, hello)
	var resumed bytes.Buffer
	_, resumedSession, err := server.attachLane(&resumed, request)
	if err != nil {
		t.Fatal(err)
	}
	defer resumedSession.ReleaseLane()
	_, resumedBody, _, _ := readTestResponse(&resumed)
	resumedDecoded := Crypt(resumedBody, sid, ModeLane, 8, true)
	if resumedDecoded[5] != 1 {
		t.Fatal("replacement lane did not resume existing SID")
	}

	session := server.sessions.GetOrCreate(sid)
	var outOfOrder bytes.Buffer
	request = v2Request(ModeUpload, sid, 1, []byte("one"))
	if err := server.handleRequest(&outOfOrder, session, request, true); err != nil {
		t.Fatal(err)
	}
	_, ackBody, _, _ := readTestResponse(&outOfOrder)
	if len(ackBody) != 9 || ackBody[0] != 0 {
		t.Fatalf("out-of-order frame was incorrectly committed: %x", ackBody)
	}

	targetRead := make(chan []byte, 1)
	go func() {
		value := make([]byte, len("zeroone"))
		_, _ = io.ReadFull(targetPeer, value)
		targetRead <- value
	}()
	var ordered bytes.Buffer
	request = v2Request(ModeUpload, sid, 0, []byte("zero"))
	if err := server.handleRequest(&ordered, session, request, true); err != nil {
		t.Fatal(err)
	}
	_, ackBody, _, _ = readTestResponse(&ordered)
	if len(ackBody) != 9 || ackBody[0] != 1 || binary.BigEndian.Uint64(ackBody[1:]) != 1 {
		t.Fatalf("cumulative committed ACK invalid: %x", ackBody)
	}
	select {
	case value := <-targetRead:
		if string(value) != "zeroone" {
			t.Fatalf("target received %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("ordered upload did not reach target")
	}
}

func TestBHTTPV2ResumeNeverCreatesExpiredSession(t *testing.T) {
	server := NewServer(Config{})
	defer server.Close()
	var sid SessionID
	copy(sid[:], "expired-resume")
	hello := make([]byte, 12)
	copy(hello[:4], "BLN2")
	hello[4] = 2
	hello[5] = laneDownload
	hello[7] = 1
	var response bytes.Buffer
	if _, _, err := server.attachLane(&response, v2Request(ModeLane, sid, 1, hello)); err == nil {
		t.Fatal("expired resume was accepted")
	}
	if _, exists := server.sessions.Get(sid); exists {
		t.Fatal("resume rejection silently created a new session")
	}
}

func TestBHTTPV2PersistentLaneCarriesMultipleRequests(t *testing.T) {
	targetServer, targetPeer := net.Pipe()
	defer targetPeer.Close()
	server := NewServer(Config{DialTarget: func(string) (net.Conn, error) {
		return targetServer, nil
	}})
	defer server.Close()
	accepted, client := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.handleConnection(accepted) }()
	defer func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("persistent handler did not stop")
		}
	}()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	var sid SessionID
	copy(sid[:], "persistent-v2")
	hello := make([]byte, 12)
	copy(hello[:4], "BLN2")
	hello[4] = 2
	hello[5] = laneUpload
	binary.BigEndian.PutUint32(hello[8:], 11)
	request := v2Request(ModeLane, sid, 11, hello)
	if err := writeTestRequest(client, request.Mode, request.SID, request.Seq, request.Payload, 0); err != nil {
		t.Fatal(err)
	}
	status, _, _, err := readTestResponse(client)
	if err != nil || status != StatusOK {
		t.Fatalf("lane attach status=%d err=%v", status, err)
	}

	want := []byte("firstsecond")
	received := make(chan []byte, 1)
	go func() {
		value := make([]byte, len(want))
		_, _ = io.ReadFull(targetPeer, value)
		received <- value
	}()
	for sequence, clear := range [][]byte{[]byte("first"), []byte("second")} {
		request = v2Request(ModeUpload, sid, uint64(sequence), clear)
		if err := writeTestRequest(client, request.Mode, request.SID, request.Seq, request.Payload, 0); err != nil {
			t.Fatal(err)
		}
		status, body, _, readErr := readTestResponse(client)
		if readErr != nil || status != StatusOK || len(body) != 9 || body[0] != 1 {
			t.Fatalf("upload %d status=%d body=%x err=%v", sequence, status, body, readErr)
		}
		if committed := binary.BigEndian.Uint64(body[1:]); committed != uint64(sequence) {
			t.Fatalf("upload %d committed=%d", sequence, committed)
		}
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, want) {
			t.Fatalf("persistent lane target=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("persistent lane did not forward both requests")
	}
}

func TestBHTTPV2ReplayAfterLaneReplacementDoesNotDuplicateBytes(t *testing.T) {
	targetServer, targetPeer := net.Pipe()
	defer targetPeer.Close()
	server := NewServer(Config{DialTarget: func(string) (net.Conn, error) {
		return targetServer, nil
	}})
	defer server.Close()
	var sid SessionID
	copy(sid[:], "replay-lane-v2")
	hello := make([]byte, 12)
	copy(hello[:4], "BLN2")
	hello[4] = 2
	hello[5] = laneUpload
	firstRequest := v2Request(ModeLane, sid, 1, hello)
	var first bytes.Buffer
	_, firstSession, err := server.attachLane(&first, firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	firstSession.ReleaseLane()

	want := []byte("firstsecond")
	received := make(chan []byte, 1)
	go func() {
		value := make([]byte, len(want))
		_, _ = io.ReadFull(targetPeer, value)
		received <- value
	}()
	session := server.sessions.GetOrCreate(sid)
	var response bytes.Buffer
	if err := server.handleRequest(&response, session,
		v2Request(ModeUpload, sid, 0, []byte("first")), true); err != nil {
		t.Fatal(err)
	}

	hello[7] = 1
	var replacement bytes.Buffer
	_, replacementSession, err := server.attachLane(&replacement,
		v2Request(ModeLane, sid, 2, hello))
	if err != nil {
		t.Fatal(err)
	}
	defer replacementSession.ReleaseLane()
	response.Reset()
	if err := server.handleRequest(&response, session,
		v2Request(ModeUpload, sid, 0, []byte("first")), true); err != nil {
		t.Fatal(err)
	}
	response.Reset()
	if err := server.handleRequest(&response, session,
		v2Request(ModeUpload, sid, 1, []byte("second")), true); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, want) {
			t.Fatalf("replayed target bytes=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement lane did not finish upload")
	}
}

func TestBHTTPV2LaneLimitIsEnforced(t *testing.T) {
	targetServer, targetPeer := net.Pipe()
	defer targetPeer.Close()
	server := NewServer(Config{MaxV2Lanes: 1, DialTarget: func(string) (net.Conn, error) {
		return targetServer, nil
	}})
	defer server.Close()
	var sid SessionID
	copy(sid[:], "lane-limit-v2")
	hello := make([]byte, 12)
	copy(hello[:4], "BLN2")
	hello[4] = 2
	hello[5] = laneUpload
	var first bytes.Buffer
	_, session, err := server.attachLane(&first, v2Request(ModeLane, sid, 1, hello))
	if err != nil {
		t.Fatal(err)
	}
	defer session.ReleaseLane()
	hello[5] = laneDownload
	hello[7] = 1
	binary.BigEndian.PutUint32(hello[8:], 2)
	var rejected bytes.Buffer
	if _, _, err := server.attachLane(&rejected,
		v2Request(ModeLane, sid, 2, hello)); err == nil {
		t.Fatal("second lane exceeded configured limit")
	}
}

func TestBHTTPV2LaneReplacementKeepsSingleLogicalSlot(t *testing.T) {
	session := NewSession(SessionID{})
	first, ok := session.AcquireLane(7, 1)
	if !ok {
		t.Fatal("first logical lane was rejected")
	}
	replacement, ok := session.AcquireLane(7, 1)
	if !ok {
		t.Fatal("replacement of the same lane ID consumed another slot")
	}
	first.ReleaseLane()
	if _, ok := session.AcquireLane(8, 1); ok {
		t.Fatal("stale release removed the replacement generation")
	}
	replacement.ReleaseLane()
	if final, ok := session.AcquireLane(8, 1); !ok {
		t.Fatal("released replacement did not free its logical slot")
	} else {
		final.ReleaseLane()
	}
}

func TestServerCloseAbortsIdlePersistentConnections(t *testing.T) {
	server := NewServer(Config{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	deadline := time.Now().Add(time.Second)
	for {
		server.connMu.Lock()
		active := len(server.active)
		server.connMu.Unlock()
		if active == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not track idle connection")
		}
		time.Sleep(time.Millisecond)
	}
	_ = listener.Close()
	server.Close()
	waitDone := make(chan struct{})
	go func() {
		server.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("server shutdown waited on idle persistent connection")
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after listener close")
	}
}
