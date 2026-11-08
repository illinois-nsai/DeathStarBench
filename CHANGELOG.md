# Changelog — hotelReservation

Changes on the `yinjie/hotel-res` branch, forked from `htliang2/hotel-res-dev`.

Every entry here was found by trying to **build the benchmark from source**. The cluster had
always pulled the prebuilt image `docker.io/htliang/hotelreservation-remove-consul:latest`,
which was built from a tree that was never pushed to this repository — so the source and the
running binaries had silently disagreed for the whole life of the project. Nothing compiled it,
so nothing noticed.

Newest first.

---

## 2026-09-15 — an uncapped mongo connection pool killed mongod on its fd limit

The trigger underneath the panic below. Fixing the panic stopped the *application* from dying;
mongod was still dying on its own.

No `maxPoolSize` was set at any of the eight `cmd/*/db.go` sites, so each client took the Go
driver default of **100 connections**. Seven replicas therefore opened up to **700 connections
to a single mongod**, which runs with the container runtime's default soft limit of **1024 file
descriptors** — measured, not assumed:

```
$ docker run --rm mongo:latest sh -c 'grep -i "open files" /proc/1/limits'
Max open files            1024                 524288               files
```

That budget is shared with data files and the journal, so 700 connections sits right at the
edge, which is why it failed under load rather than immediately:

```
"Error accepting new connection on local endpoint",
  "localEndpoint":"0.0.0.0:27017", "error":"Too many open files"

mongodb-reservation: Error, restarts=2, exitCode 14
```

The client saw each severed connection as `incomplete read of message header`, which is exactly
what `MakeReservation` then panicked on.

Fixed by capping the pool at all eight sites:

```go
opts := options.Client().ApplyURI(uri).SetMaxPoolSize(50)
```

50 keeps seven replicas near 350 connections, roughly a third of the limit, leaving room for
data files and bursts. The cap is real backpressure — requests queue in the client instead of
exhausting mongod — which is the correct behaviour under overload and is what makes the
saturation curve meaningful rather than a record of a datastore restarting.

Raising mongod's limit instead was rejected: Kubernetes has no per-container ulimit, so it would
need a privileged initContainer or a change to containerd's defaults on every node. Capping the
client is smaller and fixes the cause rather than widening the symptom.

**Verified after the panic fix but before this one:** all seven reservation pods held
`restarts=0` while logging 63-78 insert failures each, so the two defects are independent and
both were real. The panic fix is what makes this failure mode observable at all.

---

## 2026-09-15 — `MakeReservation` panicked on mongo errors and crash-looped the deployment

The 9/12 pass converted every **memcached** error into a cache miss. It left two **mongo** error
paths in `MakeReservation` still calling `log.Panic`, and that is the reserve-only code path —
`search` reaches `CheckAvailability` but never this.

```go
// :174  capacity lookup, on a _cap cache miss
log.Panic().Msgf("Tried to find hotelId [%v], but got error", hotelId, err.Error())
// :213  one insert per booked night, up to 15 per request
log.Panic().Msgf("Tried to insert hotel [hotelId %v], but got error", hotelId, err.Error())
```

At 7 replicas all seven pods write to a single `mongodb-reservation` — roughly 700 driver
operations contending for one mongod against 300 at three replicas. Inserts begin timing out,
and every timeout killed a pod.

### What it looked like

10 of the 168 experiments in the 9/15 sweep, all of them `reserve` at 7 replicas, load >= 2000,
in both iterations: 12-70% of requests failing with grpc=14.

The signature is only legible with both istio reporters. Source (frontend's envoy) reported
`URX` alone — it reached reservation's envoy fine. Destination (reservation's own envoy)
reported `URX,UF`: it could not connect to its **own app container on localhost**.

cAdvisor container count per 15s, app container versus the sidecar beside it:

| run | app containers present |
|---|---|
| reserve 4000rps **r3** (clean) | `3 3 3 3 3 3 3 3 3 3 3 3 3` |
| search 6000rps **r7** (clean) | `7 7 7 7 7 7 7 7 7 7 7 7 7` |
| **reserve 4000rps r7** (failing) | `7 7 7 4 1 1 6 1 6 1 1` |

The sidecar reported 7/7 throughout in all three. One pod appeared as five distinct container
instances inside a 210s run, each starting fresh at 3 MiB. `container_oom_events_total` was 0
and memory peaked at 145 MiB of 1024, so this was the process exiting, not the kernel killing
it — the app burned 4.41 millicores while its sidecar burned 3085.

The two controls isolate it: same mix with fewer replicas is stable, and a heavier load at the
same replica count but a different mix is stable. Only the intersection fails.

### Fixed

Both sites now log the error and return it, failing the request instead of the process. Ten
corrupted experiments become ten experiments showing honest overload, which is a usable
datapoint rather than a hole.

Note on `:213`: earlier nights in that loop are already committed and there is no transaction,
so a mid-loop return leaves a partial reservation. That is pre-existing; reporting the failure
beats crashing and beats silently claiming success.

Neither format string was ever valid — one verb, two arguments, so the panic message itself
would have printed `%!(EXTRA string=...)`. Nothing upstream had exercised these paths. After
the fix there are **no `log.Panic` or `panic(` calls left in any service**; the only remaining
`log.Fatal` calls are startup bind failures, which should be fatal.

---

## 2026-09-14 — a warm capacity cache made every hotel unavailable

`CheckAvailability` built `cacheCap` under two different key shapes and read it under only one:

```go
cacheCap[k] = hotelCap              // :264  k is "<hotelId>_cap"  (from memcached)
cacheCap[num.HotelId] = num.Number  // :294  key is "<hotelId>"    (from mongo)
...
if val+int(req.RoomNumber) <= cacheCap[id]                     // :363  reads "<hotelId>"
if count+int(req.RoomNumber) <= cacheCap[queryItem["hotelId"]] // :407  reads "<hotelId>"
```

Every capacity served from cache was therefore unreachable: the lookup missed, Go returned the
zero value, and `count + RoomNumber <= 0` is false — so the hotel was marked unavailable. Only
the cold-cache mongo path worked. With the capacity cache at a **1.000 hit rate** after the
first seconds, `CheckAvailability` returned an empty list for the rest of every run.

The cost is silent, because an empty result is a valid one. `searchHandler` passes
`reservationResp.HotelId` straight into `GetProfiles`, so profile received an empty id list,
and `gomemcache`'s `GetMulti` does no network I/O on an empty key set:

| mix | frontend->profile response | profile memcached gets |
|---|---|---|
| recommend | 781.8 bytes/req | (not on this path) |
| **search** | **41.8 bytes/req** — the empty floor | **0/s** |

`memcached-profile` held 22 items written during the brief cold window and never read again.

Fixed by keying the cache-sourced entries on the bare hotelId, `strings.TrimSuffix(k, "_cap")`,
so all four sites agree.

Upstream and pre-existing — identical in `illinois-nsai/hotelReservation-workspace` — and
present in the pre-fix 9/14 sweep, so it is **not** a regression from the `rate` query fix
below. Search measurements taken before this exercised five of the path's six hops, with
profile a no-op; they remain internally comparable, since the same path ran in every
experiment.

---

## 2026-09-14 — seed data duplicated per replica, and `rate` read the whole collection

**Replica count was silently changing the workload.** Both bugs corrupted every `search`
measurement ever taken, at every replica count, including one.

### Fixed — every replica re-seeded the database at startup

`initializeDatabase()` runs in each replica's `main()` and did an unconditional `InsertMany`, so
N replicas produced N copies of every seed document:

```
rate replicas: 7  ->  rate-db.inventory: 189 documents, 27 distinct hotelIds
```

New `seed` package with `Upsert` (`UpdateOne` + `SetUpsert`) and `EnsureUniqueIndex`, applied to
all four `cmd/*/db.go`. Upsert alone is not enough — replicas start simultaneously, so two can
both find no document and both insert one; the unique index turns that race into a duplicate-key
error on the loser.

| collection | unique key | index |
|---|---|---|
| `rate-db.inventory` | hotelId + code + inDate + outDate | yes |
| `geo-db.geo` | hotelId | yes |
| `profile-db.hotels` | id | yes |
| `reservation-db.number` | hotelId | yes |
| `reservation-db.reservation` | hotelId + customerName + inDate + outDate | **no** |

The exclusion is deliberate: `services/reservation/server.go` inserts there at runtime and the
workload draws customers from a pool of 500, so repeat bookings of the same hotel and dates by
the same name are legitimate. A unique index would have rejected real bookings.

### Fixed — `rate` fetched every rate plan in the database to answer one hotel

`services/rate/server.go` queried with an empty filter and then cached the entire result under a
single hotel's key:

```go
curr, err := collection.Find(context.TODO(), bson.D{})   // ALL documents
for _, r := range tmpRatePlans {
    ratePlans = append(ratePlans, r)
    memcStr = memcStr + string(rateJson) + "\n"
}
s.MemcClient.Set(&memcache.Item{Key: id, Value: []byte(memcStr)})
```

So the cache entry for hotel 12 held plans for every other hotel, and the response carried them
all back to `search`. Now `Find(bson.M{"hotelId": id})`, plus a nil-cursor guard (`Find` returns
a nil cursor alongside its error) and the removal of the `log.Panic` that followed.

### The chain, and how it was isolated

N replicas -> N x seed docs -> N x rate response -> N x hotels into `search` -> N x hotel IDs in
`CheckAvailability` -> N x memcached keys per multiget.

Scaling one service at a time at 250 users settled it. Only `rate` matters:

| arm | gets/CheckAvailability | rate resp/req |
|---|---|---|
| all x1 | 374 | 5,687 |
| reservation x7 only | **378** | 5,811 |
| geo x7 only | **372** | 5,726 |
| **rate x7 only** | **827** | **41,507** |

`CheckAvailability` per frontend search stayed at 1.0 in every arm, which ruled out retries and
RPC multiplicity: the growth is in the payload of a single call.

### Verified after the fix

| replicas | rate docs | distinct | rate resp/req | bytes/CA | gets/CA |
|---|---|---|---|---|---|
| 1 | 27 | 27 | 113.2 | 205.5 | 4.6 |
| 3 | 27 | 27 | 115.6 | 204.9 | 4.7 |
| 7 | 27 | 27 | 122.0 | 204.5 | 4.7 |

Constant, against 27 / 81 / 189 documents and 374 / 1,123 / 2,659 gets before.

**The query fix dominates.** At one replica, where no duplication existed at all, `gets/CA` fell
**374 -> 4.6**. Every `search` measurement taken before this was of a workload roughly two orders
of magnitude more expensive than the benchmark intends; its ~145 req/s ceiling and ~2.3s
latencies were properties of the bug.

`Dockerfile` gained `COPY seed/ seed/`.

---

## 2026-09-13 — reservation inventory exhausted mid-run

`cmd/reservation/db.go` seeds 80 hotels / 19,750 rooms. The reserve workload books one room per
night and inserts one document per night, so across 15 bookable dates that is 296,250 room-date
slots — and a single 3-minute experiment consumed:

```
100 users ->  16%      500 users ->  79%
300 users ->  47%      700 users -> 111%   (exhausts mid-run)
```

Once a hotel-date fills, `CheckAvailability` returns early without inserting, so requests get
cheaper as a run proceeds — and cheaper faster at higher load. That put a load-dependent bias
directly into the latency label: reserve latency *fell* from 9.5 ms at 400 users to 7.4 ms at
700, and three identical back-to-back runs measured 37 / 19 / 16 ms.

Added `roomScale = 25`, keeping the 200/250/300 spread so hotels still differ. The top load level
now consumes ~4% of inventory instead of 111%. Re-running the three identical runs afterwards:
**median flat at 14 / 14 / 14 ms**, and run 1's 11.31% failure rate gone.

Scaling rooms rather than adding hotels is deliberate — hotel IDs 1-80 are seeded independently
by geo, profile and rate, so hotels 81+ would exist only in reservation and never be returned by
a search.

---

## 2026-09-13 — source could not build a working image

Three independent gaps between this repository and the prebuilt image, each of which stopped the
benchmark dead when built from source.

### `config.json` pointed at addresses that do not exist

The committed file is upstream's: `mongodb-geo:27017`, `consul:8500`. The deployed services are
`mongodb-geo-hotelres:27018`. Every service died at startup with `no such host`.

The chart does define a `service-config` configMap with the right addresses, but it is mounted at
the **relative** path `config.json`, which never resolves to `/workspace/config.json` where the
image's `WORKDIR` puts it — so the baked-in copy always wins and the configMap is dead weight.
Replaced with the working image's copy; the original is kept as `config.json.upstream`.

### Consul was required but is never deployed

`Chart.yaml` gates consul behind `condition: consul.enabled`, which is unset — the image this
cluster ran is literally named `remove-consul`. But `frontend` and `search` dial
`consul://<addr>/<name>` through `mbobakov/grpc-consul-resolver`, so with no consul the frontend
started and then hung forever resolving peers, answering nothing while reporting no errors.

`getGprcConn` in both files now dials Kubernetes DNS instead, with addresses in `config.json`
beside the mongo and memcached ones:

```go
return dialer.Dial("dns:///"+s.peerAddr(name), dialer.WithTracer(s.Tracer))
```

The explicit `dns:///` matters: a bare `search-hotelres:8082` parses as *scheme*
`search-hotelres`, and grpc 1.56.3 only lands correctly by falling back to `passthrough` when
that scheme has no registered resolver.

This keeps load balancing on the istio sidecar, where every measurement in the project assumes it
is. Enabling consul instead would have moved it to client-side gRPC round-robin over pod IPs.

`registry.go` registration is now best-effort — logged, not fatal — so a missing consul cannot
kill a service at startup.

---

## 2026-09-12 — a memcached timeout killed the whole process

`services/reservation/server.go` called `log.Panic()` when `CheckAvailability`'s ~130-key
multiget failed, so one timed-out cache read killed the Go process and every in-flight gRPC
stream with it. Pod state `reason=Error, exit=2, no signal`.

Chain: memcached misses the 2s `MEMC_TIMEOUT` -> reservation panics -> callers see
`grpc=14 UNAVAILABLE` / `flags=UC` -> retries exhaust -> `flags=URX` -> frontend 500. At 5
replicas, 19% of `frontend -> reservation` was failing and reservation was the **only** failing
edge; at 7 replicas all seven pods crashed within 125s of load start. More replicas made it
worse — they share one memcached.

Every memcached error is now handled exactly like a cache miss, in all seven sites across
`profile`, `rate`, `review` and `reservation`. Two things a naive edit would have broken:

- `profile` and `rate` keep the **mongo fallback inside the `else` block**, so swapping `Panic`
  for `Warn` would have skipped it and returned an empty result set — silently wrong data
  instead of a crash. The body had to be lifted out.
- `reservation`'s `GetMulti(reqCommand)` site would have **deadlocked**: `ch` is closed either by
  the hit goroutine when `err == nil` or by the WaitGroup goroutine on the `ErrCacheMiss` path. A
  timeout matched neither, so nothing would ever close the channel. Restructured around a single
  WaitGroup covering both sender groups.

### Also fixed, uncovered by the above

- `bson.D{{"$in", queryMissKeys}}` is not a valid filter — `$in` is an operator and must sit
  under the field it constrains. Mongo rejected the query. Now
  `bson.D{{"hotelId", bson.D{{"$in", queryMissKeys}}}}`.
- `Find` returns a **nil cursor alongside its error**, and `curr.All` was called on it regardless
  after only logging — a nil dereference at `services/reservation/server.go:279`. Guarded at
  three sites.
- Miss detection was gated on `err == memcache.ErrCacheMiss`, which `GetMulti` never returns, so
  the whole mongo cache-miss path was **dead code**: misses were silently treated as "available"
  without consulting the database. Detection now runs unconditionally, since `GetMulti` reports a
  partial hit as a nil error with only the keys it found.

---

## Helm chart

Not source, but shipped from here and required for any of the above to run.

- **`MEMCACHED_THREADS` and `MEMCACHED_CACHE_SIZE` never did anything — corrected 2026-09-15.**
  Both were set as environment variables on the memcached containers. The chart runs the
  **official** memcached image, whose entrypoint does not translate environment variables into
  flags; that is a Bitnami convention. So the settings were silently dropped and every
  experiment through the 9/15 sweep ran memcached at its built-in defaults. Read back from the
  exporter mid-sweep, on all three instances:

  ```
  memcached_limit_bytes     = 67108864   (64 MiB, not the declared 128)
  memcached_max_connections = 1024       (the default)
  ```

  The earlier entries here claimed `MEMCACHED_THREADS` 2 -> 8 took throughput from 931 to
  2312 req/s and mesh errors from 19% to 1.22%. **That attribution is wrong** — the setting
  never reached the process. The improvement came from other changes in the same window, most
  likely the memcached-panic fix and removing the sidecar from `memcached-reserve`. The
  cache-size reasoning (working set under 0.5 MiB, zero evictions) still holds and is in fact
  why running at 64 MiB instead of the intended 128 changed nothing measurable.

  Sizing now goes through `args` on each memcached chart, which the image does honour, and the
  `environments` block is gone. Values are **64 MiB / 4 threads** — deliberately the defaults
  already in force, so the chart states what actually runs without changing behaviour or
  invalidating the existing sweeps. Verified against the image with a control probe:
  `-m 64 -t 4` yields `maxbytes=67108864, num_threads=4`, and `-m 100 -t 6` yields
  `104857600 / 6`, so the flags are honoured rather than merely accepted.
- `sidecar.istio.io/inject: "false"` on `memcached-reserve`. Envoy burned 650 millicores against
  memcached's own 204 and the writes into it timed out. Costs
  `istio_tcp_*_bytes_total` on that one edge; mongodb and the other two memcached instances stay
  in the mesh.
- `prom/memcached-exporter` sidecar on all three memcached pods, annotated for the existing
  `kubernetes-pods` Prometheus job, so cache hit rate is recorded rather than inferred.
- `imagePullPolicy` `Always` -> `IfNotPresent`, and the eight app charts point at
  `library/hotelreservation-memcfix` — a locally built image imported straight into each node's
  containerd, since there is no registry.

---

## Building

There is no registry and no node has docker, buildkit or a Go toolchain — only containerd. The
image is built on the client node (not a Kubernetes member, so installing docker there cannot
disturb the kubelet's containerd) and imported into each app node over the cluster LAN:

```
bash impact-estimator/run_experiment/build_and_load_image.sh
```

**Re-cloning this repository from upstream discards every change above** and silently restores
the crash loop, the hung frontend and the 80x search amplification.
