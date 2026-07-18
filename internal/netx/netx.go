// Package netx holds small networking helpers.
package netx

import (
	"fmt"
	"math/rand"
	"net"
	"strconv"
)

// RandomFreePort returns a currently-bindable TCP port in [lo,hi] on host,
// probing the range in random order so repeated calls spread out.
func RandomFreePort(host string, lo, hi int) (int, error) {
	if host == "" {
		host = "0.0.0.0"
	}
	if lo < 1 || hi > 65535 || lo > hi {
		return 0, fmt.Errorf("invalid port range %d-%d", lo, hi)
	}

	ports := make([]int, 0, hi-lo+1)
	for p := lo; p <= hi; p++ {
		ports = append(ports, p)
	}
	rand.Shuffle(len(ports), func(i, j int) { ports[i], ports[j] = ports[j], ports[i] })

	for _, p := range ports {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free TCP port in %d-%d on %s", lo, hi, host)
}
