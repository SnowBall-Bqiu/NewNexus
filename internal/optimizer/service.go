package optimizer

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"gorm.io/gorm/clause"
)

type Service struct {
	store  *store.Store
	broker *events.Broker
	client *http.Client
}

func New(store *store.Store, broker *events.Broker) *Service {
	return &Service{store: store, broker: broker, client: &http.Client{Timeout: 20 * time.Second}}
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
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return fmt.Errorf("blocked address")
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
	for i := range candidates {
		candidate := &candidates[i]
		var samples []domain.ProbeSample
		_ = s.store.DB.Where("candidate_id = ? AND observed_at > ?", candidate.ID, time.Now().UTC().Add(-24*time.Hour)).Order("observed_at desc").Limit(50).Find(&samples).Error
		if len(samples) == 0 {
			continue
		}
		successes := 0
		tcp, tlsValue, ttfb := 0.0, 0.0, 0.0
		for _, sample := range samples {
			if sample.Success {
				successes++
			}
			tcp += sample.TCPMS
			tlsValue += sample.TLSMS
			ttfb += sample.TTFBMS
		}
		candidate.SuccessRate = float64(successes) / float64(len(samples)) * 100
		candidate.TCPMS = tcp / float64(len(samples))
		candidate.TLSMS = tlsValue / float64(len(samples))
		candidate.TTFBMS = ttfb / float64(len(samples))
		candidate.Healthy = candidate.SuccessRate >= 90
		latency := candidate.TCPMS + candidate.TLSMS + candidate.TTFBMS
		candidate.Score = clamp(candidate.SuccessRate*0.65+clamp(100-latency/3, 0, 100)*0.35, 0, 100)
		if candidate.Score >= 92 {
			candidate.Tier = "active"
		} else if candidate.Score >= 80 {
			candidate.Tier = "ranking"
		} else {
			candidate.Tier = "inventory"
		}
		_ = s.store.DB.Save(candidate).Error
	}
	return nil
}

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
