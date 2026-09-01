package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"time"

	"bilola/internal/bhttp"
)

func main() {
	host := flag.String("host", "127.0.0.1", "BHTTP server host")
	port := flag.Int("port", 80, "BHTTP server port")
	timeout := flag.Duration("timeout", 5*time.Second, "probe timeout")
	flag.Parse()
	address := net.JoinHostPort(*host, fmt.Sprint(*port))
	connection, err := net.DialTimeout("tcp", address, *timeout)
	if err != nil {
		panic(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(*timeout))
	var sid bhttp.SessionID
	if _, err := rand.Read(sid[:]); err != nil {
		panic(err)
	}
	clear := make([]byte, 10)
	copy(clear[:4], "BHP2")
	clear[4] = 2
	payload := bhttp.Crypt(clear, sid, bhttp.ModeProbe, 0, false)
	packet := make([]byte, bhttp.HeaderSize+len(payload))
	packet[0] = bhttp.ModeProbe
	copy(packet[1:17], sid[:])
	binary.BigEndian.PutUint32(packet[25:29], uint32(len(payload)))
	copy(packet[bhttp.HeaderSize:], payload)
	if _, err := connection.Write(packet); err != nil {
		panic(err)
	}
	var header [5]byte
	if _, err := io.ReadFull(connection, header[:]); err != nil {
		panic(err)
	}
	length := binary.BigEndian.Uint32(header[1:])
	body := make([]byte, length)
	if _, err := io.ReadFull(connection, body); err != nil {
		panic(err)
	}
	if header[0] != bhttp.StatusOK {
		panic(fmt.Errorf("BHTTP v2 capability rejected: %q", body))
	}
	decoded := bhttp.Crypt(body, sid, bhttp.ModeProbe, 0, true)
	if len(decoded) != 14 || string(decoded[:4]) != "BHP2" || decoded[4] != 2 {
		panic(fmt.Errorf("invalid BHTTP v2 capability: %x", decoded))
	}
	features := binary.BigEndian.Uint32(decoded[6:10])
	maximum := binary.BigEndian.Uint32(decoded[10:14])
	fmt.Printf("BHTTP v2 ready host=%s features=0x%x max_lanes=%d\n", address, features, maximum)
}
