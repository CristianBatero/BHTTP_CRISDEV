package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bilola/internal/btun"
	"bilola/internal/xhttp"
	"bilola/mobile/xhttpbridge"
)

func main() {
	host := flag.String("host", "137.131.163.233", "XHTTP server address")
	port := flag.Int("port", 443, "XHTTP TLS port")
	serverName := flag.String("sni", "flux.dtmod.shop", "TLS SNI")
	hostHeader := flag.String("host-header", "", "HTTP Host (defaults to SNI)")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	targetMode := flag.String("target-mode", "ssh", "expected target: ssh or btun")
	originHTTP1 := flag.Bool("origin-http1", false, "test the origin using HTTP/1.1 framing")
	timeout := flag.Duration("timeout", 10*time.Second, "smoke-test timeout")
	flag.Parse()
	if strings.TrimSpace(*hostHeader) == "" {
		*hostHeader = *serverName
	}
	if *originHTTP1 {
		if !strings.EqualFold(*targetMode, "btun") {
			log.Fatal("-origin-http1 currently supports -target-mode btun")
		}
		if err := smokeBTUNOriginHTTP1(*host, *port, *serverName, *hostHeader, *insecure, *timeout); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("BTUN XHTTP OK: HTTP/1.1 origin %s:%d SNI=%s Host=%s, target=127.0.0.1:7300\n",
			*host, *port, *serverName, *hostHeader)
		return
	}

	engine, err := xhttpbridge.New(xhttpbridge.Config{
		Host: *host, Port: *port, ServerName: *serverName, HostHeader: *hostHeader,
		InsecureSkipVerify: *insecure, ConnectTimeout: *timeout,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		engine.Close()
		engine.Wait()
	}()
	localPort, err := engine.Start()
	if err != nil {
		log.Fatalf("%v; logs=%s", err, engine.DrainLogs())
	}
	connection, err := net.DialTimeout("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)), *timeout)
	if err != nil {
		log.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(*timeout))
	if strings.EqualFold(*targetMode, "btun") {
		if _, err := connection.Write(btun.ClientHello); err != nil {
			log.Fatalf("BTUN client hello over XHTTP: %v", err)
		}
		serverHello := make([]byte, len(btun.ServerHello))
		if _, err := io.ReadFull(connection, serverHello); err != nil {
			log.Fatalf("BTUN server hello over XHTTP: %v; engine=%s", err, engine.LastError())
		}
		if string(serverHello) != string(btun.ServerHello) {
			log.Fatalf("unexpected BTUN hello %q", serverHello)
		}
		fmt.Printf("BTUN XHTTP OK: TLS/H2 %s:%d SNI=%s Host=%s, target=127.0.0.1:7300\n",
			*host, *port, *serverName, *hostHeader)
		return
	}
	banner, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		log.Fatalf("SSH banner over XHTTP: %v; engine=%s", err, engine.LastError())
	}
	if !strings.HasPrefix(banner, "SSH-") {
		log.Fatalf("unexpected target banner %q", banner)
	}
	fmt.Printf("SSH_XHTTP OK: TLS/H2 %s:%d SNI=%s Host=%s, target=%s",
		*host, *port, *serverName, *hostHeader, banner)
}

func smokeBTUNOriginHTTP1(host string, port int, serverName, hostHeader string, insecure bool, timeout time.Duration) error {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ServerName:         serverName,
			InsecureSkipVerify: insecure, // smoke-test option only
		},
		ForceAttemptHTTP2: false,
		TLSNextProto:      make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout}
	endpoint := "https://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
	const sid = "btun-smoke-test-0001"

	upload, err := newOriginRequest(endpoint, hostHeader, sid, "upload", bytes.NewReader(btun.ClientHello))
	if err != nil {
		return err
	}
	upload.Header.Set(xhttp.HeaderSequence, "0")
	uploadResponse, err := client.Do(upload)
	if err != nil {
		return fmt.Errorf("XHTTP upload: %w", err)
	}
	_, _ = io.Copy(io.Discard, uploadResponse.Body)
	_ = uploadResponse.Body.Close()
	if uploadResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("XHTTP upload returned %s", uploadResponse.Status)
	}

	download, err := newOriginRequest(endpoint, hostHeader, sid, "download", http.NoBody)
	if err != nil {
		return err
	}
	download.Header.Set(xhttp.HeaderDownloadACK, "0")
	downloadResponse, err := client.Do(download)
	if err != nil {
		return fmt.Errorf("XHTTP download: %w", err)
	}
	defer downloadResponse.Body.Close()
	if downloadResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("XHTTP download returned %s", downloadResponse.Status)
	}
	prefix := make([]byte, len(":\r\n\r\n"))
	if _, err := io.ReadFull(downloadResponse.Body, prefix); err != nil || string(prefix) != ":\r\n\r\n" {
		return fmt.Errorf("invalid XHTTP download prefix %q: %v", prefix, err)
	}
	var frameHeader [12]byte
	if _, err := io.ReadFull(downloadResponse.Body, frameHeader[:]); err != nil {
		return fmt.Errorf("read XHTTP frame header: %w", err)
	}
	if sequence := binary.BigEndian.Uint64(frameHeader[:8]); sequence != 0 {
		return fmt.Errorf("unexpected XHTTP sequence %d", sequence)
	}
	length := binary.BigEndian.Uint32(frameHeader[8:])
	payload := make([]byte, length)
	if _, err := io.ReadFull(downloadResponse.Body, payload); err != nil {
		return fmt.Errorf("read XHTTP frame payload: %w", err)
	}
	if !bytes.Equal(payload, btun.ServerHello) {
		return fmt.Errorf("unexpected BTUN hello %q", payload)
	}
	return nil
}

func newOriginRequest(endpoint, hostHeader, sid, mode string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	request.Host = hostHeader
	request.Header.Set(xhttp.HeaderSID, sid)
	request.Header.Set(xhttp.HeaderMode, mode)
	request.Header.Set(xhttp.HeaderVersion, xhttp.ProtocolVersion)
	request.Header.Set("Cache-Control", "no-store, no-transform")
	return request, nil
}
