package xhttpbridge

import (
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"bilola/internal/xhttp"
)

func TestEngineCarriesSSHBytesOverXHTTP2(t *testing.T) {
	serverTarget, testTarget := net.Pipe()
	targetOpened := make(chan struct{})
	var receivedHost atomic.Bool
	protocolServer := xhttp.NewServer(xhttp.Config{
		Logger: log.New(io.Discard, "", 0),
		DialTarget: func(string) (net.Conn, error) {
			close(targetOpened)
			return serverTarget, nil
		},
	})
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Host == "ssh.dt5.test" {
			receivedHost.Store(true)
		}
		protocolServer.ServeHTTP(writer, request)
	}))
	httpServer.EnableHTTP2 = true
	httpServer.StartTLS()
	t.Cleanup(func() {
		_ = testTarget.Close()
		httpServer.Close()
		protocolServer.Close()
	})

	parsed, err := url.Parse(httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	badHost := "127.0.0.2"
	if host == badHost {
		badHost = "127.0.0.3"
	}
	engine, err := New(Config{
		Host: badHost + "#" + host, Port: port,
		HostHeader: "ssh.dt5.test", InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	localPort, err := engine.Start()
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.proxyHosts) != 1 || engine.currentProxy() != host {
		t.Fatalf("sequential probe selected proxies=%v", engine.proxyHosts)
	}
	startupLogs := engine.DrainLogs()
	if !strings.Contains(startupLogs, "Proxies testados: 1/2") ||
		!strings.Contains(startupLogs, "Proxies testados: 2/2") {
		t.Fatalf("missing sequential progress logs:\n%s", startupLogs)
	}
	t.Cleanup(func() {
		engine.Close()
		engine.Wait()
	})
	local, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	select {
	case <-targetOpened:
	case <-time.After(2 * time.Second):
		t.Fatal("XHTTP server did not open its SSH target")
	}

	downstream := []byte("SSH-2.0-test\r\n")
	go func() { _, _ = testTarget.Write(downstream) }()
	_ = local.SetReadDeadline(time.Now().Add(2 * time.Second))
	gotDownstream := make([]byte, len(downstream))
	if _, err := io.ReadFull(local, gotDownstream); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(gotDownstream, downstream) {
		t.Fatalf("download=%q", gotDownstream)
	}

	upload := []byte("SSH client bytes")
	go func() { _, _ = local.Write(upload) }()
	_ = testTarget.SetReadDeadline(time.Now().Add(2 * time.Second))
	gotUpload := make([]byte, len(upload))
	if _, err := io.ReadFull(testTarget, gotUpload); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !bytes.Equal(gotUpload, upload) {
		t.Fatalf("upload=%q", gotUpload)
	}
	deadline := time.Now().Add(2 * time.Second)
	for engine.UploadedBytes() != uint64(len(upload)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if engine.DownloadedBytes() != uint64(len(downstream)) || engine.UploadedBytes() != uint64(len(upload)) {
		t.Fatalf("counters: down=%d up=%d", engine.DownloadedBytes(), engine.UploadedBytes())
	}
	if !receivedHost.Load() {
		t.Fatal("XHTTP requests did not carry the configured DT5 server Host")
	}
}

func TestProxyHostsAreSplitDeduplicatedAndRotated(t *testing.T) {
	engine, err := New(Config{
		Host: "  edge-one.example # edge-two.example##edge-one.example ", Port: 443,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	if len(engine.proxyHosts) != 2 || engine.currentProxy() != "edge-one.example" {
		t.Fatalf("proxy hosts=%v current=%q", engine.proxyHosts, engine.currentProxy())
	}
	next, changed := engine.rotateProxy("edge-one.example")
	if !changed || next != "edge-two.example" {
		t.Fatalf("first failover: changed=%v next=%q", changed, next)
	}
	next, changed = engine.rotateProxy("edge-one.example")
	if changed || next != "edge-two.example" {
		t.Fatalf("stale failure changed active proxy: changed=%v next=%q", changed, next)
	}
}
