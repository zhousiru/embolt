# Local profile compatibility audit

Target: **Emby Server 4.9.5.0**, matching cabin's upstream at the time of
investigation (2026-10-11). Results below come from an isolated official
server with synthetic episodes and its OpenAPI schema, not production writes.
They are not a claim of compatibility with every Emby version or client.

The profile belongs to an Embolt deployment, not to each upstream user.
Playback reports still reach Emby because it owns playback sessions; local
history is recorded separately. Favorites, played marks, configuration and
preference writes are intercepted.

## Endpoint inventory

Paths optionally begin with `/emby`; `{u}` is an upstream user ID.

| Endpoint / surface | Official behavior and local status |
| --- | --- |
| `GET /Users/{u}/Items/Resume` | Video resume defaults to including next-up episodes. Global lists pick one continuation per series; a series parent selects its queue. Implemented; 884 recorded query comparisons cover this and NextUp, including paging, specials, ties, completed later episodes and ignored filters. |
| `GET /Shows/NextUp` | Bare query is empty in 4.9.5. `LegacyNextUp=true` selects the old global row; `SeriesId` selects a queue that can contain already played later episodes. Implemented. Empty results serialize as `Items: []`. |
| `GET /Shows/{id}/Episodes`, item details and other DTOs | Episode catalog remains intact, including watched episodes. UserData is overlaid with local state. Folder aggregation remains incomplete (see below). |
| `POST /Users/{u}/Items/{id}/UserData` | Returns 204. Missing position/played resets to 0/false; omitted count/date preserves them. Favorite and Likes inputs are ignored. Implemented against observed behavior. |
| `POST /Users/{u}/Items/{id}/HideFromResume` | Preserves progress and hides the series globally. Explicit series Resume/NextUp still displays its queue. Starting playback unhides the series. Implemented with persisted hidden state; unseen item metadata is fetched before determining scope. |
| `POST/DELETE /Users/{u}/FavoriteItems/{id}` | Returns local UserData and changes favorite only. Main flow supported. Invalid/inaccessible item validation is not yet equivalent. |
| `POST/DELETE /Users/{u}/PlayedItems/{id}`, `/Delete` | Default played mark keeps count at least one; an explicit DatePlayed increments count. Unplayed clears position/count/date. Series/season cascades enumerate children; failed enumeration now returns the error without partial mutation. Folder aggregate responses and invalid IDs remain incomplete. |
| `POST /Users/{u}/Items/{id}/Rating`, `/Rating/Delete` | Official 4.9.5 accepted these without persisting/exposing Likes in experiments. Local writes now clear legacy Likes instead of creating a divergent rating. Existing saved Likes/filter behavior is not migrated. |
| `POST /Sessions/Playing`, `/Progress`, `/Stopped` | Forwarded with original body. Local state now changes only on successful upstream response. Default 5%/90% thresholds, short media, repeated starts, failed stop and no-position stop were probed. Server-specific library settings and session validation still differ. |
| Legacy `/Users/{u}/PlayingItems/{id}` reports | Forwarded, with local mutation gated on upstream success. Full official legacy lifecycle not empirically covered. |
| `POST /Users/{u}/Configuration` | Replacement over official defaults. Local default configuration also overlays user DTOs before the first write. Implemented for schema field names used by clients. |
| `POST /Users/{u}/Configuration/Partial` | Merges into local configuration, returns 204, never forwards. Previously missed by interception and could change the shared account. Fixed. |
| `GET/POST /DisplayPreferences/{id}` | Keeps upstream DTO identity but starts with empty local CustomPrefs. POST replaces CustomPrefs; omitted map becomes empty; posted SortOrder is ignored (Ascending in observed 4.9.5). Implemented for normal client requests. Initial identity lookup still requires upstream availability. |
| `GET /Users/{u}/Items` with favorite/played/resumable filters | Local ID selection/exclusion exists. Large sets, grouped folder filtering and some sort/paging combinations remain incomplete. |
| `GET /Users/{u}/Items/Latest` | Only overlays/filter-removes returned DTOs; cannot restore items already excluded/grouped by upstream shared history. Not equivalent. |
| WebSocket `UserDataChanged` | Rewrites incoming user data locally. Does not synthesize broadcasts for locally intercepted mutations, so other clients may need a refresh. |

## Remaining work before claiming full parity

- **Folder aggregation:** derive series/season Played, UnplayedItemCount and
  related counts from all accessible local episode states. Upstream counts
  must not be reused because they belong to the shared viewer. Currently only
  explicitly completed folders get a zero unplayed count; other counts are
  omitted. This affects badges and some detail displays.
- **Latest / suggestions / similar / grouped discovery:** selection happens
  upstream before overlay. These need endpoint-specific local selection and
  pagination, respecting local configuration. A JSON overlay alone cannot
  guarantee parity.
- **Large filtered lists:** current generic filter path caps local IDs at 500.
  It needs batching with correct overall sorting and pagination; NextUp and
  Resume already use their separate batched path.
- **Playback sessions:** an upstream 2xx does not always prove a valid session
  changed history (e.g. a stop without a play-session key can be a no-op).
  Track accepted sessions and compare stale, duplicate and out-of-order
  reports. Read effective per-library resume thresholds instead of assuming
  server defaults.
- **Validation:** local write endpoints do not uniformly validate upstream
  item access, user ownership, enum/type/null semantics or malformed input as
  the official server does. The normal configuration field names are covered;
  case variants and invalid payload behavior are not fully covered.
- **Display preferences:** verify identifier/client case normalization and
  additional client variants. Old stored raw preference DTOs are retained;
  only SortOrder/SortBy and subsequent writes are normalized.
- **Notifications and concurrency:** local mutations need corresponding client
  notifications. Persisting hidden state and retrying failed file writes are
  covered, but multi-device session ordering needs further work.

## Reproducible checks in the repository

`internal/proxy/testdata/emby-4.9.5-nextup.json` contains synthetic metadata,
state changes and official expected list outputs. `profile_lists_test.go`
replays them against an upstream stub whose shared history contradicts the
local profile. `profile_writes_test.go` checks observed write contracts,
non-forwarding of local preferences/configuration, rejected reports, and
hidden-series list scope. Profile store tests cover hidden-state persistence,
metadata retention after clearing history, and retry after disk-write failure.

Run `go test -race -tags noui ./...` and `go vet -tags noui ./...`.
Live official-server experiments are additional evidence; the repository does
not bundle Emby binaries, credentials or production user data.
