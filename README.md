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

Nodes rank themselves from real traffic to your Emby server. One controller
keeps each session on a node shown to deliver what its buffer needs, and
moves it when that node falls short, or when a speed test finds a node far
faster, so each playback reads through one exit IP for long stretches, plus
a second for the few seconds of a speed test.

Embolt is not affiliated with Emby LLC, and it is not a tool for evading a
server's rules on IPs, streams or downloads.

## Run it

```sh
mkdir -p config data
cp config.example.yaml config/config.yaml   # set upstream.url and your provider
docker compose up -d                        # edit compose.yaml's OWNER first
```

Point your Emby apps at `http://<host>:8096` instead of the server. The pane
is on `http://<host>:9090`: what is playing, each session's last 10 minutes of
buffer and rate with its switches and speed tests, the last day's sessions,
and every node's estimates. It is read-only and never shows a secret; to
change anything, edit the config, which reloads by itself.

| Path | Purpose |
| --- | --- |
| `/config/config.yaml` | config, mounted read-only |
| `/data` | node estimates, image cache, last good subscriptions |
| `:8096` (`:8920` with TLS) | Emby-compatible ingress |
| `:9090` | pane, `/api/v1/*`, `/metrics`, `/healthz` |

## How it decides

Every node keeps an exponentially weighted mean and spread of its log-rate,
whose evidence halves every 2 h (`half_life`), and the same of its RTT from
pings. Its *rate now* adds its samples of the last minute or so, so a sag
half a minute old decides. Its *safe rate* is the rate now one spread under
the typical: what it keeps to most of the time. The spread counts the
typical's own doubt, 0.5/√*w* in log units for *w* samples' worth of evidence
left, so a node measured once, or hours ago, is safe at far less than its
typical rate, and one never measured at nothing.

For a buffer of *B* seconds, bitrate *V*, low mark *B*<sub>min</sub> = 10 s and
horizon *H* = 120 s, a session needs *V* · (1 − (*B* − *B*<sub>min</sub>) / *H*):
1.08 *V* from an empty buffer, 0.58 *V* from a full one. A node *meets the
target* when its safe rate covers that need, judged at *B*
less a switch's gap for any node but the session's own. Every 2 s, each
session:

1. stays if its node meets the target (a full read-ahead always does);
2. else switches to the node that meets it with the highest safe rate;
3. else takes whichever node, its own included, falls least short;
4. and once on its node for 5 min, switches for speed alone to a node tested
   in the last 2 min whose safe rate is 1.5× its own node's rate now.

No byte for 4 s, or a connection error, fails over at once. The primary is
the node quickest to deliver a page of browsing, four round trips and 3 MB,
judged at the RTT it may have given its pings, and gives way only to a
node 100 ms quicker, while nothing plays.

Rates are learned from playback alone. Every stream samples its media node,
and a session tests other nodes beside it: while its media node keeps
reading, another node reads the stretch just past the read-ahead, up to
32 MB for up to 8 s, and its bytes are dropped. A node that fails the test
costs the session nothing. Only a node that may be faster is tested: one
never measured, or one whose typical rate, raised by its doubt, beats the
media node's and that was not sampled for 30 min; of those, the one that
may be fastest, never measured first. One test runs at a time, within a
budget: after a test, a session waits until it has played 1/`probes.budget`
(20×) the test's bytes, and at least 30 s. A test costs a second connection, through a
second exit IP.

### Logs

Every choice is logged at info, as `key=value` text, so `docker compose logs`
shows the strategy at work:

| Message | When | Says |
| --- | --- | --- |
| `playback started` | a session starts | its node, why, and the best options as `node safe/need` (✓ meets) |
| `session` | every 30 s while it stays | buffer, whether the read-ahead filled, rate now, safe rate, need, verdict |
| `move` | a step decides to switch | `reason` risk or faster, why, the session's safe rate and need, the options |
| `switched` | a feed reconnects on another node | `reason` risk, faster, stall or error, buffer, rate |
| `explore` / `explored` | a speed test starts / ends | the node, why it was tested, what it delivered, its typical rate after |
| `primary switched` | the primary changes | burst times of both |
| `pinged` | each ping round | nodes, how many answered |
| `breaker open` | a node faults 3 times | until when |
| `playback ended` | a session ends | node, failovers, tests, minutes |

```sh
docker compose logs embolt | grep -E 'msg="?(move|switched|explore)'
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
| `cmd/embolt` | `serve`, `healthcheck`, `version` |
| `internal/config` | schema, defaults, validation, hot reload |
| `internal/nodes` | subscriptions, the mihomo wrapper (only importer of mihomo), transports |
| `internal/measure` | node estimates, breaker, ping |
| `internal/control` | roles, switch and migration rules, speed tests, decision logs |
| `internal/proxy` | ingress, classifier, PlaybackInfo, media lane, failover, HLS |
| `internal/cache` | image and subtitle disk LRU |
| `internal/view` | secret-free DTOs, source of the TypeScript types |
| `internal/webapi` | pane, API, metrics, event journal |

## License

GPL-3.0, because mihomo is. See `LICENSE` and `THIRD_PARTY_NOTICES`.
