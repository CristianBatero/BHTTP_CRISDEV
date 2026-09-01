package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"bilola/internal/bhttp"
	"bilola/internal/xhttp"
)

func main() {
	listenFlag := flag.String("listen", "0.0.0.0:80,0.0.0.0:53", "comma-separated BHTTP TCP listeners")
	xhttpListen := flag.String("xhttp-listen", "", "SSH_XHTTP TLS/HTTP2 listener")
	tlsCertificate := flag.String("tls-cert", "", "TLS certificate for SSH_XHTTP")
	tlsKey := flag.String("tls-key", "", "TLS private key for SSH_XHTTP")
	targetFlag := flag.String("target", "127.0.0.1:22", "OpenSSH target")
	sessionTimeout := flag.Duration("session-timeout", 2*time.Minute, "idle session timeout")
	maxV2Lanes := flag.Int("bhttp-v2-max-lanes", 128, "maximum BHTTP v2 lanes per session")
	flag.Parse()

	logger := log.New(os.Stdout, "BilolaGo ", log.LstdFlags|log.Lmicroseconds)
	bhttpServer := bhttp.NewServer(bhttp.Config{
		TargetAddress:  *targetFlag,
		SessionTimeout: *sessionTimeout,
		MaxV2Lanes:     *maxV2Lanes,
		Logger:         logger,
	})
	var listeners []net.Listener
	var serveWait sync.WaitGroup
	for _, address := range strings.Split(*listenFlag, ",") {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			logger.Fatalf("listen %s: %v", address, err)
		}
		listeners = append(listeners, listener)
		logger.Printf("BHTTP listening on %s -> SSH %s", listener.Addr(), *targetFlag)
		serveWait.Add(1)
		go func() {
			defer serveWait.Done()
			if err := bhttpServer.Serve(listener); err != nil {
				logger.Printf("listener %s stopped: %v", listener.Addr(), err)
			}
		}()
	}

	var xhttpServer *xhttp.Server
	var xhttpHTTP *http.Server
	if strings.TrimSpace(*xhttpListen) != "" {
		if *tlsCertificate == "" || *tlsKey == "" {
			logger.Fatal("--tls-cert and --tls-key are required with --xhttp-listen")
		}
		listener, err := net.Listen("tcp", strings.TrimSpace(*xhttpListen))
		if err != nil {
			logger.Fatalf("XHTTP listen %s: %v", *xhttpListen, err)
		}
		listeners = append(listeners, listener)
		xhttpServer = xhttp.NewServer(xhttp.Config{
			TargetAddress: *targetFlag, SessionTimeout: *sessionTimeout, Logger: logger,
		})
		xhttpHTTP = &http.Server{
			Handler:           xhttpServer,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       *sessionTimeout,
			TLSConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				NextProtos: []string{"h2", "http/1.1"},
			},
		}
		logger.Printf("SSH_XHTTP listening with TLS/HTTP2 on %s -> SSH %s", listener.Addr(), *targetFlag)
		serveWait.Add(1)
		go func() {
			defer serveWait.Done()
			err := xhttpHTTP.ServeTLS(listener, *tlsCertificate, *tlsKey)
			if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				logger.Printf("XHTTP listener stopped: %v", err)
			}
		}()
	}
	if len(listeners) == 0 {
		logger.Fatal("no listener configured")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	received := <-stop
	logger.Printf("received %s, shutting down", received)
	for _, listener := range listeners {
		_ = listener.Close()
	}
	if xhttpHTTP != nil {
		_ = xhttpHTTP.Close()
	}
	bhttpServer.Close()
	if xhttpServer != nil {
		xhttpServer.Close()
	}
	serveWait.Wait()
	bhttpServer.Wait()
	fmt.Println("BilolaGo stopped")
}
