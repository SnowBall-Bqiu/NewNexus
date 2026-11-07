package routing

import (
	"errors"
	"fmt"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
	"sort"
	"strings"
	"time"
)

type Vote struct {
	NodeID, TargetID string
	Healthy          bool
	Score            float64
	At               time.Time
}
type Decision struct {
	Changed, Paused, AllFailed bool
	Reason                     string
	Priority                   int
	TargetIDs                  []string
}

func Evaluate(line domain.Doc, votes []Vote, mode string, now time.Time) Decision {
	failureRounds := domain.N(line, "failure_required_rounds")
	if failureRounds <= 0 {
		failureRounds = 3
	}
	recoveryRounds := domain.N(line, "recovery_required_rounds")
	if recoveryRounds <= 0 {
		recoveryRounds = 5
	}
	performanceRounds := domain.N(line, "performance_required_rounds")
	if performanceRounds <= 0 {
		performanceRounds = 5
	}
	improvement := domain.N(line, "improvement_percent")
	if improvement <= 0 {
		improvement = 20
	}
	cooldown := domain.N(line, "cooldown_seconds")
	if line["cooldown_seconds"] == nil {
		cooldown = 600
	}
	minNodes := int(domain.N(line, "min_nodes"))
	if minNodes < 2 {
		minNodes = 2
	}
	quorum := domain.N(line, "quorum_percent")
	if quorum < 66.66 {
		quorum = 66.67
	}
	if quorum <= 67 {
		quorum = 200. / 3
	}
	targets := domain.A(line, "targets")
	latest := map[string]map[string]Vote{}
	newest := time.Time{}
	for _, v := range votes {
		if now.Sub(v.At) > 90*time.Second || v.At.After(now.Add(2*time.Minute)) {
			continue
		}
		if latest[v.TargetID] == nil {
			latest[v.TargetID] = map[string]Vote{}
		}
		if prior, ok := latest[v.TargetID][v.NodeID]; !ok || v.At.After(prior.At) {
			latest[v.TargetID][v.NodeID] = v
		}
		if v.At.After(newest) {
			newest = v.At
		}
	}
	previous, _ := time.Parse(time.RFC3339Nano, domain.S(line, "last_evaluated_sample_at"))
	advance := newest.After(previous)
	adequate := true
	for _, t := range targets {
		if !domain.B(t, "enabled") {
			continue
		}
		group := latest[domain.S(t, "id")]
		if len(group) < minNodes {
			adequate = false
			continue
		}
		healthy := 0
		score := 0.
		targetNewest := time.Time{}
		for _, v := range group {
			if v.At.After(targetNewest) {
				targetNewest = v.At
			}
			if v.Healthy {
				healthy++
				score += v.Score
			}
		}
		ok := float64(healthy)*100/float64(len(group))+1e-9 >= quorum
		targetPrevious, _ := time.Parse(time.RFC3339Nano, domain.S(t, "last_health_sample_at"))
		if targetNewest.After(targetPrevious) {
			t["last_health_sample_at"] = targetNewest.UTC().Format(time.RFC3339Nano)
			if ok {
				t["consecutive_healthy"] = domain.N(t, "consecutive_healthy") + 1
				t["consecutive_failures"] = 0
				if domain.N(t, "consecutive_healthy") >= recoveryRounds || domain.S(t, "health") == "" || domain.S(t, "health") == "unknown" {
					t["health"] = "healthy"
				}
			} else {
				t["consecutive_failures"] = domain.N(t, "consecutive_failures") + 1
				t["consecutive_healthy"] = 0
				if domain.N(t, "consecutive_failures") >= failureRounds {
					t["health"] = "unhealthy"
				}
			}
		}
		if healthy > 0 {
			t["score"] = score / float64(healthy)
		}
	}
	line["targets"] = targets
	if !adequate {
		line["auto_switch_paused"] = true
		line["pause_reason"] = "insufficient fresh node samples for quorum"
		return Decision{Paused: true, Reason: domain.S(line, "pause_reason")}
	}
	line["last_evaluated_sample_at"] = newest.UTC().Format(time.RFC3339Nano)
	line["auto_switch_paused"] = false
	line["pause_reason"] = ""
	prior := int(domain.N(line, "active_priority"))
	groups := map[int][]domain.Doc{}
	for _, t := range targets {
		if domain.B(t, "enabled") && domain.S(t, "health") == "healthy" {
			p := int(domain.N(t, "priority"))
			groups[p] = append(groups[p], t)
		}
	}
	if len(groups) == 0 {
		line["status"] = "fail_stale"
		return Decision{AllFailed: true, Reason: "all targets failed; existing DNS is preserved", Priority: prior}
	}
	priorities := []int{}
	for p := range groups {
		priorities = append(priorities, p)
	}
	sort.Ints(priorities)
	desired := priorities[0]
	current := groups[prior]
	if len(current) > 0 {
		desired = prior
		if desired != priorities[0] {
			recovered := true
			for _, t := range groups[priorities[0]] {
				if domain.N(t, "consecutive_healthy") < recoveryRounds {
					recovered = false
				}
			}
			if recovered {
				desired = priorities[0]
			}
		}
		if mode == "performance" || mode == "ranking" {
			best := prior
			bestScore := -1.
			currentScore := 0.
			for _, t := range current {
				currentScore += domain.N(t, "score")
			}
			currentScore /= float64(len(current))
			baseline := domain.N(line, "performance_baseline")
			if baseline == 0 || currentScore > baseline {
				baseline = currentScore
				line["performance_baseline"] = baseline
			}
			for p, ts := range groups {
				score := 0.
				for _, t := range ts {
					score += domain.N(t, "score")
				}
				score /= float64(len(ts))
				if score > bestScore {
					bestScore = score
					best = p
				}
			}
			degraded := mode == "ranking" || currentScore <= baseline*(1-improvement/100)
			if best != prior && degraded && bestScore >= currentScore*(1+improvement/100) {
				if advance {
					line["performance_rounds"] = domain.N(line, "performance_rounds") + 1
				}
				if domain.N(line, "performance_rounds") >= performanceRounds {
					desired = best
				}
			} else {
				line["performance_rounds"] = 0
			}
		}
	}
	last, _ := time.Parse(time.RFC3339Nano, domain.S(line, "last_switched_at"))
	if desired != prior && !last.IsZero() && now.Sub(last) < time.Duration(cooldown)*time.Second {
		line["status"] = "cooldown"
		return Decision{Paused: true, Reason: "switch cooldown is active", Priority: prior}
	}
	ids := []string{}
	for _, t := range groups[desired] {
		ids = append(ids, domain.S(t, "id"))
	}
	changed := desired != prior
	old := domain.S(line, "active_target_ids")
	joined := strings.Join(ids, ",")
	if old != "" && old != joined {
		changed = true
	}
	line["active_priority"] = desired
	line["active_target_ids"] = joined
	line["status"] = "healthy"
	if changed {
		line["last_switched_at"] = now.UTC().Format(time.RFC3339Nano)
	}
	return Decision{Changed: changed, Priority: desired, TargetIDs: ids}
}
func Validate(rule domain.Doc, cap provider.Capabilities) error {
	host := strings.TrimSuffix(strings.ToLower(domain.S(rule, "hostname")), ".")
	zone := strings.TrimSuffix(strings.ToLower(domain.S(rule, "zone_name")), ".")
	if host == "" || zone == "" || host != zone && !strings.HasSuffix(host, "."+zone) {
		return errors.New("hostname must belong to the configured DNS zone")
	}
	if host == zone && !cap.Apex {
		return errors.New("provider has not confirmed apex CNAME flattening support")
	}
	seen := map[string]bool{}
	for _, line := range domain.A(rule, "lines") {
		code := domain.S(line, "provider_line_code")
		if code != "default" && code != "" && !cap.Lines {
			return errors.New("provider does not support DNS lines")
		}
		if seen[code] {
			return errors.New("duplicate DNS line")
		}
		seen[code] = true
		for _, t := range domain.A(line, "targets") {
			cname := strings.TrimSuffix(strings.ToLower(domain.S(t, "cname")), ".")
			if cname == host || cname == "" {
				return errors.New("CNAME target is empty or creates a loop")
			}
			if domain.N(t, "weight") <= 0 {
				return errors.New("target weight must be positive")
			}
			if domain.N(t, "weight") > 1 && !cap.Weights {
				return errors.New("weighted CNAME records are not supported by this provider/plan")
			}
		}
	}
	return nil
}
func DetectLoop(host string, edges map[string][]string) error {
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var walk func(string) error
	walk = func(n string) error {
		n = strings.TrimSuffix(strings.ToLower(n), ".")
		if visiting[n] {
			return fmt.Errorf("CNAME cycle at %s", n)
		}
		if visited[n] {
			return nil
		}
		visiting[n] = true
		for _, next := range edges[n] {
			if e := walk(next); e != nil {
				return e
			}
		}
		delete(visiting, n)
		visited[n] = true
		return nil
	}
	return walk(host)
}
func Diff(before, after []provider.Record) []provider.Change {
	out := []provider.Change{}
	used := make([]bool, len(before))
	for _, want := range after {
		found := -1
		for i, have := range before {
			if !used[i] && strings.EqualFold(have.Name, want.Name) && have.Type == want.Type && have.Line == want.Line && strings.EqualFold(strings.TrimSuffix(have.Value, "."), strings.TrimSuffix(want.Value, ".")) {
				found = i
				break
			}
		}
		w := want
		if found < 0 {
			out = append(out, provider.Change{Action: "create", Name: w.Name, Type: w.Type, Line: w.Line, After: &w})
			continue
		}
		used[found] = true
		b := before[found]
		if b.TTL != w.TTL || b.Weight != w.Weight || b.Proxied != w.Proxied {
			w.ID = b.ID
			out = append(out, provider.Change{Action: "update", Name: w.Name, Type: w.Type, Line: w.Line, Before: &b, After: &w})
		}
	}
	for i, b := range before {
		if !used[i] {
			old := b
			out = append(out, provider.Change{Action: "delete", Name: b.Name, Type: b.Type, Line: b.Line, Before: &old})
		}
	}
	return out
}
