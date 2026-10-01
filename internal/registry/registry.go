// Package registry is the lobby's list of servers: who said they were up, when, and
// what they were playing when last asked. A server not heard from within the TTL is
// dropped. Safe for concurrent use.
package registry

import (
	"errors"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/soldatreloaded/soldatreloaded-lobby/internal/query"
)

// Server is one listing.
type Server struct {
	Addr      netip.AddrPort
	Info      query.Info
	FirstSeen time.Time
	LastSeen  time.Time
}

// ErrTooMany is a new server from an address already listing as many as it may.
var ErrTooMany = errors.New("registry: too many servers from this address")

type Registry struct {
	ttl      time.Duration
	maxPerIP int

	mu      sync.Mutex
	servers map[netip.AddrPort]*Server
}

// New lists a server for ttl after its last heartbeat, and at most maxPerIP servers
// from one address.
func New(ttl time.Duration, maxPerIP int) *Registry {
	return &Registry{ttl: ttl, maxPerIP: maxPerIP, servers: map[netip.AddrPort]*Server{}}
}

// Seen notes a heartbeat from addr, which answered the query with info.
func (r *Registry) Seen(addr netip.AddrPort, info query.Info, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.servers[addr]; ok && now.Sub(s.LastSeen) < r.ttl {
		s.Info, s.LastSeen = info, now
		return nil
	}
	if r.countLocked(addr.Addr(), now) >= r.maxPerIP {
		return ErrTooMany
	}
	r.servers[addr] = &Server{Addr: addr, Info: info, FirstSeen: now, LastSeen: now}
	return nil
}

// LastSeen is when addr was last heard from, if it is listed.
func (r *Registry) LastSeen(addr netip.AddrPort, now time.Time) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.servers[addr]
	if !ok || now.Sub(s.LastSeen) >= r.ttl {
		return time.Time{}, false
	}
	return s.LastSeen, true
}

// Remove takes addr off the list: a server shutting down says so.
func (r *Registry) Remove(addr netip.AddrPort) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.servers[addr]
	delete(r.servers, addr)
	return ok
}

// List is every live server, the fullest first, then by address.
func (r *Registry) List(now time.Time) []Server {
	r.mu.Lock()
	out := make([]Server, 0, len(r.servers))
	for _, s := range r.servers {
		if now.Sub(s.LastSeen) < r.ttl {
			out = append(out, *s)
		}
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b Server) int {
		if d := int(b.Info.Players) - int(a.Info.Players); d != 0 {
			return d
		}
		return a.Addr.Compare(b.Addr)
	})
	return out
}

// Expire forgets the servers past their TTL; List already leaves them out.
func (r *Registry) Expire(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for addr, s := range r.servers {
		if now.Sub(s.LastSeen) >= r.ttl {
			delete(r.servers, addr)
			n++
		}
	}
	return n
}

func (r *Registry) countLocked(ip netip.Addr, now time.Time) int {
	n := 0
	for addr, s := range r.servers {
		if addr.Addr() == ip && now.Sub(s.LastSeen) < r.ttl {
			n++
		}
	}
	return n
}
