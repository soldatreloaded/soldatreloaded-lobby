// The soldatreloaded lobby: the list of game servers a server browser shows. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/soldatreloaded/soldatreloaded-lobby/internal/api"
	"github.com/soldatreloaded/soldatreloaded-lobby/internal/query"
	"github.com/soldatreloaded/soldatreloaded-lobby/internal/registry"
)

func main() {
	listen := flag.String("listen", ":8080", "the address to serve HTTP on")
	heartbeat := flag.Duration("heartbeat", 30*time.Second, "how often game servers are told to heartbeat")
	ttl := flag.Duration("ttl", 95*time.Second, "how long a server stays listed after its last heartbeat")
	probeTimeout := flag.Duration("probe-timeout", 2*time.Second, "how long to wait for a game server to answer the query")
	maxPerIP := flag.Int("max-per-ip", 16, "the most servers listed from one address")
	clientIPHeader := flag.String("client-ip-header", "", "take the client's address from this header, e.g. Fly-Client-IP (only behind a proxy that sets it)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	reg := registry.New(*ttl, *maxPerIP)
	handler := api.New(api.Config{
		Registry: reg,
		Probe: func(ctx context.Context, addr netip.AddrPort) (query.Info, error) {
			info, _, err := query.Probe(ctx, addr, *probeTimeout)
			return info, err
		},
		Heartbeat:      *heartbeat,
		MinInterval:    *heartbeat / 3,
		ClientIPHeader: *clientIPHeader,
		Log:            log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		t := time.NewTicker(*ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if n := reg.Expire(now); n > 0 {
					log.Info("expired", "servers", n)
				}
			}
		}
	}()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      *probeTimeout + 10*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Info("listening", "addr", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
