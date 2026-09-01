package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"bilola/internal/btun"
	"bilola/internal/xhttp"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:443,0.0.0.0:8080", "comma-separated SSH_XHTTP TLS/HTTP2 listeners")
	target := flag.String("target", "127.0.0.1:22", "OpenSSH target")
	btunTarget := flag.String("btun-target", "", "BTUN protocol target selected by HTTP Host")
	btunHosts := flag.String("btun-hosts", "", "HTTP Hosts routed to --btun-target, separated by comma or #")
	autoHosts := flag.String("auto-hosts", "", "shared HTTP Hosts auto-detected as BTUN or SSH, separated by comma or #")
	autoDelay := flag.Duration("auto-delay", time.Second, "SSH fallback delay for shared Hosts")
	certificate := flag.String("tls-cert", "", "TLS certificate chain")
	privateKey := flag.String("tls-key", "", "TLS private key")
	sessionTimeout := flag.Duration("session-timeout", 2*time.Minute, "idle session timeout")
	flag.Parse()

	logger := log.New(os.Stdout, "BilolaXHTTP ", log.LstdFlags|log.Lmicroseconds)
	if *certificate == "" || *privateKey == "" {
		logger.Fatal("--tls-cert and --tls-key are required")
	}

	autoMatcher := makeHostMatcher(*autoHosts)
	transport := xhttp.NewServer(xhttp.Config{
		TargetAddress:           *target,
		TargetAddressForRequest: makeTargetSelector(*target, *btunTarget, *btunHosts),
		DeferTargetForRequest:   autoMatcher,
		TargetAddressForPayload: makePayloadTargetSelector(*target, *btunTarget),
		DeferredTargetDelay:     *autoDelay,
		SessionTimeout:          *sessionTimeout,
		Logger:                  logger,
	})
	httpServer := &http.Server{
		Handler:           transport,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       *sessionTimeout,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1"},
		},
	}

	var listeners []net.Listener
	for _, address := range strings.Split(*listen, ",") {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		listener, listenErr := net.Listen("tcp", address)
		if listenErr != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			logger.Fatalf("listen %s: %v", address, listenErr)
		}
		listeners = append(listeners, listener)
	}
	if len(listeners) == 0 {
		logger.Fatal("no listener configured")
	}

	var serveWait sync.WaitGroup
	for _, listener := range listeners {
		logger.Printf("listening with TLS/HTTP2 on %s -> SSH %s", listener.Addr(), *target)
		serveWait.Add(1)
		go func(current net.Listener) {
			defer serveWait.Done()
			if serveErr := httpServer.ServeTLS(current, *certificate, *privateKey); serveErr != nil &&
				!errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
				logger.Printf("listener stopped: %v", serveErr)
			}
		}(listener)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	received := <-stop
	logger.Printf("received %s, shutting down", received)
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = httpServer.Shutdown(shutdownContext)
	cancel()
	transport.Close()
	serveWait.Wait()
	logger.Print("stopped")
}

func makeTargetSelector(defaultTarget, btunTarget, hosts string) func(*http.Request) string {
	btunTarget = strings.TrimSpace(btunTarget)
	if btunTarget == "" {
		return nil
	}
	allowed := parseConfiguredHosts(hosts)
	if len(allowed) == 0 {
		return nil
	}
	return func(request *http.Request) string {
		if request != nil {
			if _, exists := allowed[normalizeHost(request.Host)]; exists {
				return btunTarget
			}
		}
		return defaultTarget
	}
}

func makeHostMatcher(hosts string) func(*http.Request) bool {
	allowed := parseConfiguredHosts(hosts)
	if len(allowed) == 0 {
		return nil
	}
	return func(request *http.Request) bool {
		if request == nil {
			return false
		}
		_, exists := allowed[normalizeHost(request.Host)]
		return exists
	}
}

func parseConfiguredHosts(hosts string) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, value := range strings.FieldsFunc(hosts, func(character rune) bool {
		return character == ',' || character == '#'
	}) {
		host := normalizeHost(value)
		if host != "" {
			allowed[host] = struct{}{}
		}
	}
	return allowed
}

func makePayloadTargetSelector(defaultTarget, btunTarget string) func([]byte) string {
	if strings.TrimSpace(btunTarget) == "" {
		return nil
	}
	return func(payload []byte) string {
		if bytes.Contains(payload, btun.ClientHello) {
			return btunTarget
		}
		return defaultTarget
	}
}

func normalizeHost(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if host, _, err := net.SplitHostPort(value); err == nil {
		return strings.Trim(strings.ToLower(host), "[]")
	}
	return strings.Trim(value, "[]")
}
