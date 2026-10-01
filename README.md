# soldatreloaded-lobby

The list of [soldatreloaded](https://github.com/soldatreloaded/soldatreloaded) game
servers that a server browser shows.

```
go run . -listen :8080
go test ./...
```

## How it works

A game server that wants to be listed sends a heartbeat over HTTP every
`heartbeat_seconds`, with its game port. The lobby takes the server's address from the
connection, never from the body, so a server can only list itself. Before listing it,
the lobby sends the game's **query** to that address and port over UDP. A server that
doesn't answer is not listed, which is usually a port that isn't forwarded. A server
stays listed for `-ttl` after its last heartbeat.

A browser fetches the list, then queries each server itself for its ping and current
player count. The list's own counts are only as fresh as the last heartbeat.

The query's bytes are defined in the game's `shared/network/query.h`, and
`internal/query` reads them. Both repositories test the same golden reply, so a layout
change that breaks one side fails a test on the other.

## API

| | | |
|---|---|---|
| `POST /v1/servers` | `{"port": 23073}` | heartbeat. `200 {"address","port","heartbeat_seconds"}`; `422` unreachable over UDP; `429` too many servers from this address; `400` bad body or an IPv6 source (ENet 1.3 is IPv4 only) |
| `DELETE /v1/servers` | `{"port": 23073}` | a server going away; `204` |
| `GET /v1/servers` | | `{"servers": [{"address","port","name","map","mode","players","bots","max_players","password","protocol","last_seen"}]}`, fullest first. `mode` is the game's `MatchMode`: 0 deathmatch, 1 capture the flag |
| `GET /healthz` | | `ok` |

A heartbeat less than a third of the interval after the last one is accepted without
querying the server again.

## Flags

| flag | default | |
|---|---|---|
| `-listen` | `:8080` | HTTP address |
| `-heartbeat` | `30s` | the interval servers are told to use |
| `-ttl` | `95s` | how long a server stays listed after its last heartbeat |
| `-probe-timeout` | `2s` | how long to wait for the query's answer, across three tries |
| `-max-per-ip` | `16` | the most servers listed from one address |
| `-client-ip-header` | none | take the source address from this header instead of the connection: `Fly-Client-IP` on Fly, `X-Real-IP` or `X-Forwarded-For` (its last entry) behind nginx or Caddy. Set it only behind a proxy that sets the header, or anyone can list someone else's address |

## Deploy

**Fly.io.** `fly.toml` is ready, with `Fly-Client-IP` already set:

```
fly launch --no-deploy --copy-config --name <your-app>
fly deploy --ha=false
```

Run exactly one machine. The list lives in memory, so a second machine would hold
half of it. That is why `--ha=false` is needed and why `fly.toml` never auto-stops
the machine.

Game servers must reach the lobby over **IPv4**, because ENet 1.3 can't be reached
over IPv6 and the lobby rejects an IPv6 heartbeat. Either have the server's heartbeat
force IPv4 (curl's `CURLOPT_IPRESOLVE`), or release the app's IPv6 address so that
only the shared IPv4 one remains (`fly ips list`, then `fly ips release <v6>`).

**Anywhere else.** Use the Dockerfile:

```
docker build -t soldatreloaded-lobby .
docker run -p 8080:8080 soldatreloaded-lobby
```

Put it behind a TLS-terminating proxy and pass `-client-ip-header` to match that
proxy, e.g. `docker run ... soldatreloaded-lobby -listen :8080 -client-ip-header X-Real-IP`.
