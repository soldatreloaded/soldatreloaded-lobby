package query

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// The bytes the game's query_write_reply lays out for this info, written by hand from
// shared/network/query.h, so a change on either side that breaks the other fails here.
var golden = []byte{
	0xFF, 0xFF, 0xFF, 0xFF, 'B', 'S', 'R', 'i',
	0x4D, 0x00, 0x00, 0x00, // nonce 77
	0x09, 0x00, // protocol 9
	3, 2, 32, 1, 1, // players, bots, max, CTF, password
	14, 'Y', 'e', ' ', 'O', 'l', 'd', 'e', ' ', 'S', 'e', 'r', 'v', 'e', 'r',
	7, 'c', 't', 'f', '_', 'A', 's', 'h',
}

var goldenInfo = Info{Protocol: 9, Players: 3, Bots: 2, MaxPlayers: 32, Mode: CTF, Password: true, Hostname: "Ye Olde Server", Map: "ctf_Ash"}

func TestRequest(t *testing.T) {
	b := Request(0xdeadbeef)
	want := append([]byte{0xFF, 0xFF, 0xFF, 0xFF, 'B', 'S', 'Q', 'i', 0xef, 0xbe, 0xad, 0xde}, make([]byte, RequestSize-12)...)
	if !bytes.Equal(b, want) {
		t.Fatalf("request = % x", b)
	}
}

func TestReplyGolden(t *testing.T) {
	info, err := ParseReply(golden, 77)
	if err != nil || info != goldenInfo {
		t.Fatalf("ParseReply = %+v, %v", info, err)
	}
	if b := AppendReply(nil, 77, goldenInfo); !bytes.Equal(b, golden) {
		t.Fatalf("AppendReply = % x", b)
	}
}

func TestReplyRefusals(t *testing.T) {
	if _, err := ParseReply(golden, 78); err == nil {
		t.Error("a reply to another nonce is taken")
	}
	if _, err := ParseReply(golden[:len(golden)-1], 77); err == nil {
		t.Error("a cut reply is taken")
	}
	if _, err := ParseReply(append(bytes.Clone(golden), 0), 77); err == nil {
		t.Error("a reply with bytes after is taken")
	}
	long := bytes.Clone(golden)
	long[19] = maxHostname + 1
	if _, err := ParseReply(long, 77); err == nil {
		t.Error("a name too long is taken")
	}
	if _, err := ParseReply(Request(77), 77); err == nil {
		t.Error("a request is taken as a reply")
	}
	if b := AppendReply(nil, 1, Info{Hostname: string(bytes.Repeat([]byte("x"), 40))}); len(b) > RequestSize {
		t.Errorf("a reply of %d bytes outgrows its request", len(b))
	}
}

// fakeServer answers queries on a loopback port, ignoring the first `drop` of them.
func fakeServer(t *testing.T, info Info, drop int) netip.AddrPort {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if n < RequestSize || [4]byte(buf[4:8]) != requestTag {
				continue
			}
			if drop > 0 {
				drop--
				continue
			}
			nonce := uint32(buf[8]) | uint32(buf[9])<<8 | uint32(buf[10])<<16 | uint32(buf[11])<<24
			conn.WriteToUDPAddrPort(AppendReply(nil, nonce, info), from)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

func TestProbe(t *testing.T) {
	addr := fakeServer(t, goldenInfo, 1) // the first is lost; the second attempt is answered
	info, rtt, err := Probe(context.Background(), addr, 900*time.Millisecond)
	if err != nil || info != goldenInfo || rtt < 0 {
		t.Fatalf("Probe = %+v, %v, %v", info, rtt, err)
	}
}

func TestProbeSilence(t *testing.T) {
	addr := fakeServer(t, goldenInfo, 100)
	start := time.Now()
	if _, _, err := Probe(context.Background(), addr, 300*time.Millisecond); err == nil {
		t.Fatal("a silent server answered")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %s on a 300ms timeout", waited)
	}
}
