package btun

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

func TestPacketRoundTrip(t *testing.T) {
	want := Packet{Type: PacketData, Payload: []byte{1, 2, 3, 4}}
	encoded, err := MarshalPacket(want, 1024)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadPacket(bytes.NewReader(encoded), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != want.Type || got.Flags != want.Flags || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("packet mismatch: %#v != %#v", got, want)
	}
}

func TestReadClientHelloAfterCoverBytes(t *testing.T) {
	cover := []byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n")
	stream := append(append([]byte(nil), cover...), ClientHello...)
	consumed, err := ReadClientHello(bytes.NewReader(stream), len(cover))
	if err != nil {
		t.Fatal(err)
	}
	if consumed != len(cover) {
		t.Fatalf("cover bytes = %d, want %d", consumed, len(cover))
	}
}

func TestAddressPool(t *testing.T) {
	pool, err := NewAddressPool("10.77.0.0/29")
	if err != nil {
		t.Fatal(err)
	}
	first, err := pool.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(net.IPv4(10, 77, 0, 2)) {
		t.Fatalf("first address = %s", first)
	}
	pool.Release(first)
}

func TestIPv4ParserAcceptsTransportPadding(t *testing.T) {
	packet := make([]byte, 28)
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], 20)
	copy(packet[12:16], []byte{10, 77, 0, 2})
	copy(packet[16:20], []byte{1, 1, 1, 1})
	source, destination, totalLength, ok := parseIPv4Packet(packet)
	if !ok {
		t.Fatal("padded IPv4 packet was rejected")
	}
	if source != [4]byte{10, 77, 0, 2} || destination != [4]byte{1, 1, 1, 1} {
		t.Fatalf("unexpected addresses: %v -> %v", source, destination)
	}
	if totalLength != 20 {
		t.Fatalf("IPv4 length = %d, want 20", totalLength)
	}
}
