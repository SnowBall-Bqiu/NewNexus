package routing

import (
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
	"testing"
	"time"
)

func routeLine() domain.Doc {
	return domain.Doc{"min_nodes": 2, "quorum_percent": 67, "active_priority": 0, "targets": []domain.Doc{{"id": "primary", "cname": "a.example.net", "priority": 0, "weight": 5, "enabled": true, "health": "healthy"}, {"id": "backup", "cname": "b.example.net", "priority": 1, "weight": 3, "enabled": true, "health": "healthy"}}}
}
func votes(now time.Time, primary, backup bool) []Vote {
	out := []Vote{}
	for _, node := range []string{"n1", "n2", "n3"} {
		out = append(out, Vote{NodeID: node, TargetID: "primary", Healthy: primary, Score: 90, At: now}, Vote{NodeID: node, TargetID: "backup", Healthy: backup, Score: 80, At: now})
	}
	return out
}
func TestQuorumHysteresisCooldownAndFailStale(t *testing.T) {
	line := routeLine()
	now := time.Now()
	if d := Evaluate(line, nil, "availability", now); !d.Paused {
		t.Fatal("missing quorum was not paused")
	}
	for round := 0; round < 2; round++ {
		d := Evaluate(line, votes(now.Add(time.Duration(round)*time.Second), false, true), "availability", now.Add(time.Duration(round)*time.Second))
		if d.Changed {
			t.Fatal("failed before three rounds")
		}
	}
	d := Evaluate(line, votes(now.Add(2*time.Second), false, true), "availability", now.Add(2*time.Second))
	if !d.Changed || d.Priority != 1 {
		t.Fatal(d)
	}
	targets := domain.A(line, "targets")
	if domain.N(targets[0], "weight") != 5 || domain.N(targets[1], "weight") != 3 {
		t.Fatal("manual weights changed")
	}
	for round := 3; round < 6; round++ {
		d = Evaluate(line, votes(now.Add(time.Duration(round)*time.Second), false, false), "availability", now.Add(time.Duration(round)*time.Second))
	}
	if !d.AllFailed || domain.N(line, "active_priority") != 1 {
		t.Fatal("all-failure did not preserve active priority", d)
	}
}
func TestRepeatedSamplesDoNotAdvanceFailureRounds(t *testing.T) {
	line := routeLine()
	now := time.Now()
	for i := 0; i < 10; i++ {
		Evaluate(line, votes(now, false, true), "availability", now)
	}
	if domain.N(domain.A(line, "targets")[0], "consecutive_failures") != 1 {
		t.Fatal(line)
	}
}
func TestCNAMECyclesAndProviderCapabilities(t *testing.T) {
	if e := DetectLoop("a.example", map[string][]string{"a.example": {"b.example"}, "b.example": {"a.example"}}); e == nil {
		t.Fatal("cycle accepted")
	}
	rule := domain.Doc{"hostname": "example.com", "zone_name": "example.com", "lines": []domain.Doc{{"provider_line_code": "default", "targets": []domain.Doc{{"cname": "cdn.example.net", "weight": 1}}}}}
	if e := Validate(rule, provider.Capabilities{}); e == nil {
		t.Fatal("unsupported apex accepted")
	}
	if e := Validate(rule, provider.Capabilities{Apex: true}); e != nil {
		t.Fatal(e)
	}
}

func TestTwoThirdsQuorumAndIndependentTargetRounds(t *testing.T) {
	line := routeLine()
	now := time.Now()
	v := votes(now, true, true)
	v[0].Healthy = false
	d := Evaluate(line, v, "availability", now)
	if d.Paused || domain.S(domain.A(line, "targets")[0], "health") != "healthy" {
		t.Fatal("two out of three must form quorum", d)
	}
	v = votes(now, false, true)
	for i := 0; i < 5; i++ {
		for j := range v {
			if v[j].TargetID == "backup" {
				v[j].At = now.Add(time.Duration(i+1) * time.Second)
			}
		}
		Evaluate(line, v, "availability", now.Add(time.Duration(i+1)*time.Second))
	}
	if domain.N(domain.A(line, "targets")[0], "consecutive_failures") != 0 {
		t.Fatal("new backup samples advanced stale primary rounds", line)
	}
}
