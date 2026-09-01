package btun

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
)

type AddressPool struct {
	mu      sync.Mutex
	network uint32
	first   uint32
	last    uint32
	next    uint32
	used    map[uint32]struct{}
}

func NewAddressPool(cidr string) (*AddressPool, error) {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse subnet: %w", err)
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil, errors.New("BTUN subnet must be IPv4")
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return nil, errors.New("BTUN subnet must contain at least four IPv4 addresses")
	}
	base := binary.BigEndian.Uint32(v4) & binary.BigEndian.Uint32(network.Mask)
	size := uint64(1) << uint(32-ones)
	last := uint32(uint64(base) + size - 2)
	first := base + 2
	if first > last {
		return nil, errors.New("BTUN subnet has no client addresses")
	}
	return &AddressPool{network: base, first: first, last: last, next: first, used: make(map[uint32]struct{})}, nil
}

func (pool *AddressPool) Acquire() (net.IP, error) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	capacity := uint64(pool.last) - uint64(pool.first) + 1
	for checked := uint64(0); checked < capacity; checked++ {
		candidate := pool.next
		pool.next++
		if pool.next > pool.last {
			pool.next = pool.first
		}
		if _, exists := pool.used[candidate]; exists {
			continue
		}
		pool.used[candidate] = struct{}{}
		var raw [4]byte
		binary.BigEndian.PutUint32(raw[:], candidate)
		return net.IPv4(raw[0], raw[1], raw[2], raw[3]), nil
	}
	return nil, errors.New("BTUN address pool exhausted")
}

func (pool *AddressPool) Release(ip net.IP) {
	v4 := ip.To4()
	if v4 == nil {
		return
	}
	value := binary.BigEndian.Uint32(v4)
	pool.mu.Lock()
	delete(pool.used, value)
	pool.mu.Unlock()
}
