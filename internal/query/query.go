// Package query speaks the game server's query: one UDP datagram asking what a server
// is playing, one answering. The layout is the game's shared/network/query.h, and the
// two must agree byte for byte:
//
//	request  FF FF FF FF 'B' 'S' 'Q' 'i'  nonce:u32  zeros to RequestSize
//	reply    FF FF FF FF 'B' 'S' 'R' 'i'  nonce:u32  protocol:u16
//	         players:u8 bots:u8 max_players:u8 mode:u8 flags:u8
//	         hostname:(len:u8, bytes)  map:(len:u8, bytes)
//
// Integers are little-endian.
package query

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

const (
	RequestSize  = 128 // padded, so a reply is never longer than its request
	flagPassword = 1
	maxHostname  = 23 // NET_NAME_SIZE - 1
	maxMap       = 63 // NET_MAP_SIZE - 1
)

var (
	magic      = [4]byte{0xFF, 0xFF, 0xFF, 0xFF}
	requestTag = [4]byte{'B', 'S', 'Q', 'i'}
	replyTag   = [4]byte{'B', 'S', 'R', 'i'}
)

// Mode is the game's MatchMode.
type Mode uint8

const (
	Deathmatch Mode = 0
	CTF        Mode = 1
)

// Info is what a server says of itself.
type Info struct {
	Protocol   uint16 // the game's NET_VERSION
	Players    uint8  // people, bots apart
	Bots       uint8
	MaxPlayers uint8
	Mode       Mode
	Password   bool
	Hostname   string
	Map        string
}

// Request is a query's bytes, carrying nonce.
func Request(nonce uint32) []byte {
	b := make([]byte, RequestSize)
	copy(b[0:], magic[:])
	copy(b[4:], requestTag[:])
	binary.LittleEndian.PutUint32(b[8:], nonce)
	return b
}

var errBadReply = errors.New("query: not a reply to this request")

// ParseReply reads a reply to the request carrying nonce: whole, and nothing after.
func ParseReply(b []byte, nonce uint32) (Info, error) {
	if len(b) < 19 || [4]byte(b[0:4]) != magic || [4]byte(b[4:8]) != replyTag || binary.LittleEndian.Uint32(b[8:]) != nonce {
		return Info{}, errBadReply
	}
	info := Info{
		Protocol:   binary.LittleEndian.Uint16(b[12:]),
		Players:    b[14],
		Bots:       b[15],
		MaxPlayers: b[16],
		Mode:       Mode(b[17]),
		Password:   b[18]&flagPassword != 0,
	}
	rest := b[19:]
	var ok bool
	if info.Hostname, rest, ok = str(rest, maxHostname); !ok {
		return Info{}, errBadReply
	}
	if info.Map, rest, ok = str(rest, maxMap); !ok || len(rest) != 0 {
		return Info{}, errBadReply
	}
	return info, nil
}

func str(b []byte, max int) (string, []byte, bool) {
	if len(b) < 1 {
		return "", nil, false
	}
	n := int(b[0])
	if n > max || len(b)-1 < n {
		return "", nil, false
	}
	return string(b[1 : 1+n]), b[1+n:], true
}

// AppendReply lays info out as a server would. The lobby never answers queries; this
// is for tests standing in for a server.
func AppendReply(b []byte, nonce uint32, info Info) []byte {
	b = append(b, magic[:]...)
	b = append(b, replyTag[:]...)
	b = binary.LittleEndian.AppendUint32(b, nonce)
	b = binary.LittleEndian.AppendUint16(b, info.Protocol)
	var flags byte
	if info.Password {
		flags |= flagPassword
	}
	b = append(b, info.Players, info.Bots, info.MaxPlayers, byte(info.Mode), flags)
	b = appendStr(b, info.Hostname, maxHostname)
	return appendStr(b, info.Map, maxMap)
}

func appendStr(b []byte, s string, max int) []byte {
	if len(s) > max {
		s = s[:max]
	}
	return append(append(b, byte(len(s))), s...)
}

// Probe asks the server at addr the query, again until timeout runs out (a datagram
// may be lost), and returns what it says and how long the answer took.
func Probe(ctx context.Context, addr netip.AddrPort, timeout time.Duration) (Info, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(addr))
	if err != nil {
		return Info{}, 0, err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return Info{}, 0, err
	}

	var nb [4]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return Info{}, 0, err
	}
	nonce := binary.LittleEndian.Uint32(nb[:])
	request := Request(nonce)

	const attempts = 3
	reply := make([]byte, 1500)
	for i := 0; i < attempts; i++ {
		sent := time.Now()
		if _, err := conn.Write(request); err != nil {
			return Info{}, 0, err
		}
		_ = conn.SetReadDeadline(minTime(deadline, sent.Add(timeout/attempts)))
		for {
			n, err := conn.Read(reply)
			if err != nil {
				break // this attempt's time is up, or the port refused: try again while there is time
			}
			if info, err := ParseReply(reply[:n], nonce); err == nil {
				return info, time.Since(sent), nil
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	return Info{}, 0, fmt.Errorf("query: no answer from %s within %s", addr, timeout)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
