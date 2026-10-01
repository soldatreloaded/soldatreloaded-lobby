package registry

import (
	"net/netip"
	"testing"
	"time"

	"github.com/bettersoldat/bettersoldat-lobby/internal/query"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func addr(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func TestSeenAndExpire(t *testing.T) {
	r := New(90*time.Second, 4)
	if err := r.Seen(addr("1.2.3.4:23073"), query.Info{Hostname: "a"}, t0); err != nil {
		t.Fatal(err)
	}
	r.Seen(addr("1.2.3.4:23073"), query.Info{Hostname: "b"}, t0.Add(60*time.Second))
	list := r.List(t0.Add(120 * time.Second))
	if len(list) != 1 || list[0].Info.Hostname != "b" || !list[0].FirstSeen.Equal(t0) {
		t.Fatalf("after a second heartbeat: %+v", list)
	}
	if len(r.List(t0.Add(150*time.Second))) != 0 {
		t.Fatal("a server past its TTL is listed")
	}
	if r.Expire(t0.Add(150*time.Second)) != 1 || r.Expire(t0.Add(150*time.Second)) != 0 {
		t.Fatal("Expire forgets it once")
	}
}

func TestPerIPLimit(t *testing.T) {
	r := New(time.Minute, 2)
	r.Seen(addr("1.2.3.4:1"), query.Info{}, t0)
	r.Seen(addr("1.2.3.4:2"), query.Info{}, t0)
	if err := r.Seen(addr("1.2.3.4:3"), query.Info{}, t0); err != ErrTooMany {
		t.Fatalf("a third server from one address: %v", err)
	}
	if err := r.Seen(addr("1.2.3.4:1"), query.Info{}, t0); err != nil {
		t.Fatalf("a listed server's heartbeat: %v", err)
	}
	if err := r.Seen(addr("5.6.7.8:1"), query.Info{}, t0); err != nil {
		t.Fatalf("another address: %v", err)
	}
	if err := r.Seen(addr("1.2.3.4:3"), query.Info{}, t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("once the others expire: %v", err)
	}
}

func TestOrderAndRemove(t *testing.T) {
	r := New(time.Minute, 8)
	r.Seen(addr("9.9.9.9:1"), query.Info{Players: 1}, t0)
	r.Seen(addr("1.1.1.1:1"), query.Info{Players: 5}, t0)
	r.Seen(addr("2.2.2.2:1"), query.Info{Players: 1}, t0)
	list := r.List(t0)
	if list[0].Addr != addr("1.1.1.1:1") || list[1].Addr != addr("2.2.2.2:1") || list[2].Addr != addr("9.9.9.9:1") {
		t.Fatalf("order: %v %v %v", list[0].Addr, list[1].Addr, list[2].Addr)
	}
	if !r.Remove(addr("1.1.1.1:1")) || r.Remove(addr("1.1.1.1:1")) || len(r.List(t0)) != 2 {
		t.Fatal("Remove takes it off once")
	}
}
