# Embolt

Embolt sits between your Emby apps and one Emby server and sends each request
through the [mihomo](https://github.com/metacubex/mihomo) node best suited to it:

- **Control traffic** (browsing, API, images, websocket) rides one sticky
  *primary* node, the quickest to deliver a page of browsing: low latency and
  enough speed for its images. Images and subtitles come from a local disk cache.
- **Video** rides a *media* node fast enough for the bitrate, through a 60 s
  read-ahead buffer. When the media node stalls or fails, the stream resumes
  on the best other node at the next byte, and the player never sees the
  switch.

Nodes rank themselves from real traffic to your Emby server. Each node holds
a learned belief about its rate and RTT; one controller keeps the predicted
probability of a stall under 1%, and moves a session only when it must, so
each playback reads through one exit IP whenever possible, plus a second for
the few seconds of a speed test.

Embolt is not affiliated with Emby LLC, and it is not a tool for evading a
server's rules on IPs, streams or downloads.

## Run it

```sh
mkdir -p config data
cp config.example.yaml config/config.yaml   # set upstream.url and your provider
docker compose up -d                        # edit compose.yaml's OWNER first
```

Point your Emby apps at `http://<host>:8096` instead of the server. The pane
is on `http://<host>:9090`: roles, every node's beliefs, and each session with
its buffer, stall risk, the node choice its controller faces, and its events.
It is read-only and never shows a
secret; to change anything, edit the config, which reloads by itself.

| Path | Purpose |
| --- | --- |
| `/config/config.yaml` | config, mounted read-only |
| `/data` | beliefs, sample log, image cache, last good subscriptions |
| `:8096` (`:8920` with TLS) | Emby-compatible ingress |
| `:9090` | pane, `/api/v1/*`, `/metrics`, `/healthz` |

## How it decides

For a buffer of *B* seconds, bitrate *V*, low mark *B*<sub>min</sub> = 10 s and
horizon *H* = 120 s, the buffer holds only if the next *H* seconds average
more than *V* · (1 − (*B* − *B*<sub>min</sub>) / *H*). Each node's rate belief
is a Normal–Inverse-Gamma posterior on log-rate whose evidence halves every
2 h; its Student-t predictive gives the probability of falling short. Every
2 s, each session takes the cheapest action that keeps that probability at or
under 1%: stay, or switch to the best other node. No byte
for 4 s, or a connection error, fails over at once.

Rates are learned from playback alone. Every stream samples its media node,
and a session tests other nodes beside it: while its media node keeps
reading, another node reads the stretch just past the read-ahead, up to
32 MB for up to 8 s, and its bytes are dropped. A node that fails the test
costs the session nothing. A session that meets its target, with another
node known to meet it from an empty buffer, tests nothing; as that node's
belief fades, it stops counting and tests resume. Otherwise each node draws
once from its rate posterior (Thompson sampling), and a node that out-draws
the media node is tested, so barely measured nodes that may be fast are
tried first, known fallbacks stay fresh, and known slow nodes are left
alone. One test runs at a time, within
a budget: a session earns `probes.budget` (5%) of its bitrate while it
plays, whatever its node delivers, so a session stuck on a slow node may
test as often as any, and tests of slow nodes cost fewer bytes. A test costs
a second connection, through a second exit IP.
`embolt replay` runs the model over the logged samples and reports
how well calibrated it is, to tune `half_life` and `prior_strength` on your
own data:

```sh
docker compose exec embolt embolt replay --samples /data/samples
```

## Develop

Needs Go 1.26+ and Bun. [Task](https://taskfile.dev) wraps the commands.

```sh
task test        # go vet + go test, no Bun needed (-tags noui)
task dev         # backend on :8096 and :9090, using ./config.yaml
cd web && bun run dev   # pane with hot reload on :3000, API proxied to :9090
task gen         # regenerate web/src/api/types.gen.ts after changing internal/view
task build       # bin/embolt with the pane embedded
```

| Package | Holds |
| --- | --- |
| `cmd/embolt` | `serve`, `replay`, `healthcheck`, `version` |
| `internal/config` | schema, defaults, validation, hot reload |
| `internal/nodes` | subscriptions, the mihomo wrapper (only importer of mihomo), transports |
| `internal/measure` | beliefs, breaker, ping, sample log |
| `internal/control` | stall risk, roles, switch actions, Thompson-sampled exploration |
| `internal/proxy` | ingress, classifier, PlaybackInfo, media lane, failover, HLS |
| `internal/cache` | image and subtitle disk LRU |
| `internal/view` | secret-free DTOs, source of the TypeScript types |
| `internal/webapi` | pane, API, metrics, event journal |

## License

GPL-3.0, because mihomo is. See `LICENSE` and `THIRD_PARTY_NOTICES`.
