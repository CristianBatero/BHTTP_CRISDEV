package btun

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeTUN struct {
	reads  chan []byte
	writes chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFakeTUN() *fakeTUN {
	return &fakeTUN{reads: make(chan []byte, 4), writes: make(chan []byte, 4), closed: make(chan struct{})}
}

func (tun *fakeTUN) Name() string { return "test0" }
func (tun *fakeTUN) Read(buffer []byte) (int, error) {
	select {
	case packet := <-tun.reads:
		return copy(buffer, packet), nil
	case <-tun.closed:
		return 0, io.EOF
	}
}
func (tun *fakeTUN) Write(packet []byte) (int, error) {
	copyOfPacket := append([]byte(nil), packet...)
	select {
	case tun.writes <- copyOfPacket:
		return len(packet), nil
	case <-tun.closed:
		return 0, io.ErrClosedPipe
	}
}
func (tun *fakeTUN) Close() error {
	tun.once.Do(func() { close(tun.closed) })
	return nil
}

func TestTCPAuthenticationAndPacketRouting(t *testing.T) {
	tun := newFakeTUN()
	server, err := NewServer(Config{
		Subnet: "10.77.0.0/29", Authenticator: AllowAuthenticator{},
		Logger: log.New(io.Discard, "", 0), HandshakeTimeout: time.Second,
		IdleTimeout: time.Minute,
	}, tun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		server.Wait()
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ServeTCP(listener) }()
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))

	if _, err := connection.Write(append([]byte("cover request\r\n"), ClientHello...)); err != nil {
		t.Fatal(err)
	}
	hello := make([]byte, len(ServerHello))
	if _, err := io.ReadFull(connection, hello); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hello, ServerHello) {
		t.Fatalf("server hello = %q", hello)
	}
	writeTestPacket(t, connection, Packet{Type: PacketAuth, Payload: []byte("user:password")})
	auth := readTestPacket(t, connection)
	if auth.Type != PacketAuthResponse || len(auth.Payload) == 0 || auth.Payload[0] != 1 {
		t.Fatalf("unexpected auth response: %#v", auth)
	}
	writeTestPacket(t, connection, Packet{Type: PacketIP})
	ipResponse := readTestPacket(t, connection)
	if ipResponse.Type != PacketIP || len(ipResponse.Payload) != 4 {
		t.Fatalf("unexpected IP response: %#v", ipResponse)
	}
	assigned := net.IP(ipResponse.Payload)
	// DT5 may emit a non-IPv4 frame while its TUN is coming up. The server
	// must drop it without destroying the authenticated stream.
	writeTestPacket(t, connection, Packet{Type: PacketData, Payload: []byte{0x60, 0, 0, 0}})
	outbound := ipv4Packet(assigned, net.IPv4(1, 1, 1, 1), []byte("out"))
	writeTestPacket(t, connection, Packet{Type: PacketData, Payload: outbound})
	select {
	case got := <-tun.writes:
		if !bytes.Equal(got, outbound) {
			t.Fatal("outbound TUN packet mismatch")
		}
	case <-time.After(time.Second):
		t.Fatal("outbound packet did not reach TUN")
	}
	inbound := ipv4Packet(net.IPv4(1, 1, 1, 1), assigned, []byte("in"))
	tun.reads <- inbound
	data := readTestPacket(t, connection)
	if data.Type != PacketData || !bytes.Equal(data.Payload, inbound) {
		t.Fatalf("unexpected inbound packet: %#v", data)
	}

	_ = server.Close()
	server.Wait()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestUDPAuthenticationAndPacketRouting(t *testing.T) {
	tun := newFakeTUN()
	server, err := NewServer(Config{
		Subnet: "10.77.1.0/29", Authenticator: AllowAuthenticator{},
		Logger: log.New(io.Discard, "", 0), IdleTimeout: time.Minute,
	}, tun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		server.Wait()
	})
	packetConnection, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ServeUDP(packetConnection) }()
	connection, err := net.Dial("udp", packetConnection.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))

	if _, err := connection.Write(ClientHello); err != nil {
		t.Fatal(err)
	}
	hello := make([]byte, len(ServerHello))
	if _, err := io.ReadFull(connection, hello); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hello, ServerHello) {
		t.Fatalf("server hello = %q", hello)
	}
	writeUDPTestPacket(t, connection, Packet{Type: PacketAuth, Payload: []byte("user:password")})
	auth := readUDPTestPacket(t, connection)
	if auth.Type != PacketAuthResponse || len(auth.Payload) == 0 || auth.Payload[0] != 1 {
		t.Fatalf("unexpected auth response: %#v", auth)
	}
	writeUDPTestPacket(t, connection, Packet{Type: PacketIP})
	ipResponse := readUDPTestPacket(t, connection)
	if ipResponse.Type != PacketIP || len(ipResponse.Payload) != 4 {
		t.Fatalf("unexpected IP response: %#v", ipResponse)
	}
	assigned := net.IP(ipResponse.Payload)
	outbound := ipv4Packet(assigned, net.IPv4(8, 8, 8, 8), []byte("udp-out"))
	writeUDPTestPacket(t, connection, Packet{Type: PacketData, Payload: outbound})
	select {
	case got := <-tun.writes:
		if !bytes.Equal(got, outbound) {
			t.Fatal("outbound UDP TUN packet mismatch")
		}
	case <-time.After(time.Second):
		t.Fatal("outbound UDP packet did not reach TUN")
	}
	inbound := ipv4Packet(net.IPv4(8, 8, 8, 8), assigned, []byte("udp-in"))
	tun.reads <- inbound
	data := readUDPTestPacket(t, connection)
	if data.Type != PacketData || !bytes.Equal(data.Payload, inbound) {
		t.Fatalf("unexpected inbound UDP packet: %#v", data)
	}

	_ = server.Close()
	server.Wait()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func writeTestPacket(t *testing.T, writer io.Writer, packet Packet) {
	t.Helper()
	encoded, err := MarshalPacket(packet, DefaultMaxPacket)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFull(writer, encoded); err != nil {
		t.Fatal(err)
	}
}

func readTestPacket(t *testing.T, reader io.Reader) Packet {
	t.Helper()
	packet, err := ReadPacket(reader, DefaultMaxPacket)
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func writeUDPTestPacket(t *testing.T, writer io.Writer, packet Packet) {
	t.Helper()
	encoded, err := MarshalPacket(packet, DefaultMaxPacket)
	if err != nil {
		t.Fatal(err)
	}
	written, err := writer.Write(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if written != len(encoded) {
		t.Fatal(io.ErrShortWrite)
	}
}

func readUDPTestPacket(t *testing.T, reader io.Reader) Packet {
	t.Helper()
	buffer := make([]byte, DefaultMaxPacket+HeaderSize)
	length, err := reader.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := ParseDatagram(buffer[:length], DefaultMaxPacket)
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func ipv4Packet(source, destination net.IP, payload []byte) []byte {
	packet := make([]byte, 20+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[8] = 64
	packet[9] = 17
	copy(packet[12:16], source.To4())
	copy(packet[16:20], destination.To4())
	copy(packet[20:], payload)
	return packet
}
