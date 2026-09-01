package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bilola/internal/btun"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("BTUN server: %v", err)
	}
}

func run() error {
	tcpAddress := flag.String("tcp-listen", envString("BTUN_TCP_LISTEN", "0.0.0.0:7300"), "TCP listen address; empty disables TCP")
	udpAddress := flag.String("udp-listen", envString("BTUN_UDP_LISTEN", "0.0.0.0:7300"), "UDP listen address; empty disables UDP")
	tunName := flag.String("tun", envString("BTUN_TUN", "btun0"), "TUN interface name")
	subnet := flag.String("subnet", envString("BTUN_SUBNET", "10.77.0.0/16"), "client IPv4 subnet")
	authMode := flag.String("auth", envString("BTUN_AUTH", "pam"), "authentication backend: pam, file or allow")
	pamService := flag.String("pam-service", envString("BTUN_PAM_SERVICE", "login"), "PAM service")
	authFile := flag.String("auth-file", envString("BTUN_AUTH_FILE", "/etc/btun/users"), "user:password authentication file")
	handshakeTimeout := flag.Duration("handshake-timeout", envDuration("BTUN_HANDSHAKE_TIMEOUT", 15*time.Second), "handshake timeout")
	idleTimeout := flag.Duration("idle-timeout", envDuration("BTUN_IDLE_TIMEOUT", 2*time.Minute), "session idle timeout")
	maxPacketSize := flag.Int("max-packet-size", envInt("BTUN_MAX_PACKET_SIZE", btun.DefaultMaxPacket), "maximum frame payload")
	maxCoverBytes := flag.Int("max-cover-bytes", envInt("BTUN_MAX_COVER_BYTES", 64*1024), "maximum TCP cover bytes before client hello")
	statsFile := flag.String("stats-file", envString("BTUN_STATS_FILE", "/var/lib/btun/stats.json"), "JSON statistics path; empty disables it")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("btun-server %s\n", version)
		return nil
	}
	if strings.TrimSpace(*tcpAddress) == "" && strings.TrimSpace(*udpAddress) == "" {
		return errors.New("at least one TCP or UDP listener is required")
	}
	authenticator, err := makeAuthenticator(*authMode, *pamService, *authFile)
	if err != nil {
		return err
	}
	logger := log.New(os.Stdout, "", log.LstdFlags)
	if _, ok := authenticator.(btun.AllowAuthenticator); ok {
		logger.Printf("WARNING: authentication is disabled (BTUN_AUTH=allow)")
	}
	device, err := btun.OpenTUN(*tunName)
	if err != nil {
		return err
	}
	server, err := btun.NewServer(btun.Config{
		Subnet: *subnet, Authenticator: authenticator, Logger: logger,
		HandshakeTimeout: *handshakeTimeout, IdleTimeout: *idleTimeout,
		MaxPacketSize: *maxPacketSize, MaxCoverBytes: *maxCoverBytes,
	}, device)
	if err != nil {
		_ = device.Close()
		return err
	}

	errorChannel := make(chan error, 2)
	listeners := 0
	var tcpListener net.Listener
	if address := strings.TrimSpace(*tcpAddress); address != "" {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			_ = server.Close()
			return fmt.Errorf("listen TCP %s: %w", address, err)
		}
		tcpListener = listener
		listeners++
		go func() { errorChannel <- server.ServeTCP(listener) }()
	}
	if address := strings.TrimSpace(*udpAddress); address != "" {
		connection, err := net.ListenPacket("udp", address)
		if err != nil {
			if tcpListener != nil {
				_ = tcpListener.Close()
			}
			_ = server.Close()
			return fmt.Errorf("listen UDP %s: %w", address, err)
		}
		listeners++
		go func() { errorChannel <- server.ServeUDP(connection) }()
	}

	logger.Printf("BTUN server %s ready: tun=%s subnet=%s auth=%s", version, device.Name(), *subnet, authenticator.Name())
	stopStats := startStatsWriter(server, *statsFile, logger)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	var serveError error
	select {
	case received := <-signals:
		logger.Printf("BTUN stopping on %s", received)
	case err := <-errorChannel:
		if err != nil {
			serveError = err
		}
	}
	signal.Stop(signals)
	stopStats()
	closeError := server.Close()
	server.Wait()
	if err := writeStats(*statsFile, server.Stats()); err != nil {
		logger.Printf("BTUN final stats: %v", err)
	}
	for index := 1; index < listeners; index++ {
		<-errorChannel
	}
	if serveError != nil {
		return serveError
	}
	return closeError
}

func makeAuthenticator(mode, pamService, authFile string) (btun.Authenticator, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "pam", "system":
		return btun.PAMAuthenticator{Service: pamService}, nil
	case "file":
		if strings.TrimSpace(authFile) == "" {
			return nil, errors.New("BTUN_AUTH_FILE is required for file authentication")
		}
		return btun.FileAuthenticator{Path: authFile}, nil
	case "allow", "none", "insecure":
		return btun.AllowAuthenticator{}, nil
	default:
		return nil, fmt.Errorf("unknown authentication backend %q", mode)
	}
}

func startStatsWriter(server *btun.Server, path string, logger *log.Logger) func() {
	stop := make(chan struct{})
	if strings.TrimSpace(path) == "" {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := writeStats(path, server.Stats()); err != nil {
					logger.Printf("BTUN stats: %v", err)
				}
			case <-stop:
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func writeStats(path string, snapshot btun.StatsSnapshot) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".btun-stats-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err = temporary.Write(encoded); err == nil {
		err = temporary.Chmod(0644)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func envString(name, fallback string) string {
	if value, exists := os.LookupEnv(name); exists {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		log.Fatalf("invalid %s: %v", name, err)
	}
	return parsed
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		log.Fatalf("invalid %s: %v", name, err)
	}
	return parsed
}
