package btun

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"strings"
)

type Authenticator interface {
	Authenticate(username, password string) error
	Name() string
}

type AllowAuthenticator struct{}

func (AllowAuthenticator) Authenticate(_, _ string) error { return nil }
func (AllowAuthenticator) Name() string                   { return "allow-insecure" }

type FileAuthenticator struct {
	Path string
}

func (auth FileAuthenticator) Name() string { return "file" }

func (auth FileAuthenticator) Authenticate(username, password string) error {
	if strings.TrimSpace(auth.Path) == "" {
		return errors.New("authentication file is not configured")
	}
	contents, err := os.ReadFile(auth.Path)
	if err != nil {
		return fmt.Errorf("read authentication file: %w", err)
	}
	if len(contents) > 4<<20 {
		return errors.New("authentication file is too large")
	}
	matched := 0
	for _, rawLine := range strings.Split(string(contents), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		userEqual := subtle.ConstantTimeCompare([]byte(parts[0]), []byte(username))
		passEqual := subtle.ConstantTimeCompare([]byte(parts[1]), []byte(password))
		matched |= userEqual & passEqual
	}
	if matched != 1 {
		return errors.New("invalid username or password")
	}
	return nil
}
