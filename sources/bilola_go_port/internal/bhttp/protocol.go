package bhttp

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

const (
	ModeProbe    byte = 0
	ModeUpload   byte = 1
	ModeDownload byte = 2
	ModeBatch    byte = 3
	ModeACK      byte = 4
	ModeLane     byte = 5

	StatusOK    byte = 0
	StatusError byte = 1
	StatusData  byte = 2

	HeaderSize      = 29
	MaxPayload      = 524288
	MaxDownloadSize = 524284
)

type SessionID [16]byte

type Request struct {
	Mode    byte
	SID     SessionID
	Seq     uint64
	Value   uint32
	Payload []byte
}

func Crypt(data []byte, sid SessionID, mode byte, seq uint64, response bool) []byte {
	if len(data) == 0 {
		return nil
	}
	seed := make([]byte, 26)
	copy(seed[:16], sid[:])
	seed[16] = mode
	binary.BigEndian.PutUint64(seed[17:25], seq)
	if response {
		seed[25] = 1
	}
	out := make([]byte, len(data))
	blockInput := make([]byte, 30)
	copy(blockInput, seed)
	for offset, counter := 0, uint32(0); offset < len(data); offset, counter = offset+32, counter+1 {
		binary.BigEndian.PutUint32(blockInput[26:30], counter)
		key := sha256.Sum256(blockInput)
		end := offset + len(key)
		if end > len(data) {
			end = len(data)
		}
		for index := offset; index < end; index++ {
			out[index] = data[index] ^ key[index-offset]
		}
	}
	return out
}

func ReadRequest(reader io.Reader) (Request, error) {
	var request Request
	var header [HeaderSize]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return request, err
	}
	request.Mode = header[0]
	if request.Mode > ModeLane {
		return request, errors.New("unknown BHTTP mode")
	}
	copy(request.SID[:], header[1:17])
	request.Seq = binary.BigEndian.Uint64(header[17:25])
	request.Value = binary.BigEndian.Uint32(header[25:29])
	// Mode 2 overloads Value as a size hint. Mode 4 is an empty cumulative ACK.
	if request.Mode == ModeDownload || request.Mode == ModeACK {
		return request, nil
	}
	if request.Value > MaxPayload {
		return request, errors.New("BHTTP request is too large")
	}
	if request.Value > 0 {
		request.Payload = make([]byte, int(request.Value))
		if _, err := io.ReadFull(reader, request.Payload); err != nil {
			return request, err
		}
	}
	return request, nil
}

func WriteStatus(writer io.Writer, status byte, data []byte) error {
	packet := make([]byte, 5+len(data))
	packet[0] = status
	binary.BigEndian.PutUint32(packet[1:5], uint32(len(data)))
	copy(packet[5:], data)
	return writeAll(writer, packet)
}

func WriteDownload(writer io.Writer, sid SessionID, mode byte, seq uint64, data []byte) error {
	encrypted := Crypt(data, sid, mode, seq, true)
	packet := make([]byte, 9+len(encrypted))
	packet[0] = StatusData
	binary.BigEndian.PutUint32(packet[1:5], uint32(4+len(encrypted)))
	binary.BigEndian.PutUint32(packet[5:9], uint32(len(data)))
	copy(packet[9:], encrypted)
	return writeAll(writer, packet)
}

func writeAll(writer io.Writer, data []byte) error {
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
