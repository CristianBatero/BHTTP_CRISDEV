//go:build !linux

package btun

import "errors"

func OpenTUN(_ string) (PacketDevice, error) {
	return nil, errors.New("BTUN TUN is only supported on Linux")
}
