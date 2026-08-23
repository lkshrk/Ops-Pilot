package oci

import (
	"context"
	"net"
	"net/netip"
	"strconv"

	"github.com/lkshrk/ops-pilot/internal/netpolicy"
)

func safeDialer(fixture netip.AddrPort) func(context.Context, string, string) (net.Conn, error) {
	dialer := net.Dialer{}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, category(ErrTrustBoundary, "invalid destination")
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			ip = ip.Unmap()
			if !allowed(ip, port, fixture) {
				return nil, category(ErrTrustBoundary, "unsafe registry destination")
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err != nil {
				return nil, unavailable(err, "dial registry")
			}
			if !samePeer(conn.RemoteAddr(), ip) {
				conn.Close()
				return nil, category(ErrTrustBoundary, "registry peer mismatch")
			}
			return conn, nil
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, unavailable(err, "resolve registry")
		}
		if len(ips) == 0 {
			return nil, category(ErrUnavailable, "registry host has no addresses")
		}
		if !allPublic(ips) {
			return nil, category(ErrTrustBoundary, "unsafe registry destination")
		}
		ip := ips[0].Unmap()
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err != nil {
			return nil, unavailable(err, "dial registry")
		}
		if !samePeer(conn.RemoteAddr(), ip) {
			conn.Close()
			return nil, category(ErrTrustBoundary, "registry peer mismatch")
		}
		return conn, nil
	}
}

func samePeer(addr net.Addr, want netip.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	got, err := netip.ParseAddr(host)
	return err == nil && got.Unmap() == want
}
func allowed(ip netip.Addr, port string, fixture netip.AddrPort) bool {
	if fixture.IsValid() {
		p, err := strconv.ParseUint(port, 10, 16)
		if err == nil && ip == fixture.Addr().Unmap() && uint16(p) == fixture.Port() {
			return true
		}
	}
	return netpolicy.Public(ip)
}

func allPublic(addrs []netip.Addr) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		if !netpolicy.Public(addr) {
			return false
		}
	}
	return true
}
