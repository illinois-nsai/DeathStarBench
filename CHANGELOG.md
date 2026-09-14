# Changelog — hotelReservation

Changes on the `yinjie/hotel-res` branch, forked from `htliang2/hotel-res-dev`.

Every entry here was found by trying to **build the benchmark from source**. The cluster had
always pulled the prebuilt image `docker.io/htliang/hotelreservation-remove-consul:latest`,
which was built from a tree that was never pushed to this repository — so the source and the
running binaries had silently disagreed for the whole life of the project. Nothing compiled it,
so nothing noticed.

Newest first.

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

- `MEMCACHED_THREADS` 2 -> 8. Two threads fielded ~300k key lookups/s; throughput
  931 -> 2312 req/s and mesh errors 19% -> 1.22%.
- `MEMCACHED_CACHE_SIZE` 128 -> 1024 -> back to **128**. Measured working set is under 0.5 MiB
  with zero evictions, so the size was never the lever — 128 is ~1000x the working set.
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
