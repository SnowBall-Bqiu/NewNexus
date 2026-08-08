package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/optimizer"
	"github.com/SnowBall-Bqiu/NewNexus/internal/routing"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
)

type Scheduler struct {
	store     *store.Store
	hub       *agenthub.Hub
	optimizer *optimizer.Service
	routes    *routing.Service
	interval  time.Duration
	cursor    int
}

func New(store *store.Store, hub *agenthub.Hub, optimizer *optimizer.Service, routes *routing.Service, interval time.Duration) *Scheduler {
	return &Scheduler{store: store, hub: hub, optimizer: optimizer, routes: routes, interval: interval}
}

func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}
func (s *Scheduler) tick(ctx context.Context) {
	if err := s.routes.EvaluateAutomaticRoutes(ctx); err != nil {
		slog.Warn("automatic route evaluation failed", "error", err)
	}
	nodes := s.hub.ConnectedNodeIDs()
	if len(nodes) == 0 {
		return
	}
	if err := s.dispatchTargets(nodes); err != nil {
		slog.Warn("route probe scheduling failed", "error", err)
	}
	if err := s.dispatchCandidates(nodes); err != nil {
		slog.Warn("candidate probe scheduling failed", "error", err)
	}
}

func (s *Scheduler) dispatchTargets(nodes []string) error {
	connected := make(map[string]bool, len(nodes))
	for _, nodeID := range nodes {
		connected[nodeID] = true
	}
	var routes []domain.Route
	if err := s.store.DB.Where("status <> ?", "disabled").Find(&routes).Error; err != nil {
		return err
	}
	for _, route := range routes {
		var lines []domain.RouteLine
		if err := s.store.DB.Where("route_id = ?", route.ID).Find(&lines).Error; err != nil {
			return err
		}
		for _, line := range lines {
			lineNodes := nodes
			if line.HealthPerspective == "matching_group" && line.NodeGroup != "" {
				var grouped []domain.ProbeNode
				if err := s.store.DB.Where("group_name = ?", line.NodeGroup).Find(&grouped).Error; err != nil {
					return err
				}
				lineNodes = make([]string, 0, len(grouped))
				for _, node := range grouped {
					if connected[node.ID] {
						lineNodes = append(lineNodes, node.ID)
					}
				}
				if len(lineNodes) < maxInt(2, line.MinNodes) {
					continue
				}
			}
			var targets []domain.RouteTarget
			if err := s.store.DB.Where("route_line_id = ? AND enabled = ?", line.ID, true).Find(&targets).Error; err != nil {
				return err
			}
			for _, target := range targets {
				task := agenthub.Task{RouteTargetID: target.ID, Mode: "http", Address: target.CNAME, URL: "https://" + route.Hostname + "/", Host: route.Hostname, SNI: route.Hostname, Method: "GET", ExpectedStatuses: []int{200, 204, 301, 302, 307, 308, 401, 403}, TimeoutMS: 5000, MaxBytes: 65536}
				node := lineNodes[s.cursor%len(lineNodes)]
				s.cursor++
				_ = s.hub.Dispatch(node, task)
			}
		}
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Scheduler) dispatchCandidates(nodes []string) error {
	var candidates []domain.Candidate
	if err := s.store.DB.Where("tier IN ?", []string{"active", "ranking"}).Order("score desc").Limit(200).Find(&candidates).Error; err != nil {
		return err
	}
	var profile domain.ProbeProfile
	_ = s.store.DB.Where("scope = ?", "optimizer").First(&profile).Error
	parsed, _ := url.Parse(profile.URL)
	host := profile.Host
	if host == "" && parsed != nil {
		host = parsed.Hostname()
	}
	sni := profile.SNI
	if sni == "" {
		sni = host
	}
	statuses := parseStatuses(profile.ExpectedStatuses)
	for _, candidate := range candidates {
		task := agenthub.Task{CandidateID: candidate.ID, Mode: map[bool]string{true: "http", false: "tcp"}[profile.URL != ""], Address: candidate.Address, URL: profile.URL, Host: host, SNI: sni, Method: profile.Method, ExpectedStatuses: statuses, ExpectedContains: profile.ExpectedContains, TimeoutMS: profile.TimeoutMS, MaxBytes: profile.MaxBytes}
		node := nodes[s.cursor%len(nodes)]
		s.cursor++
		_ = s.hub.Dispatch(node, task)
	}
	if len(candidates) > 0 {
		go func() {
			if err := s.optimizer.RecomputeScores(candidates[0].ProjectID); err != nil {
				slog.Warn("score recomputation failed", "error", err)
			}
		}()
	}
	return nil
}
func parseStatuses(raw string) []int {
	var values []int
	if strings.TrimSpace(raw) == "" {
		return []int{200, 204}
	}
	_ = json.Unmarshal([]byte(raw), &values)
	if len(values) > 0 {
		return values
	}
	for _, part := range strings.Split(raw, ",") {
		switch strings.TrimSpace(part) {
		case "200":
			values = append(values, 200)
		case "204":
			values = append(values, 204)
		case "301":
			values = append(values, 301)
		case "302":
			values = append(values, 302)
		}
	}
	return values
}
