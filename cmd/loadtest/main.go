// Command loadtest drives a concurrent BET/WIN/REFUND mix against the
// three-instance compose stack (docker-compose.multi.yml) and reports
// throughput, latency percentiles, status and error counts, the
// concurrency-conflict and outbox metrics of each instance and a
// reconciliation of every wallet. Method and results: docs/CARGA.md.
//
// It is an operator tool, not part of the service: it only talks to the
// public HTTP API, Keycloak and the Prometheus endpoints.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	apps, metrics      []string
	tokenURL, provider string
	providerSecret     string
	internalID         string
	internalSecret     string
	wallets, hot       int
	workers            int
	duration           time.Duration
	hotPercent         int
	balanceCents       int64
	hotBalanceCents    int64
	drainTimeout       time.Duration
}

func main() {
	var c config
	apps := flag.String("apps", "http://localhost:8081,http://localhost:8082,http://localhost:8083", "comma-separated app base URLs")
	metrics := flag.String("metrics", "http://localhost:9090,http://localhost:9091,http://localhost:9092", "comma-separated metrics base URLs")
	flag.StringVar(&c.tokenURL, "token-url", "http://localhost:8080/realms/wagering/protocol/openid-connect/token", "Keycloak token endpoint")
	flag.StringVar(&c.provider, "provider", "provider-a", "provider client id")
	flag.StringVar(&c.providerSecret, "provider-secret", "provider-a-local-secret", "provider client secret")
	flag.StringVar(&c.internalID, "internal", "wallet-internal", "internal client id")
	flag.StringVar(&c.internalSecret, "internal-secret", "wallet-internal-local-secret", "internal client secret")
	flag.IntVar(&c.wallets, "wallets", 200, "number of wallets")
	flag.IntVar(&c.hot, "hot", 4, "number of shared (hot) wallets among them")
	flag.IntVar(&c.hotPercent, "hot-percent", 30, "percentage of operations aimed at hot wallets")
	flag.IntVar(&c.workers, "workers", 48, "concurrent workers")
	flag.DurationVar(&c.duration, "duration", 60*time.Second, "load duration")
	flag.Int64Var(&c.balanceCents, "balance-cents", 100000, "initial balance of regular wallets, in cents")
	flag.Int64Var(&c.hotBalanceCents, "hot-balance-cents", 5000, "initial balance of hot wallets, in cents (low, to provoke INSUFFICIENT_FUNDS)")
	flag.DurationVar(&c.drainTimeout, "drain-timeout", 10*time.Minute, "how long to wait for the outbox to drain after the load")
	flag.Parse()
	c.apps = strings.Split(*apps, ",")
	c.metrics = strings.Split(*metrics, ",")
	if c.hot > c.wallets || c.wallets < 1 || c.workers < 1 {
		fmt.Fprintln(os.Stderr, "invalid -wallets/-hot/-workers")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.duration+c.drainTimeout+5*time.Minute)
	defer cancel()
	if err := run(ctx, c); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

type wallet struct{ id, player string }

type sample struct {
	op     string
	status int
	code   string
	dur    time.Duration
}

func run(ctx context.Context, c config) error {
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: c.workers}}
	internal, err := token(ctx, client, c.tokenURL, c.internalID, c.internalSecret)
	if err != nil {
		return err
	}
	provider, err := token(ctx, client, c.tokenURL, c.provider, c.providerSecret)
	if err != nil {
		return err
	}

	fmt.Printf("opening %d wallets (%d hot)\n", c.wallets, c.hot)
	ws := make([]wallet, c.wallets)
	for i := range ws {
		bal := c.balanceCents
		if i < c.hot {
			bal = c.hotBalanceCents
		}
		player := uuid()
		body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, player, cents(bal))
		st, resp, err := post(ctx, client, c.apps[i%len(c.apps)]+"/wallets", internal, "", body)
		if err != nil || st != http.StatusCreated {
			return fmt.Errorf("open wallet: status %d: %v %s", st, err, resp)
		}
		var w struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(resp, &w); err != nil {
			return fmt.Errorf("open wallet: %w", err)
		}
		ws[i] = wallet{id: w.ID, player: player}
	}

	before := scrapeAll(ctx, client, c.metrics)
	var maxLag atomic.Int64 // milliseconds
	stopSampling := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-t.C:
				m := scrapeAll(ctx, client, c.metrics)
				if ms := int64(m.maxLag * 1000); ms > maxLag.Load() {
					maxLag.Store(ms)
				}
			}
		}
	}()

	fmt.Printf("running %d workers for %s across %d instances\n", c.workers, c.duration, len(c.apps))
	loadCtx, stop := context.WithTimeout(ctx, c.duration)
	defer stop()
	var mu sync.Mutex
	var samples []sample
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < c.workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var local []sample
			for seq := 0; loadCtx.Err() == nil; seq++ {
				target := ws[c.hot+randn(c.wallets-c.hot)]
				if c.hot > 0 && (c.hot == c.wallets || randn(100) < c.hotPercent) {
					target = ws[randn(c.hot)]
				}
				local = append(local, round(loadCtx, client, c, provider, target, w, seq)...)
			}
			mu.Lock()
			samples = append(samples, local...)
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stopSampling)
	<-samplerDone

	atEnd := scrapeAll(ctx, client, c.metrics)
	// Wait for the outbox to drain and time it.
	drainStart := time.Now()
	after := atEnd
	for after.unpublished > 0 && time.Since(drainStart) < c.drainTimeout {
		time.Sleep(time.Second)
		after = scrapeAll(ctx, client, c.metrics)
	}

	report(samples, elapsed, before, atEnd, time.Duration(maxLag.Load())*time.Millisecond)
	fmt.Printf("  outbox drained after the load: unpublished=%.0f after %s\n", after.unpublished, time.Since(drainStart).Round(time.Second))

	fmt.Println("\nreconciliation")
	// The drain can outlast the first token.
	if internal, err = token(ctx, client, c.tokenURL, c.internalID, c.internalSecret); err != nil {
		return err
	}
	inconsistent := 0
	for i, w := range ws {
		st, resp, err := post(ctx, client, c.apps[i%len(c.apps)]+"/wallets/"+w.id+"/reconciliation", internal, "", "")
		var r struct {
			Consistent bool `json:"consistent"`
		}
		if err != nil || st != http.StatusOK || json.Unmarshal(resp, &r) != nil || !r.Consistent {
			inconsistent++
			fmt.Printf("  wallet %s: status %d consistent=false %v %s\n", w.id, st, err, resp)
		}
	}
	fmt.Printf("  wallets=%d consistent=%d inconsistent=%d\n", len(ws), len(ws)-inconsistent, inconsistent)
	if inconsistent > 0 {
		return fmt.Errorf("%d wallets inconsistent", inconsistent)
	}
	return nil
}

// round sends one BET and, depending on a die roll, a WIN or REFUND that
// references it, an identical replay (200, idempotentReplay) or a reuse of
// the key with a different payload (409 IDEMPOTENCY_CONFLICT).
func round(ctx context.Context, client *http.Client, c config, tok string, w wallet, worker, seq int) []sample {
	prefix := fmt.Sprintf("lt-%d-%d-%s", worker, seq, uuid()[:8])
	roundID := prefix + "-r"
	app := c.apps[randn(len(c.apps))]
	amount := 100 + int64(randn(1900)) // 1.00 .. 19.99
	betID := prefix + "-bet"
	bet := txBody(c.provider, betID, w, roundID, "BET", amount, "")
	out := []sample{send(ctx, client, app, tok, c.provider, betID, bet, "BET")}
	if out[0].status != http.StatusOK {
		return out
	}
	app = c.apps[randn(len(c.apps))]
	switch d := randn(100); {
	case d < 40:
		id := prefix + "-win"
		out = append(out, send(ctx, client, app, tok, c.provider, id, txBody(c.provider, id, w, roundID, "WIN", amount*2, betID), "WIN"))
	case d < 60:
		id := prefix + "-ref"
		out = append(out, send(ctx, client, app, tok, c.provider, id, txBody(c.provider, id, w, roundID, "REFUND", amount, betID), "REFUND"))
	case d < 65:
		out = append(out, send(ctx, client, app, tok, c.provider, betID, bet, "BET-replay"))
	case d < 70:
		out = append(out, send(ctx, client, app, tok, c.provider, betID, txBody(c.provider, betID, w, roundID, "BET", amount+1, ""), "BET-conflict"))
	}
	return out
}

func txBody(provider, ext string, w wallet, roundID, kind string, amount int64, ref string) string {
	refField := ""
	if ref != "" {
		refField = fmt.Sprintf(`,"referenceExternalTransactionId":%q`, ref)
	}
	return fmt.Sprintf(`{"providerId":%q,"externalTransactionId":%q,"playerId":%q,"walletId":%q,"roundId":%q,"gameId":"load-test","kind":%q,"money":{"amount":%q,"currency":"BRL"}%s}`,
		provider, ext, w.player, w.id, roundID, kind, cents(amount), refField)
}

func send(ctx context.Context, client *http.Client, app, tok, provider, ext, body, op string) sample {
	t0 := time.Now()
	st, resp, err := post(ctx, client, app+"/wagering/transactions", tok, provider+":"+ext, body)
	s := sample{op: op, status: st, dur: time.Since(t0)}
	if err != nil {
		s.code = "TRANSPORT_ERROR"
		if ctx.Err() != nil {
			// In flight when the duration ran out; it may or may not have
			// committed, which the reconciliation covers.
			s.code = "CANCELLED_AT_END"
		}
		return s
	}
	var r struct {
		FailureCode string `json:"failureCode"`
		Error       struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(resp, &r)
	s.code = r.FailureCode
	if s.code == "" {
		s.code = r.Error.Code
	}
	return s
}

func post(ctx context.Context, client *http.Client, u, tok, idemKey, body string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func token(ctx context.Context, client *http.Client, tokenURL, id, secret string) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token %s: %w", id, err)
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil || t.AccessToken == "" {
		return "", fmt.Errorf("token %s: status %d: %v", id, resp.StatusCode, err)
	}
	return t.AccessToken, nil
}

type metricsSnapshot struct {
	conflicts   map[string]float64 // db_concurrency_conflicts_total by sqlstate, summed over instances
	retries     float64
	maxLag      float64
	unpublished float64
}

func scrapeAll(ctx context.Context, client *http.Client, bases []string) metricsSnapshot {
	m := metricsSnapshot{conflicts: map[string]float64{}}
	for _, b := range bases {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, b+"/metrics", nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "#") {
				continue
			}
			i := strings.LastIndexByte(line, ' ')
			if i < 0 {
				continue
			}
			name, v := line[:i], line[i+1:]
			val, err := strconv.ParseFloat(v, 64) // metric samples only, never money
			if err != nil {
				continue
			}
			switch {
			case strings.HasPrefix(name, "db_concurrency_conflicts_total"):
				m.conflicts[label(name, "sqlstate")] += val
			case strings.HasPrefix(name, "retries_total"):
				m.retries += val
			case name == "outbox_lag_seconds":
				if val > m.maxLag {
					m.maxLag = val
				}
			case name == "outbox_unpublished":
				// Every instance reports the same shared table: take the max.
				if val > m.unpublished {
					m.unpublished = val
				}
			}
		}
	}
	return m
}

func label(series, key string) string {
	i := strings.Index(series, key+`="`)
	if i < 0 {
		return "-"
	}
	rest := series[i+len(key)+2:]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return rest
}

func report(samples []sample, elapsed time.Duration, before, after metricsSnapshot, maxLag time.Duration) {
	byOp := map[string][]time.Duration{}
	counts := map[string]int{}
	var all []time.Duration
	for _, s := range samples {
		counts[fmt.Sprintf("%-12s %d %s", s.op, s.status, s.code)]++
		if s.code == "CANCELLED_AT_END" {
			continue
		}
		byOp[s.op] = append(byOp[s.op], s.dur)
		all = append(all, s.dur)
	}
	fmt.Printf("\ncompleted requests=%d elapsed=%s throughput=%.1f req/s\n", len(all), elapsed.Round(time.Millisecond), float64(len(all))/elapsed.Seconds())
	fmt.Println("\nlatency in ms (excluding CANCELLED_AT_END)       n     p50     p95     p99     max")
	printLat("ALL", all)
	ops := make([]string, 0, len(byOp))
	for op := range byOp {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		printLat(op, byOp[op])
	}

	fmt.Println("\nstatus counts (operation, HTTP status, failureCode or error.code)")
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-50s %d\n", k, counts[k])
	}

	fmt.Println("\nmetrics at the end of the load (counters: sum of the three instances, delta over the run)")
	states := map[string]bool{}
	for k := range after.conflicts {
		states[k] = true
	}
	if len(states) == 0 {
		fmt.Println("  db_concurrency_conflicts_total: 0")
	}
	for k := range states {
		fmt.Printf("  db_concurrency_conflicts_total{sqlstate=%q}: %.0f\n", k, after.conflicts[k]-before.conflicts[k])
	}
	fmt.Printf("  retries_total: %.0f\n", after.retries-before.retries)
	fmt.Printf("  outbox_lag_seconds: max sampled during run %.3fs, at end %.3fs\n", maxLag.Seconds(), after.maxLag)
	fmt.Printf("  outbox_unpublished at end: %.0f\n", after.unpublished)
}

func printLat(name string, d []time.Duration) {
	if len(d) == 0 {
		return
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	p := func(q int) string { return ms(d[(len(d)-1)*q/100]) }
	fmt.Printf("  %-38s %6d %7s %7s %7s %7s\n", name, len(d), p(50), p(95), p(99), ms(d[len(d)-1]))
}

func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 1, 64)
}

// cents formats integer minor units as a decimal string with scale 2.
func cents(v int64) string { return fmt.Sprintf("%d.%02d", v/100, v%100) }

func randn(n int) int { return mrand.IntN(n) }

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails since Go 1.24
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
