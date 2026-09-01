//go:build !linux || !cgo

package btun

import "errors"

type PAMAuthenticator struct {
	Service string
}

func (auth PAMAuthenticator) Name() string { return "pam-unavailable" }

func (auth PAMAuthenticator) Authenticate(_, _ string) error {
	return errors.New("PAM authentication requires Linux and CGO")
}
