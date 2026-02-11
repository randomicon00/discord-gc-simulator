package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

type ReadState struct {
	UserID    uint64
	ChannelID uint64
	MessageID uint64
	Timestamp int64
	Data      [256]byte
}

type Config struct {
	CacheEntries        int
	Duration            time.Duration
	RequestRate         int
	TempAllocBytes      int
	BaselineForcedGC    time.Duration
	StressedForcedGC    time.Duration
	SpikeThreshold      time.Duration
	SpikeWindow         time.Duration
	RequestReplaceEvery int
	GOMAXPROCS          int
	GCPercent           int
}

type RequestSample struct {
	Scheduled time.Time
	Finished  time.Time
	Latency   time.Duration
}

type GCEvent struct {
	At        time.Time
	Total     time.Duration
	STWPause  time.Duration
	HeapAlloc uint64
	NumGC     uint32
}

type ScenarioResult struct {
	Name      string
	Samples   []RequestSample
	GCEvents  []GCEvent
	NumGCFrom uint32
	NumGCTo   uint32
	Runtime   time.Duration
}

type LatencySummary struct {
	Count  int
	P50    time.Duration
	P95    time.Duration
	P99    time.Duration
	P999   time.Duration
	Max    time.Duration
	Mean   time.Duration
	Spikes int
}

type SpikeBucket struct {
	Offset   time.Duration
	Requests int
	Spikes   int
}

func main() {
	cfg := Config{}
	flag.IntVar(&cfg.CacheEntries, "cache-entries", 500_000, "number of live cache entries")
	flag.DurationVar(&cfg.Duration, "duration", 8*time.Second, "duration per scenario")
	flag.IntVar(&cfg.RequestRate, "request-rate", 2000, "scheduled requests per second")
	flag.IntVar(&cfg.TempAllocBytes, "temp-alloc-bytes", 8*1024, "temporary bytes allocated per request")
	flag.DurationVar(&cfg.BaselineForcedGC, "baseline-forced-gc", 0, "forced GC interval in baseline scenario (0 disables)")
	flag.DurationVar(&cfg.StressedForcedGC, "stressed-forced-gc", 250*time.Millisecond, "forced GC interval in stressed scenario")
	flag.DurationVar(&cfg.SpikeThreshold, "spike-threshold", 5*time.Millisecond, "latency threshold for spike counting")
	flag.DurationVar(&cfg.SpikeWindow, "spike-window", 1*time.Second, "window used to report spike rate over time")
	flag.IntVar(&cfg.RequestReplaceEvery, "replace-every", 200, "replace one cache entry every N requests to keep churn")
	flag.IntVar(&cfg.GOMAXPROCS, "gomaxprocs", 1, "GOMAXPROCS used by simulator")
	flag.IntVar(&cfg.GCPercent, "gc-percent", 100, "value passed to debug.SetGCPercent")
	flag.Parse()

	if err := validateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(1)
	}

	runtime.GOMAXPROCS(cfg.GOMAXPROCS)
	debug.SetGCPercent(cfg.GCPercent)

	fmt.Println("=== GC Latency Spike Simulator (Go) ===")
	fmt.Printf("Config: cache=%d, duration=%v, reqRate=%d/s, alloc/request=%dB\n",
		cfg.CacheEntries, cfg.Duration, cfg.RequestRate, cfg.TempAllocBytes)
	fmt.Printf("Runtime: GOMAXPROCS=%d, gcPercent=%d\n", cfg.GOMAXPROCS, cfg.GCPercent)
	fmt.Println()

	cache := buildLiveCache(cfg.CacheEntries)
	printMem("After cache build")
	fmt.Println()

	baseline := runScenario("Baseline", cfg, cache, cfg.BaselineForcedGC)
	stressed := runScenario("Forced-GC", cfg, cache, cfg.StressedForcedGC)

	printScenarioReport(baseline, cfg.SpikeThreshold, cfg.SpikeWindow)
	printScenarioReport(stressed, cfg.SpikeThreshold, cfg.SpikeWindow)
	printComparison(baseline, stressed, cfg.SpikeThreshold)

	fmt.Println("Tip: run with GODEBUG=gctrace=1 to see runtime-level GC traces alongside these app-level latencies.")
}

func buildLiveCache(entries int) []*ReadState {
	fmt.Printf("Building live cache with %d entries...\n", entries)
	start := time.Now()
	cache := make([]*ReadState, entries)
	for i := 0; i < entries; i++ {
		cache[i] = &ReadState{
			UserID:    uint64(i),
			ChannelID: uint64(i * 10),
			MessageID: uint64(i * 100),
			Timestamp: time.Now().UnixNano(),
		}
	}
	fmt.Printf("Cache built in %v\n", time.Since(start))
	return cache
}

func runScenario(name string, cfg Config, cache []*ReadState, forcedGCInterval time.Duration) ScenarioResult {
	fmt.Printf("Running scenario: %s (forced GC every %v)\n", name, forcedGCInterval)

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	deadline := start.Add(cfg.Duration)
	period := time.Second / time.Duration(cfg.RequestRate)

	stopGC := make(chan struct{})
	var wg sync.WaitGroup
	var gcMu sync.Mutex
	gcEvents := make([]GCEvent, 0, 32)

	if forcedGCInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(forcedGCInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					gcStart := time.Now()
					runtime.GC()
					total := time.Since(gcStart)
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					idx := (m.NumGC + 255) % 256
					gcMu.Lock()
					gcEvents = append(gcEvents, GCEvent{
						At:        gcStart,
						Total:     total,
						STWPause:  time.Duration(m.PauseNs[idx]),
						HeapAlloc: m.HeapAlloc,
						NumGC:     m.NumGC,
					})
					gcMu.Unlock()
				case <-stopGC:
					return
				}
			}
		}()
	}

	samples := make([]RequestSample, 0, int(cfg.Duration.Seconds())*cfg.RequestRate)
	hold := make([][]byte, 512)
	requestID := 0
	for scheduled := start; ; scheduled = scheduled.Add(period) {
		if scheduled.After(deadline) {
			break
		}
		now := time.Now()
		if now.Before(scheduled) {
			time.Sleep(scheduled.Sub(now))
		}

		processRequest(cache, requestID, cfg, hold)
		requestID++
		finished := time.Now()
		samples = append(samples, RequestSample{
			Scheduled: scheduled,
			Finished:  finished,
			Latency:   finished.Sub(scheduled),
		})
	}

	close(stopGC)
	wg.Wait()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	gcMu.Lock()
	events := append([]GCEvent(nil), gcEvents...)
	gcMu.Unlock()

	return ScenarioResult{
		Name:      name,
		Samples:   samples,
		GCEvents:  events,
		NumGCFrom: before.NumGC,
		NumGCTo:   after.NumGC,
		Runtime:   time.Since(start),
	}
}

func processRequest(cache []*ReadState, requestID int, cfg Config, hold [][]byte) {
	idx := requestID % len(cache)
	rs := cache[idx]
	rs.Timestamp = time.Now().UnixNano()
	rs.MessageID++

	tmp := make([]byte, cfg.TempAllocBytes)
	for i := 0; i < len(tmp); i += 64 {
		tmp[i] = byte(rs.UserID)
	}
	hold[requestID%len(hold)] = tmp

	if cfg.RequestReplaceEvery > 0 && requestID%cfg.RequestReplaceEvery == 0 {
		cache[idx] = &ReadState{
			UserID:    rs.UserID,
			ChannelID: rs.ChannelID,
			MessageID: rs.MessageID,
			Timestamp: rs.Timestamp,
		}
	}
}

func summarize(samples []RequestSample, spikeThreshold time.Duration) LatencySummary {
	if len(samples) == 0 {
		return LatencySummary{}
	}

	sorted := append([]RequestSample(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Latency < sorted[j].Latency })

	var total time.Duration
	spikes := 0
	for _, s := range samples {
		total += s.Latency
		if s.Latency >= spikeThreshold {
			spikes++
		}
	}

	return LatencySummary{
		Count:  len(samples),
		P50:    samplePercentile(sorted, 50),
		P95:    samplePercentile(sorted, 95),
		P99:    samplePercentile(sorted, 99),
		P999:   samplePercentile(sorted, 99.9),
		Max:    sorted[len(sorted)-1].Latency,
		Mean:   total / time.Duration(len(samples)),
		Spikes: spikes,
	}
}

func samplePercentile(sorted []RequestSample, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0].Latency
	}
	if p >= 100 {
		return sorted[len(sorted)-1].Latency
	}

	rank := (p / 100.0) * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo].Latency
	}
	frac := rank - float64(lo)
	loNs := float64(sorted[lo].Latency.Nanoseconds())
	hiNs := float64(sorted[hi].Latency.Nanoseconds())
	return time.Duration(loNs + frac*(hiNs-loNs))
}

func spikesNearGC(samples []RequestSample, gcEvents []GCEvent, spikeThreshold, window time.Duration) int {
	if len(samples) == 0 || len(gcEvents) == 0 {
		return 0
	}

	count := 0
	for _, sample := range samples {
		if sample.Latency < spikeThreshold {
			continue
		}
		for _, event := range gcEvents {
			delta := sample.Finished.Sub(event.At)
			if delta < 0 {
				delta = -delta
			}
			if delta <= window {
				count++
				break
			}
		}
	}
	return count
}

func slowestSamples(samples []RequestSample, limit int) []RequestSample {
	if len(samples) == 0 || limit <= 0 {
		return nil
	}
	copySamples := append([]RequestSample(nil), samples...)
	sort.Slice(copySamples, func(i, j int) bool {
		return copySamples[i].Latency > copySamples[j].Latency
	})
	if len(copySamples) < limit {
		limit = len(copySamples)
	}
	return copySamples[:limit]
}

func spikeRateByWindow(samples []RequestSample, spikeThreshold, window time.Duration) []SpikeBucket {
	if len(samples) == 0 {
		return nil
	}
	if window <= 0 {
		window = time.Second
	}

	start := samples[0].Scheduled
	last := samples[len(samples)-1].Scheduled
	bucketCount := int(last.Sub(start)/window) + 1
	if bucketCount < 1 {
		bucketCount = 1
	}
	buckets := make([]SpikeBucket, bucketCount)
	for i := range buckets {
		buckets[i].Offset = time.Duration(i) * window
	}

	for _, sample := range samples {
		idx := int(sample.Scheduled.Sub(start) / window)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(buckets) {
			idx = len(buckets) - 1
		}
		buckets[idx].Requests++
		if sample.Latency >= spikeThreshold {
			buckets[idx].Spikes++
		}
	}
	return buckets
}

func printSpikeTimeline(buckets []SpikeBucket) {
	if len(buckets) == 0 {
		return
	}
	fmt.Println("Spike rate over time:")
	limit := len(buckets)
	tail := 0
	if len(buckets) > 40 {
		limit = 30
		tail = 5
	}
	for i := 0; i < limit; i++ {
		b := buckets[i]
		rate := 100 * float64(b.Spikes) / float64(maxInt(1, b.Requests))
		fmt.Printf("  t+%-8v spikes=%d/%d (%.2f%%)\n", b.Offset, b.Spikes, b.Requests, rate)
	}
	if tail > 0 {
		fmt.Println("  ...")
		for i := len(buckets) - tail; i < len(buckets); i++ {
			b := buckets[i]
			rate := 100 * float64(b.Spikes) / float64(maxInt(1, b.Requests))
			fmt.Printf("  t+%-8v spikes=%d/%d (%.2f%%)\n", b.Offset, b.Spikes, b.Requests, rate)
		}
	}
}

func printScenarioReport(result ScenarioResult, spikeThreshold, spikeWindow time.Duration) {
	s := summarize(result.Samples, spikeThreshold)
	fmt.Printf("\n--- %s ---\n", result.Name)
	fmt.Printf("Runtime: %v, requests: %d\n", result.Runtime, s.Count)
	fmt.Printf("Latency  mean=%v  p50=%v  p95=%v  p99=%v  p99.9=%v  max=%v\n", s.Mean, s.P50, s.P95, s.P99, s.P999, s.Max)
	fmt.Printf("Spikes >= %v: %d (%.2f%%)\n", spikeThreshold, s.Spikes, 100*float64(s.Spikes)/float64(maxInt(1, s.Count)))
	fmt.Printf("GC cycles during scenario: %d\n", result.NumGCTo-result.NumGCFrom)

	if len(result.GCEvents) == 0 {
		fmt.Println("Forced GC events: 0")
	} else {
		fmt.Printf("Forced GC events: %d\n", len(result.GCEvents))
		limit := minInt(5, len(result.GCEvents))
		for i := 0; i < limit; i++ {
			e := result.GCEvents[i]
			fmt.Printf("  GC#%d at %s total=%v stw=%v heap=%dMB\n",
				e.NumGC,
				e.At.Format("15:04:05.000"),
				e.Total,
				e.STWPause,
				e.HeapAlloc/1024/1024,
			)
		}
		near := spikesNearGC(result.Samples, result.GCEvents, spikeThreshold, 25*time.Millisecond)
		fmt.Printf("Spikes near forced GC (within 25ms): %d\n", near)
	}

	slowest := slowestSamples(result.Samples, 5)
	if len(slowest) > 0 {
		fmt.Println("Top slow requests:")
		for _, req := range slowest {
			fmt.Printf("  finished=%s latency=%v\n", req.Finished.Format("15:04:05.000"), req.Latency)
		}
	}
	printSpikeTimeline(spikeRateByWindow(result.Samples, spikeThreshold, spikeWindow))
}

func printComparison(a, b ScenarioResult, spikeThreshold time.Duration) {
	sa := summarize(a.Samples, spikeThreshold)
	sb := summarize(b.Samples, spikeThreshold)

	fmt.Println("\n=== Comparison ===")
	fmt.Printf("p99: %s=%v  vs  %s=%v\n", a.Name, sa.P99, b.Name, sb.P99)
	fmt.Printf("p99.9: %s=%v  vs  %s=%v\n", a.Name, sa.P999, b.Name, sb.P999)
	fmt.Printf("max: %s=%v  vs  %s=%v\n", a.Name, sa.Max, b.Name, sb.Max)
	fmt.Printf("spikes: %s=%d  vs  %s=%d\n", a.Name, sa.Spikes, b.Name, sb.Spikes)
}

func printMem(label string) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("[%s] HeapAlloc=%dMB HeapObjects=%d NumGC=%d NextGC=%dMB\n",
		label,
		m.HeapAlloc/1024/1024,
		m.HeapObjects,
		m.NumGC,
		m.NextGC/1024/1024,
	)
}

func validateConfig(cfg Config) error {
	if cfg.CacheEntries <= 0 {
		return fmt.Errorf("cache-entries must be > 0")
	}
	if cfg.Duration <= 0 {
		return fmt.Errorf("duration must be > 0")
	}
	if cfg.RequestRate <= 0 {
		return fmt.Errorf("request-rate must be > 0")
	}
	if cfg.TempAllocBytes <= 0 {
		return fmt.Errorf("temp-alloc-bytes must be > 0")
	}
	if cfg.SpikeWindow <= 0 {
		return fmt.Errorf("spike-window must be > 0")
	}
	if cfg.GOMAXPROCS <= 0 {
		return fmt.Errorf("gomaxprocs must be > 0")
	}
	if cfg.GCPercent == 0 || cfg.GCPercent < -1 {
		return fmt.Errorf("gc-percent must be -1 or > 0")
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
