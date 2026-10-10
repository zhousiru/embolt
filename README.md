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
keeps each session on its node while the buffer holds, and moves it when the
node falls behind the bitrate, or when a speed test finds a node far faster, so each
playback reads through one exit IP for long stretches, plus a second for the
few seconds of a speed test.

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
read-ahead and rate with its switches and speed tests, the last day's sessions,
and every node's estimates. It is read-only and never shows a secret; to
change anything, edit the config, which reloads by itself.

| Path | Purpose |
| --- | --- |
| `/config/config.yaml` | config, mounted read-only |
| `/data` | node estimates, image cache, last good subscriptions |
| `:8096` (`:8920` with TLS) | Emby-compatible ingress |
| `:9090` | pane, `/api/v1/*`, `/metrics`, `/healthz` |

## Local viewing profile

Set `profile: local` to keep progress, watched marks and preferences in
`data_dir/profile.json`. Continue Watching uses Emby's `Items/Resume`
semantics: `IncludeNextUp` defaults to true, while false returns only resume
points. `Shows/NextUp?LegacyNextUp=true` supplies the traditional home row;
with `SeriesId`, it supplies the playback queue, including already-watched
episodes after the selected starting point. The series' normal episode list
stays complete and carries the local watched marks and progress.

The list behavior is tested against recorded responses from Emby Server
4.9.5.0, the compatibility target for these endpoints. The
[reference cases](internal/proxy/testdata/emby-4.9.5-nextup.json) cover resume,
completion, rewatching, specials, multiple series, filters, paging and empty
results. `go test ./internal/proxy -run TestProfileListsMatchEmby495` checks
those responses against the proxy with conflicting upstream user data.

The [compatibility audit](docs/local-profile-compatibility.md) tracks verified
write contracts and remaining differences from Emby 4.9.5, including folder
counts, Latest selection and multi-client notifications.

## How it decides

Every node keeps a moving average of its rate, from 2 s samples of real
playback, and of its RTT, from pings. Each new sample weighs 0.3, so a sag
of a few samples shows at once. A node never measured ranks below every
node measured.

A session starts on the primary when the primary's rate is 1.2× the
bitrate, so browsing and playback share one exit IP; else on the fastest
node. Embolt judges only its own read-ahead and never guesses what the
player holds: a read-ahead that runs out is not a stall, but a node
delivering under the bitrate drains the player. Every 2 s, each session:

1. stays for 20 s after a switch;
2. moves to the fastest other node when its node has fallen *behind*:
   while the read-ahead was under 20 s and not filling, upstream delivered
   3 s of media less than the bitrate calls for, net of what it delivered
   over, unless that node is no faster than its own: then the server, not
   the node, is slow. One step's rate swings widely; the shortfall counts
   only as it adds up over consecutive steps. A step while a connection
   opens or ramps up counts for nothing, since a start, a seek, a resume or
   a switch costs any node its setup. A full or long read-ahead, a switch,
   or a spell with no player clears it;
3. once on its node for 5 min, moves for speed alone to a node measured in
   the last 30 min at 1.5× its own node's rate;
4. else stays.

No answer within 8 s, no byte for 4 s, or a connection error fails over at
once to the fastest other node. The primary is the node quickest to deliver
a page of browsing, four round trips and 3 MB, and gives way only to a node
100 ms quicker, while nothing plays.

Rates are learned from playback alone. Every stream samples its media node,
and a session tests other nodes beside it: while its media node keeps
reading, another node reads the stretch just past the read-ahead, up to
32 MB for up to 8 s, and its bytes are dropped. A node that fails the test
costs the session nothing. Every node takes its turn: never tested or
measured first, then the one whose last test or sample came longest ago; a
node tested or measured in the last 30 min waits, whether its test
succeeded or not. One test runs at a time, within a budget: after a test, a
session waits until it has played 1/`probes.budget` (20×) the test's bytes,
and at least 30 s. A test costs a second connection, through a second exit
IP.

### Logs

Every choice is logged at info, as `key=value` text, so `docker compose logs`
shows the strategy at work:

| Message | When | Says |
| --- | --- | --- |
| `playback started` | a session starts | its node, why, and the fastest nodes as `node Mbps` |
| `session` | every 30 s while it stays | read-ahead, deficit, whether it filled, what upstream delivered, its node's rate, verdict |
| `move` | a step decides to switch | `reason` risk or faster, why, read-ahead, deficit, what upstream delivered, the fastest others |
| `switched` | a feed reconnects on another node | `reason` risk, faster, stall or error, read-ahead, both nodes' rates |
| `explore` / `explored` | a speed test starts / ends | the node, why it was tested, what it delivered, its rate after |
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
