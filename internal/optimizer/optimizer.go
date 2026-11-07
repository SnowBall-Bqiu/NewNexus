package optimizer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"io"
	"net/netip"
	"sort"
	"strings"
	"time"
)

func Expand(input string, limit int) ([]netip.Addr, error) {
	if limit <= 0 || limit > 100000 {
		limit = 100000
	}
	seen := map[netip.Addr]bool{}
	out := []netip.Addr{}
	add := func(ip netip.Addr) {
		ip = ip.Unmap()
		if security.PublicAddress(ip) && !seen[ip] && len(out) < limit {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	entries := []netip.Prefix{}
	for _, token := range strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' ' }) {
		token = strings.Trim(token, "\"[]")
		if token == "" {
			continue
		}
		if ip, e := netip.ParseAddr(token); e == nil {
			if !security.PublicAddress(ip) {
				return nil, fmt.Errorf("address %s is not public", ip)
			}
			entries = append(entries, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		p, e := netip.ParsePrefix(token)
		if e != nil {
			return nil, fmt.Errorf("invalid IP/CIDR %q", token)
		}
		p = p.Masked()
		if !security.PublicAddress(p.Addr()) {
			return nil, fmt.Errorf("network %s is not public", p)
		}
		entries = append(entries, p)
	}
	quota := limit / max(1, len(entries))
	if quota < 1 {
		quota = 1
	}
	for _, p := range entries {
		if p.Addr().Is4() {
			for ip, scanned := p.Addr(), 0; p.Contains(ip) && len(out) < limit && scanned < quota; ip, scanned = ip.Next(), scanned+1 {
				add(ip)
			}
		} else {
			count := 64
			if p.Bits() > 122 {
				count = 1 << (128 - p.Bits())
			}
			base := p.Addr().As16()
			for i := 0; i < count && len(out) < limit && i < quota; i++ {
				hash := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", p, i)))
				candidate := base
				for bit := p.Bits(); bit < 128; bit++ {
					mask := byte(1 << uint(7-bit%8))
					candidate[bit/8] = (candidate[bit/8] &^ mask) | (hash[bit/8] & mask)
				}
				if p.Bits() >= 122 {
					binary.BigEndian.PutUint64(candidate[8:], binary.BigEndian.Uint64(base[8:])|uint64(i))
				}
				add(netip.AddrFrom16(candidate))
			}
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
func Import(ctx context.Context, s *store.Store, source domain.Doc) (int, error) {
	kind := domain.S(source, "type")
	input := domain.S(source, "location")
	if kind == "cloudflare" {
		input = ""
		for _, endpoint := range []string{"https://www.cloudflare.com/ips-v4", "https://www.cloudflare.com/ips-v6"} {
			data, e := download(ctx, endpoint)
			if e != nil {
				return 0, e
			}
			input += "\n" + data
		}
	} else if kind == "url" {
		data, e := download(ctx, input)
		if e != nil {
			return 0, e
		}
		input = data
	} else if kind == "dns" {
		ips, e := security.ResolvePublic(ctx, input)
		if e != nil {
			return 0, e
		}
		parts := []string{}
		for _, ip := range ips {
			parts = append(parts, ip.String())
		}
		input = strings.Join(parts, "\n")
	}
	if kind == "json" || strings.HasPrefix(strings.TrimSpace(input), "[") {
		var entries []any
		if e := json.Unmarshal([]byte(input), &entries); e != nil {
			return 0, e
		}
		parts := []string{}
		for _, entry := range entries {
			switch v := entry.(type) {
			case string:
				parts = append(parts, v)
			case map[string]any:
				if a, ok := v["address"].(string); ok {
					parts = append(parts, a)
				}
			}
		}
		input = strings.Join(parts, "\n")
	}
	if kind == "csv" {
		r := csv.NewReader(strings.NewReader(input))
		r.FieldsPerRecord = -1
		parts := []string{}
		for {
			row, e := r.Read()
			if e == io.EOF {
				break
			}
			if e != nil {
				return 0, e
			}
			if len(row) > 0 {
				v := strings.TrimSpace(row[0])
				if strings.EqualFold(v, "ip") || strings.EqualFold(v, "address") {
					continue
				}
				parts = append(parts, v)
			}
		}
		input = strings.Join(parts, "\n")
	}
	addresses, e := Expand(input, 100000)
	if e != nil {
		return 0, e
	}
	existing, e := s.List(ctx, "candidates")
	if e != nil {
		return 0, e
	}
	byAddress := map[string]bool{}
	for _, d := range existing {
		if domain.S(d, "project_id") == domain.S(source, "project_id") {
			byAddress[domain.S(d, "address")] = true
		}
	}
	docs := []domain.Doc{}
	for _, ip := range addresses {
		if byAddress[ip.String()] {
			continue
		}
		family := 6
		if ip.Is4() {
			family = 4
		}
		d := domain.Doc{"address": ip.String(), "project_id": source["project_id"], "source_id": source["id"], "family": family, "tier": "inventory", "healthy": false, "score": 0, "sample_count": 0, "success_rate": 0}
		docs = append(docs, d)
	}
	if e = s.PutMany(ctx, "candidates", docs); e != nil {
		return 0, e
	}
	source["candidate_count"] = len(addresses)
	source["status"] = "synced"
	source["last_synced_at"] = domain.Now()
	return len(docs), s.Put(ctx, "sources", source)
}
func download(ctx context.Context, endpoint string) (string, error) {
	if u, e := security.ValidateURL(endpoint); e != nil {
		return "", e
	} else if u.Scheme != "https" {
		return "", errors.New("subscriptions must use HTTPS")
	}
	req, e := httpRequest(ctx, endpoint)
	if e != nil {
		return "", e
	}
	resp, e := security.SafeClient(30 * time.Second).Do(req)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("subscription returned HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if len(b) > 8<<20 {
		return "", errors.New("subscription exceeds 8 MiB")
	}
	return string(b), e
}

type Sample struct {
	Count                    int
	Rate                     float64
	NodeID                   string
	Success                  bool
	TCP, TLS, TTFB, Download float64
	At                       time.Time
}
type Summary struct {
	Healthy                                                bool
	Score, SuccessRate, TCP, TLS, TTFB, P50, P95, Download float64
	Count                                                  int
	Last                                                   time.Time
}

func Score(samples []Sample, now time.Time, minSamples int, freshness time.Duration) Summary {
	out := Summary{}
	latencies := []float64{}
	success := 0.
	for _, v := range samples {
		if now.Sub(v.At) > freshness || v.At.After(now.Add(2*time.Minute)) {
			continue
		}
		count := v.Count
		if count < 1 {
			count = 1
		}
		rate := v.Rate
		if v.Success && rate == 0 {
			rate = 1
		}
		out.Count += count
		if v.At.After(out.Last) {
			out.Last = v.At
		}
		if rate > 0 {
			weight := float64(count) * rate
			success += weight
			out.TCP += v.TCP * weight
			out.TLS += v.TLS * weight
			out.TTFB += v.TTFB * weight
			out.Download += v.Download * weight
			for i := 0; i < count; i++ {
				latencies = append(latencies, v.TTFB+v.TCP+v.TLS)
			}
		}
	}
	if out.Count == 0 {
		return out
	}
	out.SuccessRate = float64(success) / float64(out.Count)
	if success > 0 {
		out.TCP /= float64(success)
		out.TLS /= float64(success)
		out.TTFB /= float64(success)
		out.Download /= float64(success)
		sort.Float64s(latencies)
		out.P50 = latencies[(len(latencies)-1)/2]
		out.P95 = latencies[int(float64(len(latencies)-1)*.95)]
	}
	out.Healthy = out.Count >= minSamples && out.SuccessRate >= .8 && now.Sub(out.Last) <= freshness
	if out.Healthy {
		latency := out.TCP*.2 + out.TLS*.15 + out.TTFB*.25 + out.P50*.15 + out.P95*.25
		out.Score = 100*out.SuccessRate/(1+latency/100) + min(out.Download/1000000, 20)
	}
	return out
}

type scoreConfig struct {
	freshness time.Duration
	minimum   int
	rate      float64
	weights   domain.Doc
}

func loadConfig(ctx context.Context, s *store.Store) (scoreConfig, error) {
	c := scoreConfig{15 * time.Minute, 3, .8, domain.Doc{"tcp": .2, "tls": .15, "ttfb": .25, "p50": .15, "p95": .25, "download": .2, "failure": 1}}
	settings, e := s.List(ctx, "settings")
	if e != nil {
		return c, e
	}
	for _, d := range settings {
		switch domain.S(d, "key") {
		case "probe.freshness_seconds":
			if domain.N(d, "value") > 0 {
				c.freshness = time.Duration(domain.N(d, "value")) * time.Second
			}
		case "probe.min_samples":
			if domain.N(d, "value") > 0 {
				c.minimum = int(domain.N(d, "value"))
			}
		case "probe.minimum_success_rate":
			c.rate = domain.N(d, "value")
		case "score.weights":
			for k, v := range domain.M(d, "value") {
				c.weights[k] = v
			}
		}
	}
	return c, nil
}
func (c scoreConfig) score(samples []Sample, now time.Time) Summary {
	result := Score(samples, now, c.minimum, c.freshness)
	result.Healthy = result.Count >= c.minimum && result.SuccessRate >= c.rate && !result.Last.IsZero() && now.Sub(result.Last) <= c.freshness
	if result.Healthy {
		latency := result.TCP*domain.N(c.weights, "tcp") + result.TLS*domain.N(c.weights, "tls") + result.TTFB*domain.N(c.weights, "ttfb") + result.P50*domain.N(c.weights, "p50") + result.P95*domain.N(c.weights, "p95")
		result.Score = max(0, 100*result.SuccessRate/(1+latency/100)+min(result.Download/1000000, 100)*domain.N(c.weights, "download")-(1-result.SuccessRate)*100*domain.N(c.weights, "failure"))
	} else {
		result.Score = 0
	}
	return result
}
func Aggregate(ctx context.Context, s *store.Store, candidate string) (Summary, error) {
	config, e := loadConfig(ctx, s)
	if e != nil {
		return Summary{}, e
	}
	rows, e := s.DB.QueryContext(ctx, s.Q(`SELECT node_id,success,tcp_ms,tls_ms,ttfb_ms,download_bps,observed_at,sample_count,success_rate FROM metrics WHERE candidate_id=? AND observed_at>=? ORDER BY observed_at DESC LIMIT 1000`), candidate, s.Time(time.Now().Add(-config.freshness)))
	if e != nil {
		return Summary{}, e
	}
	samples := []Sample{}
	for rows.Next() {
		var n int
		var at string
		v := Sample{}
		if e = rows.Scan(&v.NodeID, &n, &v.TCP, &v.TLS, &v.TTFB, &v.Download, &at, &v.Count, &v.Rate); e != nil {
			rows.Close()
			return Summary{}, e
		}
		v.Success = n == 1
		v.At, _ = time.Parse(time.RFC3339Nano, at)
		samples = append(samples, v)
	}
	iterationError := rows.Err()
	rows.Close()
	if iterationError != nil {
		return Summary{}, iterationError
	}
	return config.score(samples, time.Now()), nil
}
func Refresh(ctx context.Context, s *store.Store, id string) error {
	candidate, e := s.Get(ctx, "candidates", id)
	if e != nil {
		return e
	}
	sum, e := Aggregate(ctx, s, id)
	if e != nil {
		return e
	}
	candidate["healthy"] = sum.Healthy
	candidate["score"] = sum.Score
	candidate["success_rate"] = sum.SuccessRate
	candidate["tcp_ms"] = sum.TCP
	candidate["tls_ms"] = sum.TLS
	candidate["ttfb_ms"] = sum.TTFB
	candidate["p50_ms"] = sum.P50
	candidate["p95_ms"] = sum.P95
	candidate["download_bps"] = sum.Download
	candidate["sample_count"] = sum.Count
	candidate["last_probed_at"] = sum.Last.UTC().Format(time.RFC3339Nano)
	if sum.Healthy && domain.S(candidate, "tier") == "inventory" {
		candidate["tier"] = "ranked"
	}
	if !sum.Healthy && domain.S(candidate, "tier") == "ranked" {
		candidate["tier"] = "inventory"
	}
	return s.Put(ctx, "candidates", candidate)
}
