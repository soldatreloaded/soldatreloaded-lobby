package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/soldatreloaded/soldatreloaded-lobby/internal/query"
	"github.com/soldatreloaded/soldatreloaded-lobby/internal/registry"
)

type rig struct {
	h      http.Handler
	now    time.Time
	probed []netip.AddrPort
	up     map[uint16]query.Info // the ports answering, and what they say
}

func newRig(clientIPHeader string) *rig {
	g := &rig{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), up: map[uint16]query.Info{}}
	g.h = New(Config{
		Registry: registry.New(90*time.Second, 2),
		Probe: func(_ context.Context, addr netip.AddrPort) (query.Info, error) {
			g.probed = append(g.probed, addr)
			if info, ok := g.up[addr.Port()]; ok {
				return info, nil
			}
			return query.Info{}, errors.New("silence")
		},
		Heartbeat:       30 * time.Second,
		MinInterval:     10 * time.Second,
		ProbesPerMinute: 5,
		ClientIPHeader:  clientIPHeader,
		Now:             func() time.Time { return g.now },
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return g
}

func (g *rig) do(method, path, from, body string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = from
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	return w
}

func (g *rig) list(t *testing.T) []ServerJSON {
	t.Helper()
	w := g.do("GET", "/v1/servers", "9.9.9.9:1", "")
	var out struct{ Servers []ServerJSON }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	return out.Servers
}

func TestHeartbeatListsAReachableServer(t *testing.T) {
	g := newRig("")
	g.up[23073] = query.Info{Hostname: "Ash", Map: "ctf_Ash", Players: 4, MaxPlayers: 32, Protocol: 9}
	w := g.do("POST", "/v1/servers", "203.0.113.5:51234", `{"port":23073}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"heartbeat_seconds":30`) {
		t.Fatalf("heartbeat: %d %s", w.Code, w.Body)
	}
	if len(g.probed) != 1 || g.probed[0] != netip.MustParseAddrPort("203.0.113.5:23073") {
		t.Fatalf("probed %v: the connection's address, the body's port", g.probed)
	}
	list := g.list(t)
	if len(list) != 1 || list[0].Address != "203.0.113.5" || list[0].Name != "Ash" || list[0].Players != 4 {
		t.Fatalf("list: %+v", list)
	}

	g.now = g.now.Add(5 * time.Second)
	g.do("POST", "/v1/servers", "203.0.113.5:51234", `{"port":23073}`)
	if len(g.probed) != 1 {
		t.Fatal("a heartbeat too soon after the last asks the server again")
	}
	g.now = g.now.Add(100 * time.Second)
	if len(g.list(t)) != 0 {
		t.Fatal("a server gone quiet is still listed")
	}
}

func TestUnreachableIsNotListed(t *testing.T) {
	g := newRig("")
	if w := g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":23073}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unreachable server: %d", w.Code)
	}
	if len(g.list(t)) != 0 {
		t.Fatal("and is listed")
	}
}

func TestBadRequests(t *testing.T) {
	g := newRig("")
	for _, c := range []struct{ from, body string }{
		{"203.0.113.5:1", `{"port":0}`},
		{"203.0.113.5:1", `not json`},
		{"[2001:db8::1]:1", `{"port":23073}`},
	} {
		if w := g.do("POST", "/v1/servers", c.from, c.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s %s: %d", c.from, c.body, w.Code)
		}
	}
	if len(g.probed) != 0 {
		t.Fatal("a bad request was probed")
	}
}

func TestPerIPLimitAndRemove(t *testing.T) {
	g := newRig("")
	for p := uint16(1); p <= 3; p++ {
		g.up[p] = query.Info{}
	}
	g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":1}`)
	g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":2}`)
	if w := g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":3}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a third server from one address: %d", w.Code)
	}
	if w := g.do("DELETE", "/v1/servers", "198.51.100.1:1", `{"port":1}`); w.Code != http.StatusNoContent || len(g.list(t)) != 2 {
		t.Fatal("another address took a server off the list")
	}
	if g.do("DELETE", "/v1/servers", "203.0.113.5:1", `{"port":1}`); len(g.list(t)) != 1 {
		t.Fatal("a server could not take itself off")
	}
}

func TestClientIPHeader(t *testing.T) {
	g := newRig("X-Forwarded-For")
	g.up[23073] = query.Info{}
	g.do("POST", "/v1/servers", "127.0.0.1:1", `{"port":23073}`, "X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	if len(g.probed) != 1 || g.probed[0].Addr().String() != "203.0.113.9" {
		t.Fatalf("probed %v: the proxy's last entry", g.probed)
	}

	g = newRig("Fly-Client-IP")
	g.up[23073] = query.Info{}
	g.do("POST", "/v1/servers", "172.16.0.2:1", `{"port":23073}`, "Fly-Client-IP", "203.0.113.7")
	if len(g.probed) != 1 || g.probed[0].Addr().String() != "203.0.113.7" {
		t.Fatalf("probed %v: Fly-Client-IP whole", g.probed)
	}
	if w := g.do("POST", "/v1/servers", "172.16.0.2:1", `{"port":23073}`); w.Code != http.StatusBadRequest {
		t.Fatalf("no header from the proxy: %d, want 400 rather than the proxy's own address", w.Code)
	}

	g = newRig("")
	g.up[23073] = query.Info{}
	g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":23073}`, "X-Forwarded-For", "6.6.6.6")
	if g.probed[0].Addr().String() != "203.0.113.5" {
		t.Fatal("X-Forwarded-For was believed without a proxy")
	}
}

func TestTextList(t *testing.T) {
	g := newRig("")
	g.up[1] = query.Info{Players: 2}
	g.up[2] = query.Info{Players: 7}
	g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":1}`)
	g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":2}`)
	w := g.do("GET", "/v1/servers.txt", "9.9.9.9:1", "")
	if w.Code != 200 || w.Body.String() != "203.0.113.5:2\n203.0.113.5:1\n" {
		t.Fatalf("text list: %d %q", w.Code, w.Body)
	}
}

func TestNamedAddress(t *testing.T) {
	g := newRig("")
	g.up[23073] = query.Info{Hostname: "behind a proxy"}
	// from an IPv6 egress, naming the IPv4 address players reach it on
	w := g.do("POST", "/v1/servers", "[2001:db8::1]:1", `{"port":23073,"address":"213.188.216.246"}`)
	if w.Code != 200 || len(g.probed) != 1 || g.probed[0] != netip.MustParseAddrPort("213.188.216.246:23073") {
		t.Fatalf("named address: %d, probed %v", w.Code, g.probed)
	}
	if list := g.list(t); len(list) != 1 || list[0].Address != "213.188.216.246" {
		t.Fatalf("list: %+v", list)
	}
	for _, bad := range []string{"10.0.0.1", "127.0.0.1", "192.168.1.1", "2001:db8::2", "nonsense", "0.0.0.0"} {
		if w := g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":23073,"address":"`+bad+`"}`); w.Code != http.StatusBadRequest {
			t.Errorf("address %s: %d", bad, w.Code)
		}
	}
	if len(g.probed) != 1 {
		t.Fatalf("a refused address was probed: %v", g.probed)
	}
	// nobody takes a named server off the list but time
	g.do("DELETE", "/v1/servers", "203.0.113.5:1", `{"port":23073,"address":"213.188.216.246"}`)
	if len(g.list(t)) != 1 {
		t.Fatal("a DELETE naming an address took it off")
	}
}

func TestProbeLimit(t *testing.T) {
	g := newRig("")
	for p := uint16(1); p <= 6; p++ {
		g.do("POST", "/v1/servers", "203.0.113.5:1", fmt.Sprintf(`{"port":%d}`, p)) // nothing answers: every one probes
	}
	if len(g.probed) != 5 {
		t.Fatalf("probed %d times on a limit of 5 a minute", len(g.probed))
	}
	if w := g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":7}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("past the limit: %d", w.Code)
	}
	g.do("POST", "/v1/servers", "198.51.100.1:1", `{"port":7}`)
	g.now = g.now.Add(time.Minute)
	g.do("POST", "/v1/servers", "203.0.113.5:1", `{"port":7}`)
	if len(g.probed) != 7 {
		t.Fatalf("another requester, then the next minute: probed %d, want 7", len(g.probed))
	}
}
