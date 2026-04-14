// Package dnsdiscovery provides DNS-based implementations of the
// discovery.Discovery interface.
//
// Two modes are available:
//
//   - A-record mode (NewARecord): resolves a hostname to a list of IP addresses
//     and combines each with a fixed port. Common for Kubernetes headless services
//     where each pod gets its own A record.
//
//   - SRV mode (NewSRV): resolves a DNS SRV record set. Each SRV record carries
//     its own target hostname and port, so no fixed-port assumption is needed.
//     Common for Kubernetes services with named ports or traditional service
//     discovery systems.
//
// Both implementations use only the standard library; no external dependencies
// are required.
package dnsdiscovery

import (
	"context"
	"fmt"
	"net"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/discovery"
)

// Resolver is the DNS lookup interface used by this package.
// *net.Resolver satisfies this interface and is the default implementation.
// Tests may supply a mock.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

// NodeIDMapperA maps an IP address string to a NodeID. Return false to skip
// that address (e.g. to exclude the local node).
type NodeIDMapperA func(ip string) (raft.NodeID, bool)

// NodeIDMapperSRV maps an SRV target hostname and port to a NodeID. Return
// false to skip that record.
type NodeIDMapperSRV func(target string, port uint16) (raft.NodeID, bool)

// aRecord implements Discovery using DNS A/AAAA record lookups.
type aRecord struct {
	resolver Resolver
	hostname string
	port     string
	mapper   NodeIDMapperA
}

// NewARecord returns a Discovery that resolves hostname to IP addresses and
// pairs each with port (e.g. "7001") to form "host:port" peer addresses.
// mapper converts each IP to a NodeID; return false to skip an address.
// resolver may be nil to use net.DefaultResolver.
func NewARecord(hostname, port string, mapper NodeIDMapperA, resolver Resolver) discovery.Discovery {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &aRecord{
		resolver: resolver,
		hostname: hostname,
		port:     port,
		mapper:   mapper,
	}
}

func (a *aRecord) Discover(ctx context.Context) ([]discovery.PeerInfo, error) {
	addrs, err := a.resolver.LookupHost(ctx, a.hostname)
	if err != nil {
		return nil, fmt.Errorf("dnsdiscovery: LookupHost %q: %w", a.hostname, err)
	}
	var peers []discovery.PeerInfo
	for _, ip := range addrs {
		id, ok := a.mapper(ip)
		if !ok {
			continue
		}
		peers = append(peers, discovery.PeerInfo{
			ID:   id,
			Addr: net.JoinHostPort(ip, a.port),
		})
	}
	return peers, nil
}

// srvRecord implements Discovery using DNS SRV record lookups.
type srvRecord struct {
	resolver Resolver
	service  string
	proto    string
	domain   string
	mapper   NodeIDMapperSRV
}

// NewSRV returns a Discovery that looks up SRV records for
// _service._proto.domain (e.g. service="raft", proto="tcp",
// domain="default.svc.cluster.local"). Each SRV record's target and port are
// passed to mapper to produce a NodeID; return false to skip a record.
// resolver may be nil to use net.DefaultResolver.
func NewSRV(service, proto, domain string, mapper NodeIDMapperSRV, resolver Resolver) discovery.Discovery {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &srvRecord{
		resolver: resolver,
		service:  service,
		proto:    proto,
		domain:   domain,
		mapper:   mapper,
	}
}

func (s *srvRecord) Discover(ctx context.Context) ([]discovery.PeerInfo, error) {
	_, records, err := s.resolver.LookupSRV(ctx, s.service, s.proto, s.domain)
	if err != nil {
		return nil, fmt.Errorf("dnsdiscovery: LookupSRV _%s._%s.%s: %w",
			s.service, s.proto, s.domain, err)
	}
	var peers []discovery.PeerInfo
	for _, rec := range records {
		id, ok := s.mapper(rec.Target, rec.Port)
		if !ok {
			continue
		}
		peers = append(peers, discovery.PeerInfo{
			ID:   id,
			Addr: net.JoinHostPort(rec.Target, fmt.Sprintf("%d", rec.Port)),
		})
	}
	return peers, nil
}
