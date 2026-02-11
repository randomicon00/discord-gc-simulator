# GC Latency Spike Simulator (Go)

A toy Go project to demonstrate how garbage-collection activity can create **tail-latency spikes** in latency-sensitive applications (chat systems, trading platforms, etc.), even when median latency remains similar.

## What This Simulates

- A large live cache (`ReadState`) to keep the GC live set meaningful.
- A request loop with fixed scheduled rate (`request-rate`) and per-request allocations.
- Two scenarios run back-to-back:
  - `Baseline`: no forced GC.
  - `Forced-GC`: triggers `runtime.GC()` on a fixed interval.
- Latency metrics per scenario: `mean`, `p50`, `p95`, `p99`, `p99.9`, `max`.
- Spike metrics:
  - Count of requests above `spike-threshold`.
  - Spike rate over time windows (`spike-window`).
  - Spikes occurring near forced GC events.

## Run

```bash
mkdir -p /tmp/go-cache
GOCACHE=/tmp/go-cache go run . -duration=10s
```

Useful flags:

- `-cache-entries` (default `500000`)
- `-request-rate` (default `2000`)
- `-temp-alloc-bytes` (default `8192`)
- `-stressed-forced-gc` (default `250ms`)
- `-spike-threshold` (default `5ms`)
- `-spike-window` (default `1s`)
- `-gomaxprocs` (default `1`)
- `-gc-percent` (default `100`)

## Results (Run on February 11, 2026)

Command used:

```bash
GOCACHE=/tmp/go-cache go run . -duration=10s
```

### Key numbers

| Metric | Baseline | Forced-GC |
|---|---:|---:|
| Mean | 584.743µs | 634.426µs |
| p50 | 580.36µs | 583.893µs |
| p95 | 1.120489ms | 1.150591ms |
| p99 | 1.214015ms | 3.466119ms |
| p99.9 | 1.290354ms | 6.272ms |
| Max | 5.479091ms | 9.528035ms |
| Spikes `>= 5ms` | 1 | 80 |
| GC cycles | 1 | 39 |

Interpretation:

- Typical latency (`p50`, `p95`) is similar between scenarios.
- Tail latency (`p99`, `p99.9`, `max`) worsens substantially under forced GC.
- Spike incidence increases sharply and lines up with GC activity.

### Full output

```text
=== GC Latency Spike Simulator (Go) ===
Config: cache=500000, duration=10s, reqRate=2000/s, alloc/request=8192B
Runtime: GOMAXPROCS=1, gcPercent=100

Building live cache with 500000 entries...
Cache built in 53.24396ms
[After cache build] HeapAlloc=141MB HeapObjects=500296 NumGC=3 NextGC=270MB

Running scenario: Baseline (forced GC every 0s)
Running scenario: Forced-GC (forced GC every 250ms)

--- Baseline ---
Runtime: 10.000618065s, requests: 20001
Latency  mean=584.743µs  p50=580.36µs  p95=1.120489ms  p99=1.214015ms  p99.9=1.290354ms  max=5.479091ms
Spikes >= 5ms: 1 (0.00%)
GC cycles during scenario: 1
Forced GC events: 0
Top slow requests:
  finished=17:09:16.265 latency=5.479091ms
  finished=17:09:16.265 latency=4.980985ms
  finished=17:09:16.265 latency=4.481939ms
  finished=17:09:16.265 latency=3.982703ms
  finished=17:09:16.265 latency=3.483564ms
Spike rate over time:
  t+0s       spikes=0/2000 (0.00%)
  t+1s       spikes=0/2000 (0.00%)
  t+2s       spikes=0/2000 (0.00%)
  t+3s       spikes=0/2000 (0.00%)
  t+4s       spikes=0/2000 (0.00%)
  t+5s       spikes=1/2000 (0.05%)
  t+6s       spikes=0/2000 (0.00%)
  t+7s       spikes=0/2000 (0.00%)
  t+8s       spikes=0/2000 (0.00%)
  t+9s       spikes=0/2000 (0.00%)
  t+10s      spikes=0/1 (0.00%)

--- Forced-GC ---
Runtime: 10.000241577s, requests: 20001
Latency  mean=634.426µs  p50=583.893µs  p95=1.150591ms  p99=3.466119ms  p99.9=6.272ms  max=9.528035ms
Spikes >= 5ms: 80 (0.40%)
GC cycles during scenario: 39
Forced GC events: 39
  GC#5 at 17:09:20.871 total=10.160214ms stw=31.777µs heap=147MB
  GC#6 at 17:09:21.121 total=7.840783ms stw=28.458µs heap=147MB
  GC#7 at 17:09:21.372 total=7.603376ms stw=16.52µs heap=147MB
  GC#8 at 17:09:21.621 total=7.071487ms stw=15.804µs heap=147MB
  GC#9 at 17:09:21.871 total=10.64491ms stw=24.063µs heap=147MB
Spikes near forced GC (within 25ms): 80
Top slow requests:
  finished=17:09:22.631 latency=9.528035ms
  finished=17:09:22.631 latency=9.030085ms
  finished=17:09:22.631 latency=8.531635ms
  finished=17:09:24.380 latency=8.266399ms
  finished=17:09:21.880 latency=8.266381ms
Spike rate over time:
  t+0s       spikes=6/2000 (0.30%)
  t+1s       spikes=10/2000 (0.50%)
  t+2s       spikes=13/2000 (0.65%)
  t+3s       spikes=13/2000 (0.65%)
  t+4s       spikes=4/2000 (0.20%)
  t+5s       spikes=7/2000 (0.35%)
  t+6s       spikes=8/2000 (0.40%)
  t+7s       spikes=4/2000 (0.20%)
  t+8s       spikes=7/2000 (0.35%)
  t+9s       spikes=8/2000 (0.40%)
  t+10s      spikes=0/1 (0.00%)

=== Comparison ===
p99: Baseline=1.214015ms  vs  Forced-GC=3.466119ms
p99.9: Baseline=1.290354ms  vs  Forced-GC=6.272ms
max: Baseline=5.479091ms  vs  Forced-GC=9.528035ms
spikes: Baseline=1  vs  Forced-GC=80
Tip: run with GODEBUG=gctrace=1 to see runtime-level GC traces alongside these app-level latencies.
```

## Notes

This is a toy model, but it captures a practical pattern seen in production systems:

- Throughput and median latency can look healthy.
- Tail latency and spike frequency can still violate user-facing or trading SLOs.
