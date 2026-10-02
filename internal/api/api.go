// Package api is the lobby's HTTP face.
//
//	POST   /v1/servers      {"port": 23073}  a game server's heartbeat
//	                        {"port": 23073, "address": "1.2.3.4"}  one behind a proxy
//	DELETE /v1/servers      {"port": 23073}  a game server going away
//	GET    /v1/servers      the list, for a browser, as JSON
//	GET    /v1/servers.txt  the list as addresses, "1.2.3.4:23073" a line, for the game
//	GET    /healthz
//
// A heartbeat's address is the connection's unless the body names one. A server behind
// a proxy that sends from another address than players reach it on (Fly's
// fly-global-services) names the one players reach. Either way the lobby asks that
// address the query (package query) before listing it, so only a game server that
// answers there is ever listed: naming another's address can at most list a real server
// that was not asking to be. What the server answers is what the list says until the
// next heartbeat; a browser asks each server the query itself for the ping and the
// players now.
//
// A probe is a few datagrams at an address someone named, so each requester may cause
// only so many a minute.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/soldatreloaded/soldatreloaded-lobby/internal/query"
	"github.com/soldatreloaded/soldatreloaded-lobby/internal/registry"
)

// Prober asks a game server the query.
type Prober func(ctx context.Context, addr netip.AddrPort) (query.Info, error)

type Config struct {
	Registry *registry.Registry
	Probe    Prober
	// Heartbeat is how often a server is told to say it is up; the registry's TTL
	// should be a few of these.
	Heartbeat time.Duration
	// MinInterval: a heartbeat sooner than this after the last is taken without
	// asking the server again.
	MinInterval time.Duration
	// ProbesPerMinute is how many probes one requester's heartbeats may cause in a
	// minute; 0 for no limit.
	ProbesPerMinute int
	// ClientIPHeader takes the client's address from this header rather than the
	// connection: only behind a proxy that sets it and strips a client's own. For
	// X-Forwarded-For the last entry is taken, the one the nearest proxy appended;
	// any other header (Fly-Client-IP, X-Real-IP) is taken whole. Empty for none.
	ClientIPHeader string
	Now            func() time.Time
	Log            *slog.Logger
}

type handler struct {
	Config
	probes limiter
}

func New(c Config) http.Handler {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	h := &handler{Config: c, probes: limiter{max: c.ProbesPerMinute, counts: map[netip.Addr]int{}}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/servers", h.heartbeat)
	mux.HandleFunc("DELETE /v1/servers", h.remove)
	mux.HandleFunc("GET /v1/servers", h.list)
	mux.HandleFunc("GET /v1/servers.txt", h.listText)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

type heartbeatBody struct {
	Port    uint16 `json:"port"`
	Address string `json:"address"` // the address players reach the server on; empty for the connection's
}

type heartbeatReply struct {
	Address          string `json:"address"`
	Port             uint16 `json:"port"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
}

// ServerJSON is one listing as the list gives it.
type ServerJSON struct {
	Address    string    `json:"address"`
	Port       uint16    `json:"port"`
	Name       string    `json:"name"`
	Map        string    `json:"map"`
	Mode       uint8     `json:"mode"` // the game's MatchMode: 0 deathmatch, 1 capture the flag
	Players    uint8     `json:"players"`
	Bots       uint8     `json:"bots"`
	MaxPlayers uint8     `json:"max_players"`
	Password   bool      `json:"password"`
	Protocol   uint16    `json:"protocol"`
	LastSeen   time.Time `json:"last_seen"`
}

// The address the request came from.
func (h *handler) source(r *http.Request) (netip.Addr, error) {
	raw := r.RemoteAddr
	if h.ClientIPHeader != "" {
		v := r.Header.Get(h.ClientIPHeader)
		if v == "" {
			return netip.Addr{}, errors.New("no " + h.ClientIPHeader + " from the proxy")
		}
		if http.CanonicalHeaderKey(h.ClientIPHeader) == "X-Forwarded-For" {
			parts := strings.Split(v, ",")
			v = parts[len(parts)-1]
		}
		raw = strings.TrimSpace(v)
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	ip, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, err
	}
	return ip.Unmap(), nil
}

// The game server a request is about, and who asked. A named address must be a public
// IPv4 one, so the lobby is never pointed into a private network; otherwise the
// connection's must be IPv4, as ENet 1.3 is.
func (h *handler) target(w http.ResponseWriter, r *http.Request, named bool) (addr netip.AddrPort, from netip.Addr, ok bool) {
	from, err := h.source(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return addr, from, false
	}
	var body heartbeatBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || body.Port == 0 {
		http.Error(w, `want {"port": <1-65535>}, and "address" if it is not the connection's`, http.StatusBadRequest)
		return addr, from, false
	}
	ip := from
	if named && body.Address != "" {
		ip, err = netip.ParseAddr(body.Address)
		if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			http.Error(w, "address: want a public IPv4 address", http.StatusBadRequest)
			return addr, from, false
		}
	} else if !ip.Is4() {
		http.Error(w, "game servers are IPv4 only: heartbeat over IPv4, or name the address", http.StatusBadRequest)
		return addr, from, false
	}
	return netip.AddrPortFrom(ip, body.Port), from, true
}

func (h *handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	addr, from, ok := h.target(w, r, true)
	if !ok {
		return
	}
	reply := heartbeatReply{Address: addr.Addr().String(), Port: addr.Port(), HeartbeatSeconds: int(h.Heartbeat / time.Second)}
	if last, listed := h.Registry.LastSeen(addr, h.Now()); listed && h.Now().Sub(last) < h.MinInterval {
		writeJSON(w, http.StatusOK, reply) // too soon to ask again; still listed
		return
	}
	if !h.probes.allow(from, h.Now()) {
		http.Error(w, "too many heartbeats from this address", http.StatusTooManyRequests)
		return
	}
	info, err := h.Probe(r.Context(), addr)
	if err != nil {
		h.Log.Info("unreachable", "addr", addr, "from", from, "err", err)
		http.Error(w, "the lobby could not reach "+addr.String()+" over UDP: is the port forwarded?", http.StatusUnprocessableEntity)
		return
	}
	switch err := h.Registry.Seen(addr, info, h.Now()); {
	case errors.Is(err, registry.ErrTooMany):
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// A server takes itself off the list from the address it is listed at; one listed at a
// named address is let expire, as nobody else may take it off.
func (h *handler) remove(w http.ResponseWriter, r *http.Request) {
	addr, _, ok := h.target(w, r, false)
	if !ok {
		return
	}
	h.Registry.Remove(addr)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	servers := h.Registry.List(h.Now())
	out := make([]ServerJSON, 0, len(servers))
	for _, s := range servers {
		out = append(out, ServerJSON{
			Address: s.Addr.Addr().String(), Port: s.Addr.Port(),
			Name: s.Info.Hostname, Map: s.Info.Map, Mode: uint8(s.Info.Mode),
			Players: s.Info.Players, Bots: s.Info.Bots, MaxPlayers: s.Info.MaxPlayers,
			Password: s.Info.Password, Protocol: s.Info.Protocol, LastSeen: s.LastSeen.UTC(),
		})
	}
	w.Header().Set("Cache-Control", "public, max-age=5")
	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

// The list as the game reads it: an address and port a line, nothing else, since the
// game asks every server the query itself.
func (h *handler) listText(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=5")
	for _, s := range h.Registry.List(h.Now()) {
		fmt.Fprintf(w, "%s\n", s.Addr)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// A count per address over a minute, all forgotten as the next minute begins.
type limiter struct {
	max int

	mu     sync.Mutex
	window time.Time
	counts map[netip.Addr]int
}

func (l *limiter) allow(ip netip.Addr, now time.Time) bool {
	if l.max <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.window) >= time.Minute {
		l.window = now
		clear(l.counts)
	}
	if l.counts[ip] >= l.max {
		return false
	}
	l.counts[ip]++
	return true
}
