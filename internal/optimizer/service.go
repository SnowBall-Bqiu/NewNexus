package optimizer

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/netguard"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	store  *store.Store
	broker *events.Broker
	client *http.Client
}

func New(store *store.Store, broker *events.Broker) *Service {
	return &Service{store: store, broker: broker, client: netguard.NewHTTPClient(20*time.Second, 3, 16)}
}

type SourceConfig struct {
	Content         string `json:"content"`
	Format          string `json:"format"`
	Hostname        string `json:"hostname"`
	URL             string `json:"url"`
	SamplePerPrefix int    `json:"samplePerPrefix"`
}

func (s *Service) SyncSource(ctx context.Context, sourceID string, samplePerPrefix int) (count int, err error) {
	var source domain.CandidateSource
	if err := s.store.DB.First(&source, "id = ?", sourceID).Error; err != nil {
		return 0, err
	}
	if !source.Enabled {
		return 0, fmt.Errorf("candidate source is disabled")
	}
	source.Status = "syncing"
	_ = s.store.DB.Save(&source).Error
	defer func() {
		if err != nil {
			source.Status = "error"
			_ = s.store.DB.Save(&source).Error
		}
	}()
	var config SourceConfig
	_ = json.Unmarshal([]byte(source.ConfigJSON), &config)
	if samplePerPrefix <= 0 {
		samplePerPrefix = config.SamplePerPrefix
	}
	switch source.Type {
	case "cloudflare_official":
		return s.SyncCloudflare(ctx, sourceID, samplePerPrefix)
	case "manual", "cidr", "csv", "json":
		format := defaultFormat(source.Type, config.Format)
		return s.Import(ctx, source, strings.NewReader(config.Content), format)
	case "dns":
		hostname := defaultValue(config.Hostname, source.Location)
		return s.syncDNS(ctx, source, hostname)
	case "https_subscription":
		return s.syncSubscription(ctx, source, defaultValue(config.URL, source.Location), config.Format)
	default:
		return 0, fmt.Errorf("unsupported candidate source type %q", source.Type)
	}
}

func defaultFormat(sourceType, configured string) string {
	if configured != "" {
		return configured
	}
	if sourceType == "csv" || sourceType == "json" {
		return sourceType
	}
	return "text"
}

func defaultValue(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

type CloudflareRanges struct {
	Result struct {
		IPv4 []string `json:"ipv4_cidrs"`
		IPv6 []string `json:"ipv6_cidrs"`
	} `json:"result"`
	Success bool `json:"success"`
}

func (s *Service) SyncCloudflare(ctx context.Context, sourceID string, samplePerPrefix int) (int, error) {
	var source domain.CandidateSource
	if err := s.store.DB.First(&source, "id = ?", sourceID).Error; err != nil {
		return 0, err
	}
	if source.Type != "cloudflare_official" {
		return 0, fmt.Errorf("source is not a Cloudflare official range source")
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.cloudflare.com/client/v4/ips", nil)
	request.Header.Set("User-Agent", "NewNexus/1.0")
	response, err := s.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Cloudflare ranges returned HTTP %d", response.StatusCode)
	}
	var ranges CloudflareRanges
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&ranges); err != nil {
		return 0, err
	}
	if !ranges.Success {
		return 0, fmt.Errorf("Cloudflare ranges response was not successful")
	}
	if samplePerPrefix <= 0 {
		samplePerPrefix = 32
	}
	if samplePerPrefix > 4096 {
		samplePerPrefix = 4096
	}
	candidates := make([]domain.Candidate, 0)
	for _, rawPrefix := range append(ranges.Result.IPv4, ranges.Result.IPv6...) {
		prefix, err := netip.ParsePrefix(rawPrefix)
		if err != nil {
			continue
		}
		for _, address := range samplePrefix(prefix, samplePerPrefix) {
			family := 6
			if address.Is4() {
				family = 4
			}
			candidates = append(candidates, domain.Candidate{ProjectID: source.ProjectID, SourceID: source.ID, Address: address.String(), Family: family, Tier: "inventory", Tags: "cloudflare_official", Healthy: false})
		}
	}
	for i := range candidates {
		if err := s.store.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "project_id"}, {Name: "address"}}, DoUpdates: clause.Assignments(map[string]any{"source_id": source.ID, "tags": "cloudflare_official"})}).Create(&candidates[i]).Error; err != nil {
			return 0, err
		}
	}
	now := time.Now().UTC()
	source.LastSyncedAt = &now
	source.CandidateCount = len(candidates)
	source.Status = "synced"
	_ = s.store.DB.Save(&source).Error
	s.publish("optimizer.synced", source.ID, fmt.Sprintf("已同步 %d 个候选地址", len(candidates)))
	return len(candidates), nil
}

func (s *Service) syncDNS(ctx context.Context, source domain.CandidateSource, hostname string) (int, error) {
	addresses, err := netguard.ResolvePublic(ctx, net.DefaultResolver, hostname)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, address := range addresses {
		if err := s.upsertCandidate(source, address); err == nil {
			count++
		}
	}
	return s.completeSourceSync(&source, count)
}

func (s *Service) syncSubscription(ctx context.Context, source domain.CandidateSource, rawURL, format string) (int, error) {
	parsed, _, err := netguard.ValidateURL(ctx, net.DefaultResolver, rawURL, map[int]bool{443: true})
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("User-Agent", "NewNexus/1.0")
	request.Header.Set("Accept", "application/json,text/csv,text/plain;q=0.9")
	response, err := s.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, fmt.Errorf("subscription returned HTTP %d", response.StatusCode)
	}
	if format == "" {
		contentType := strings.ToLower(response.Header.Get("Content-Type"))
		switch {
		case strings.Contains(contentType, "json"):
			format = "json"
		case strings.Contains(contentType, "csv"):
			format = "csv"
		default:
			format = "text"
		}
	}
	return s.Import(ctx, source, io.LimitReader(response.Body, 8<<20), format)
}

func (s *Service) completeSourceSync(source *domain.CandidateSource, count int) (int, error) {
	now := time.Now().UTC()
	source.LastSyncedAt, source.CandidateCount, source.Status = &now, count, "synced"
	if err := s.store.DB.Save(source).Error; err != nil {
		return count, err
	}
	s.publish("optimizer.synced", source.ID, fmt.Sprintf("已同步 %d 个候选地址", count))
	return count, nil
}

func (s *Service) Import(ctx context.Context, source domain.CandidateSource, reader io.Reader, format string) (int, error) {
	addresses := make([]string, 0)
	switch strings.ToLower(format) {
	case "csv":
		rows, err := csv.NewReader(io.LimitReader(reader, 8<<20)).ReadAll()
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			if len(row) > 0 {
				addresses = append(addresses, strings.TrimSpace(row[0]))
			}
		}
	case "json":
		if err := json.NewDecoder(io.LimitReader(reader, 8<<20)).Decode(&addresses); err != nil {
			return 0, err
		}
	default:
		raw, err := io.ReadAll(io.LimitReader(reader, 8<<20))
		if err != nil {
			return 0, err
		}
		addresses = strings.Fields(string(raw))
	}
	count := 0
	for _, raw := range addresses {
		if prefix, err := netip.ParsePrefix(raw); err == nil {
			for _, address := range samplePrefix(prefix, 64) {
				if err := s.upsertCandidate(source, address); err == nil {
					count++
				}
			}
			continue
		}
		address, err := netip.ParseAddr(raw)
		if err != nil {
			continue
		}
		if err := s.upsertCandidate(source, address); err == nil {
			count++
		}
	}
	now := time.Now().UTC()
	source.LastSyncedAt = &now
	source.CandidateCount = count
	source.Status = "synced"
	_ = s.store.DB.Save(&source).Error
	return count, nil
}

func (s *Service) upsertCandidate(source domain.CandidateSource, address netip.Addr) error {
	if err := netguard.ValidateAddress(address); err != nil {
		return err
	}
	family := 6
	if address.Is4() {
		family = 4
	}
	candidate := domain.Candidate{ProjectID: source.ProjectID, SourceID: source.ID, Address: address.String(), Family: family, Tier: "inventory", Tags: source.Name}
	return s.store.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "project_id"}, {Name: "address"}}, DoNothing: true}).Create(&candidate).Error
}

func samplePrefix(prefix netip.Prefix, count int) []netip.Addr {
	prefix = prefix.Masked()
	address := prefix.Addr()
	result := make([]netip.Addr, 0, count)
	if prefix.Bits() == address.BitLen() {
		return []netip.Addr{address}
	}
	for i := 0; i < count && prefix.Contains(address); i++ {
		if address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() && !address.IsLinkLocalUnicast() {
			result = append(result, address)
		}
		address = address.Next()
		if !address.IsValid() {
			break
		}
	}
	return result
}

func (s *Service) RecomputeScores(projectID string) error {
	var candidates []domain.Candidate
	if err := s.store.DB.Where("project_id = ?", projectID).Find(&candidates).Error; err != nil {
		return err
	}
	config := s.scoreConfig()
	now := time.Now().UTC()
	for i := range candidates {
		candidate := &candidates[i]
		var samples []domain.ProbeSample
		_ = s.store.DB.Where("candidate_id = ? AND observed_at > ?", candidate.ID, now.Add(-config.Window)).Order("observed_at desc").Limit(500).Find(&samples).Error
		if len(samples) == 0 {
			candidate.Healthy, candidate.Score, candidate.SampleCount = false, 0, 0
			candidate.Tier = "inventory"
			candidate.NextProbeAt = timePtr(now.Add(config.InventoryInterval))
			_ = s.store.DB.Save(candidate).Error
			continue
		}
		successes := 0
		tcp, tlsValue, ttfb, download := 0.0, 0.0, 0.0, 0.0
		totals := make([]float64, 0, len(samples))
		for _, sample := range samples {
			if sample.Success {
				successes++
				tcp += sample.TCPMS
				tlsValue += sample.TLSMS
				ttfb += sample.TTFBMS
				download += sample.DownloadBPS
				totals = append(totals, sample.TotalMS)
			}
		}
		candidate.SampleCount = len(samples)
		candidate.SuccessRate = float64(successes) / float64(len(samples)) * 100
		candidate.FailureRate = 100 - candidate.SuccessRate
		if successes > 0 {
			divisor := float64(successes)
			candidate.TCPMS, candidate.TLSMS, candidate.TTFBMS = tcp/divisor, tlsValue/divisor, ttfb/divisor
			candidate.DownloadBPS = download / divisor
			candidate.P50MS, candidate.P95MS = quantile(totals, 0.50), quantile(totals, 0.95)
		}
		candidate.LastProbedAt = &samples[0].ObservedAt
		fresh := now.Sub(samples[0].ObservedAt) <= config.MaxAge
		candidate.Healthy = len(samples) >= config.MinSamples && candidate.SuccessRate >= config.MinSuccessRate && fresh
		if candidate.Healthy {
			metrics := map[string]float64{
				"success":  candidate.SuccessRate,
				"failure":  100 - candidate.FailureRate,
				"tcp":      latencyScore(candidate.TCPMS, 250),
				"tls":      latencyScore(candidate.TLSMS, 400),
				"ttfb":     latencyScore(candidate.TTFBMS, 800),
				"p50":      latencyScore(candidate.P50MS, 1000),
				"p95":      latencyScore(candidate.P95MS, 1500),
				"download": clamp(candidate.DownloadBPS/(20*1024*1024)*100, 0, 100),
			}
			candidate.Score = weightedScore(metrics, config.Weights)
		} else {
			candidate.Score = 0
		}
		if candidate.Score >= config.ActiveScore {
			candidate.Tier = "active"
			candidate.NextProbeAt = timePtr(now.Add(config.ActiveInterval))
		} else if candidate.Score >= config.RankingScore {
			candidate.Tier = "ranking"
			candidate.NextProbeAt = timePtr(now.Add(config.RankingInterval))
		} else {
			candidate.Tier = "inventory"
			candidate.NextProbeAt = timePtr(now.Add(config.InventoryInterval))
		}
		if err := s.store.DB.Save(candidate).Error; err != nil {
			return err
		}
	}
	return nil
}

// ScopedRankings recalculates candidate scores from retained aggregates for
// a node-group or route-line health perspective. It never writes candidate
// tiers or DNS weights; global ranking remains the persisted canonical score.
func (s *Service) ScopedRankings(projectID, scope, scopeValue string, limit int) ([]domain.Candidate, error) {
	if scope != "node_group" && scope != "line" {
		return nil, fmt.Errorf("ranking scope must be node_group or line")
	}
	nodeGroup := strings.TrimSpace(scopeValue)
	if scope == "line" {
		var line domain.RouteLine
		if err := s.store.DB.First(&line, "id = ?", scopeValue).Error; err != nil {
			return nil, fmt.Errorf("route line does not exist")
		}
		if line.HealthPerspective == "matching_group" {
			nodeGroup = line.NodeGroup
		} else {
			nodeGroup = ""
		}
	}
	config := s.scoreConfig()
	now := time.Now().UTC()
	query := s.store.DB.Where("bucket = ? AND candidate_id <> ? AND bucket_start >= ?", "5m", "", now.Add(-config.Window))
	if nodeGroup != "" {
		query = query.Where("node_group = ?", nodeGroup)
	}
	var rows []domain.ProbeAggregate
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	type totals struct {
		samples, successes            int
		tcp, tls, ttfb, p50, p95, bps float64
		latest                        time.Time
	}
	grouped := make(map[string]*totals)
	for _, row := range rows {
		item := grouped[row.CandidateID]
		if item == nil {
			item = &totals{}
			grouped[row.CandidateID] = item
		}
		item.samples += row.Samples
		item.successes += row.Successes
		weight := float64(row.Successes)
		item.tcp += row.TCPMS * weight
		item.tls += row.TLSMS * weight
		item.ttfb += row.TTFBMS * weight
		item.p50 += row.P50MS * weight
		item.p95 += row.P95MS * weight
		item.bps += row.DownloadBPS * weight
		if row.BucketStart.After(item.latest) {
			item.latest = row.BucketStart
		}
	}
	ids := make([]string, 0, len(grouped))
	for id := range grouped {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []domain.Candidate{}, nil
	}
	var candidates []domain.Candidate
	query = s.store.DB.Where("id IN ?", ids)
	if projectID != "" {
		query = query.Where("project_id = ?", projectID)
	}
	if err := query.Find(&candidates).Error; err != nil {
		return nil, err
	}
	result := make([]domain.Candidate, 0, len(candidates))
	for index := range candidates {
		candidate := candidates[index]
		item := grouped[candidate.ID]
		candidate.SampleCount = item.samples
		if item.samples > 0 {
			candidate.SuccessRate = float64(item.successes) / float64(item.samples) * 100
		}
		candidate.FailureRate = 100 - candidate.SuccessRate
		if item.successes > 0 {
			divisor := float64(item.successes)
			candidate.TCPMS, candidate.TLSMS, candidate.TTFBMS = item.tcp/divisor, item.tls/divisor, item.ttfb/divisor
			candidate.P50MS, candidate.P95MS, candidate.DownloadBPS = item.p50/divisor, item.p95/divisor, item.bps/divisor
		}
		candidate.LastProbedAt = timePtr(item.latest)
		candidate.Healthy = item.samples >= config.MinSamples && candidate.SuccessRate >= config.MinSuccessRate && now.Sub(item.latest) <= config.MaxAge
		if candidate.Healthy {
			candidate.Score = weightedScore(map[string]float64{
				"success": candidate.SuccessRate, "failure": 100 - candidate.FailureRate,
				"tcp": latencyScore(candidate.TCPMS, 250), "tls": latencyScore(candidate.TLSMS, 400), "ttfb": latencyScore(candidate.TTFBMS, 800),
				"p50": latencyScore(candidate.P50MS, 1000), "p95": latencyScore(candidate.P95MS, 1500), "download": clamp(candidate.DownloadBPS/(20*1024*1024)*100, 0, 100),
			}, config.Weights)
		} else {
			candidate.Score = 0
		}
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score == result[j].Score {
			if result[i].Address == result[j].Address {
				return result[i].ID < result[j].ID
			}
			return result[i].Address < result[j].Address
		}
		return result[i].Score > result[j].Score
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// AggregateAndRetain rolls raw probe samples into the two supported reporting
// buckets and applies the default retention policy. It is deliberately
// idempotent: each bucket is replaced inside a transaction, so rerunning a
// scheduler tick cannot double-count samples.
func (s *Service) AggregateAndRetain(ctx context.Context) error {
	now := time.Now().UTC()
	if err := s.store.EnsureProbeSamplePartitions(now); err != nil {
		return fmt.Errorf("ensure probe sample partitions: %w", err)
	}
	rawDays := maxIntSetting(s.intSetting("retention.raw_days", 7), 1)
	fiveMinuteDays := maxIntSetting(s.intSetting("retention.five_minute_days", 30), 1)
	hourlyDays := maxIntSetting(s.intSetting("retention.hourly_days", 180), 1)
	var samples []domain.ProbeSample
	if err := s.store.DB.Where("observed_at >= ?", now.Add(-2*time.Hour)).Find(&samples).Error; err != nil {
		return err
	}
	nodes := make(map[string]domain.ProbeNode)
	var probeNodes []domain.ProbeNode
	if err := s.store.DB.Find(&probeNodes).Error; err != nil {
		return err
	}
	for _, node := range probeNodes {
		nodes[node.ID] = node
	}
	type aggregateKey struct {
		bucket, candidateID, targetID, nodeGroup, lineScope string
		start                                               time.Time
	}
	groups := make(map[aggregateKey][]domain.ProbeSample)
	for _, sample := range samples {
		if err := ctx.Err(); err != nil {
			return err
		}
		group := nodes[sample.NodeID].GroupName
		lineScope := "global"
		if sample.RouteTargetID != "" {
			var target domain.RouteTarget
			if s.store.DB.Select("route_line_id").First(&target, "id = ?", sample.RouteTargetID).Error == nil {
				var line domain.RouteLine
				if s.store.DB.Select("route_id, alias, provider_line_code").First(&line, "id = ?", target.RouteLineID).Error == nil {
					lineScope = line.ProviderLineCode
					if lineScope == "" {
						lineScope = line.Alias
					}
				}
			}
		}
		five := sample.ObservedAt.UTC().Truncate(5 * time.Minute)
		hour := sample.ObservedAt.UTC().Truncate(time.Hour)
		groups[aggregateKey{"5m", sample.CandidateID, sample.RouteTargetID, group, lineScope, five}] = append(groups[aggregateKey{"5m", sample.CandidateID, sample.RouteTargetID, group, lineScope, five}], sample)
		groups[aggregateKey{"1h", sample.CandidateID, sample.RouteTargetID, group, lineScope, hour}] = append(groups[aggregateKey{"1h", sample.CandidateID, sample.RouteTargetID, group, lineScope, hour}], sample)
	}
	return s.store.DB.Transaction(func(tx *gorm.DB) error {
		for key, rows := range groups {
			aggregate := buildAggregate(key.bucket, key.start, key.candidateID, key.targetID, key.nodeGroup, key.lineScope, rows)
			if err := tx.Where("bucket = ? AND bucket_start = ? AND candidate_id = ? AND route_target_id = ? AND node_group = ? AND line_scope = ?", key.bucket, key.start, key.candidateID, key.targetID, key.nodeGroup, key.lineScope).Delete(&domain.ProbeAggregate{}).Error; err != nil {
				return err
			}
			if err := tx.Create(&aggregate).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("observed_at < ?", now.Add(-time.Duration(rawDays)*24*time.Hour)).Delete(&domain.ProbeSample{}).Error; err != nil {
			return err
		}
		if err := tx.Where("bucket = ? AND bucket_start < ?", "5m", now.Add(-time.Duration(fiveMinuteDays)*24*time.Hour)).Delete(&domain.ProbeAggregate{}).Error; err != nil {
			return err
		}
		return tx.Where("bucket = ? AND bucket_start < ?", "1h", now.Add(-time.Duration(hourlyDays)*24*time.Hour)).Delete(&domain.ProbeAggregate{}).Error
	})
}

func maxIntSetting(value, fallback int) int {
	if value < 1 {
		return fallback
	}
	return value
}

func buildAggregate(bucket string, start time.Time, candidateID, targetID, nodeGroup, lineScope string, rows []domain.ProbeSample) domain.ProbeAggregate {
	aggregate := domain.ProbeAggregate{Model: domain.Model{ID: fmt.Sprintf("aggregate-%d-%s-%s-%s-%s", start.UnixNano(), candidateID, targetID, nodeGroup, lineScope)}, Bucket: bucket, BucketStart: start, CandidateID: candidateID, RouteTargetID: targetID, NodeGroup: nodeGroup, LineScope: lineScope, Samples: len(rows)}
	totals := make([]float64, 0, len(rows))
	for _, row := range rows {
		if row.Success {
			aggregate.Successes++
		}
		if row.Success {
			aggregate.TCPMS += row.TCPMS
			aggregate.TLSMS += row.TLSMS
			aggregate.TTFBMS += row.TTFBMS
			aggregate.DownloadBPS += row.DownloadBPS
			totals = append(totals, row.TotalMS)
		}
	}
	if aggregate.Samples > 0 {
		aggregate.SuccessRate = float64(aggregate.Successes) / float64(aggregate.Samples) * 100
	}
	if aggregate.Successes > 0 {
		divisor := float64(aggregate.Successes)
		aggregate.TCPMS /= divisor
		aggregate.TLSMS /= divisor
		aggregate.TTFBMS /= divisor
		aggregate.DownloadBPS /= divisor
		aggregate.P50MS, aggregate.P95MS = quantile(totals, 0.5), quantile(totals, 0.95)
	}
	return aggregate
}

type scoringConfig struct {
	Window, MaxAge, InventoryInterval, RankingInterval, ActiveInterval time.Duration
	MinSamples                                                         int
	MinSuccessRate, ActiveScore, RankingScore                          float64
	Weights                                                            map[string]float64
}

func (s *Service) scoreConfig() scoringConfig {
	return scoringConfig{
		Window: s.durationSetting("score.window", 24*time.Hour), MaxAge: s.durationSetting("score.max_age", 12*time.Hour),
		InventoryInterval: s.durationSetting("probe.discovery_interval", 6*time.Hour), RankingInterval: s.durationSetting("probe.ranking_interval", 5*time.Minute), ActiveInterval: s.durationSetting("probe.active_interval", 30*time.Second),
		MinSamples: s.intSetting("score.min_samples", 3), MinSuccessRate: s.floatSetting("score.min_success_rate", 90), ActiveScore: s.floatSetting("score.active_threshold", 90), RankingScore: s.floatSetting("score.ranking_threshold", 75),
		Weights: map[string]float64{
			"success": s.floatSetting("score.weight.success", 35), "tcp": s.floatSetting("score.weight.tcp", 10), "tls": s.floatSetting("score.weight.tls", 10),
			"ttfb": s.floatSetting("score.weight.ttfb", 15), "p50": s.floatSetting("score.weight.p50", 10), "p95": s.floatSetting("score.weight.p95", 15),
			"download": s.floatSetting("score.weight.download", 15), "failure": s.floatSetting("score.weight.failure", 10),
		},
	}
}

func weightedScore(metrics, weights map[string]float64) float64 {
	total, divisor := 0.0, 0.0
	for name, weight := range weights {
		if weight <= 0 {
			continue
		}
		total += metrics[name] * weight
		divisor += weight
	}
	if divisor == 0 {
		return 0
	}
	return clamp(total/divisor, 0, 100)
}

func latencyScore(value, budget float64) float64 { return clamp(100-(value/budget*100), 0, 100) }

func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	position := q * float64(len(copyValues)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return copyValues[lower]
	}
	return copyValues[lower] + (copyValues[upper]-copyValues[lower])*(position-float64(lower))
}

func (s *Service) durationSetting(key string, fallback time.Duration) time.Duration {
	var setting domain.Setting
	if s.store.DB.First(&setting, "key = ?", key).Error != nil {
		return fallback
	}
	value, err := time.ParseDuration(setting.Value)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func (s *Service) intSetting(key string, fallback int) int {
	var setting domain.Setting
	if s.store.DB.First(&setting, "key = ?", key).Error != nil {
		return fallback
	}
	value, err := strconv.Atoi(setting.Value)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func (s *Service) floatSetting(key string, fallback float64) float64 {
	var setting domain.Setting
	if s.store.DB.First(&setting, "key = ?", key).Error != nil {
		return fallback
	}
	value, err := strconv.ParseFloat(setting.Value, 64)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func timePtr(value time.Time) *time.Time { return &value }

func clamp(value, minimum, maximum float64) float64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
func (s *Service) publish(kind, resource, message string) {
	raw, _ := json.Marshal(map[string]any{"type": kind, "resourceId": resource, "message": message, "at": time.Now().UTC()})
	s.broker.Publish(raw)
}
