package btun

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

const (
	PacketAuth         byte = 1
	PacketAuthResponse byte = 2
	PacketIP           byte = 3
	PacketData         byte = 4
	PacketKeepAlive    byte = 5

	HeaderSize       = 6
	DefaultMaxPacket = 64 * 1024
)

var (
	ClientHello = []byte("DTUNNEL/1.1 CLIENT_HELLO\r\n")
	ServerHello = []byte("DTUNNEL/1.1 SERVER_HELLO\r\n")
)

type Packet struct {
	Type    byte
	Flags   byte
	Payload []byte
}

func MarshalPacket(packet Packet, maxPayload int) ([]byte, error) {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxPacket
	}
	if len(packet.Payload) > maxPayload {
		return nil, fmt.Errorf("packet payload too large: %d", len(packet.Payload))
	}
	encoded := make([]byte, HeaderSize+len(packet.Payload))
	encoded[0] = packet.Type
	encoded[1] = packet.Flags
	binary.BigEndian.PutUint32(encoded[2:6], uint32(len(packet.Payload)))
	copy(encoded[HeaderSize:], packet.Payload)
	return encoded, nil
}

func ReadPacket(reader io.Reader, maxPayload int) (Packet, error) {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxPacket
	}
	var header [HeaderSize]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return Packet{}, err
	}
	length := int(binary.BigEndian.Uint32(header[2:6]))
	if length > maxPayload {
		return Packet{}, fmt.Errorf("packet payload too large: %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return Packet{}, err
	}
	return Packet{Type: header[0], Flags: header[1], Payload: payload}, nil
}

func ParseDatagram(datagram []byte, maxPayload int) (Packet, error) {
	if len(datagram) < HeaderSize {
		return Packet{}, errors.New("datagram is shorter than the packet header")
	}
	if maxPayload <= 0 {
		maxPayload = DefaultMaxPacket
	}
	length := int(binary.BigEndian.Uint32(datagram[2:6]))
	if length > maxPayload {
		return Packet{}, fmt.Errorf("packet payload too large: %d", length)
	}
	if len(datagram) != HeaderSize+length {
		return Packet{}, fmt.Errorf("invalid datagram length: got %d, want %d", len(datagram), HeaderSize+length)
	}
	payload := append([]byte(nil), datagram[HeaderSize:]...)
	return Packet{Type: datagram[0], Flags: datagram[1], Payload: payload}, nil
}

func ReadClientHello(reader io.Reader, maxCoverBytes int) (int, error) {
	if maxCoverBytes < 0 {
		maxCoverBytes = 0
	}
	readLimit := maxCoverBytes + len(ClientHello)
	window := make([]byte, 0, len(ClientHello))
	var one [1]byte
	for consumed := 0; consumed < readLimit; consumed++ {
		if _, err := io.ReadFull(reader, one[:]); err != nil {
			return consumed, err
		}
		if len(window) == cap(window) {
			copy(window, window[1:])
			window = window[:len(window)-1]
		}
		window = append(window, one[0])
		if bytes.Equal(window, ClientHello) {
			return consumed + 1 - len(ClientHello), nil
		}
	}
	return maxCoverBytes, errors.New("client hello not found")
}

func ParseCredential(payload []byte) (string, string, error) {
	separator := bytes.IndexByte(payload, ':')
	if separator <= 0 {
		return "", "", errors.New("invalid credential")
	}
	username := string(payload[:separator])
	password := string(payload[separator+1:])
	if len(username) > 256 || len(password) > 4096 || bytes.IndexByte(payload, 0) >= 0 {
		return "", "", errors.New("invalid credential")
	}
	return username, password, nil
}

func AuthResponse(accepted bool, message string) Packet {
	payload := make([]byte, 1, 1+len(message))
	if accepted {
		payload[0] = 1
	}
	payload = append(payload, message...)
	return Packet{Type: PacketAuthResponse, Payload: payload}
}

func ParseIPv4Packet(packet []byte) (source [4]byte, destination [4]byte, ok bool) {
	source, destination, _, ok = parseIPv4Packet(packet)
	return source, destination, ok
}

func parseIPv4Packet(packet []byte) (source [4]byte, destination [4]byte, totalLength int, ok bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return source, destination, 0, false
	}
	headerLength := int(packet[0]&0x0f) * 4
	if headerLength < 20 || headerLength > len(packet) {
		return source, destination, 0, false
	}
	totalLength = int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLength < headerLength || totalLength > len(packet) {
		return source, destination, 0, false
	}
	copy(source[:], packet[12:16])
	copy(destination[:], packet[16:20])
	return source, destination, totalLength, true
}

func ipKey(ip net.IP) ([4]byte, bool) {
	var key [4]byte
	v4 := ip.To4()
	if v4 == nil {
		return key, false
	}
	copy(key[:], v4)
	return key, true
}
