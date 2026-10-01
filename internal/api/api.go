// Package api is the lobby's HTTP face.
//
//	POST   /v1/servers  {"port": 23073}  a game server's heartbeat
//	DELETE /v1/servers  {"port": 23073}  a game server going away
//	GET    /v1/servers                   the list, for a browser
//	GET    /healthz
//
// A heartbeat's address is the connection's, never the body's, so a server can only
// list itself. The lobby asks the server the query (package query) before listing it,
// so a server nobody can reach is never listed, and what it answers is what the list
// says until the next heartbeat. A browser should ask each server the query itself for
// the ping and the players now.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/bettersoldat/bettersoldat-lobby/internal/query"
	"github.com/bettersoldat/bettersoldat-lobby/internal/registry"
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
	// TrustProxy takes the client's address from X-Forwarded-For's last entry: only
	// behind a reverse proxy that sets it.
	TrustProxy bool
	Now        func() time.Time
	Log        *slog.Logger
}

type handler struct{ Config }

func New(c Config) http.Handler {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	h := &handler{c}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/servers", h.heartbeat)
	mux.HandleFunc("DELETE /v1/servers", h.remove)
	mux.HandleFunc("GET /v1/servers", h.list)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

type portBody struct {
	Port uint16 `json:"port"`
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

// The address the request came from, as ENet can reach it: IPv4 only, as ENet 1.3 is.
func (h *handler) source(r *http.Request) (netip.Addr, error) {
	raw := r.RemoteAddr
	if h.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			parts := strings.Split(fwd, ",")
			raw = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	ip, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, err
	}
	ip = ip.Unmap()
	if !ip.Is4() {
		return netip.Addr{}, errors.New("game servers are IPv4 only")
	}
	return ip, nil
}

func (h *handler) target(w http.ResponseWriter, r *http.Request) (netip.AddrPort, bool) {
	ip, err := h.source(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return netip.AddrPort{}, false
	}
	var body portBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || body.Port == 0 {
		http.Error(w, `want {"port": <1-65535>}`, http.StatusBadRequest)
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip, body.Port), true
}

func (h *handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	addr, ok := h.target(w, r)
	if !ok {
		return
	}
	reply := heartbeatReply{Address: addr.Addr().String(), Port: addr.Port(), HeartbeatSeconds: int(h.Heartbeat / time.Second)}
	if last, listed := h.Registry.LastSeen(addr, h.Now()); listed && h.Now().Sub(last) < h.MinInterval {
		writeJSON(w, http.StatusOK, reply) // too soon to ask again; still listed
		return
	}
	info, err := h.Probe(r.Context(), addr)
	if err != nil {
		h.Log.Info("unreachable", "addr", addr, "err", err)
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

func (h *handler) remove(w http.ResponseWriter, r *http.Request) {
	addr, ok := h.target(w, r)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
