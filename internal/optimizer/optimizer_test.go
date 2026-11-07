package optimizer

import (
	"testing"
	"time"
)

func TestCIDRExpansionAndIPv6Sampling(t *testing.T) {
	ips, e := Expand("1.1.1.0/30 1.1.1.1 2606:4700::/32", 1000)
	if e != nil {
		t.Fatal(e)
	}
	if len(ips) < 60 || len(ips) > 68 {
		t.Fatalf("unexpected bounded sample count %d", len(ips))
	}
	again, _ := Expand("1.1.1.0/30 1.1.1.1 2606:4700::/32", 1000)
	for i, ip := range ips {
		if ip != again[i] {
			t.Fatal("sampling must remain stable on reimport")
		}
	}
	if _, e = Expand("10.0.0.0/8", 10); e == nil {
		t.Fatal("private CIDR accepted")
	}
}
func TestScoringFiltersAndWeightedSamples(t *testing.T) {
	now := time.Now()
	one := Sample{Success: true, Count: 3, Rate: 1, TCP: 10, TLS: 20, TTFB: 30, Download: 1000000, At: now}
	score := Score([]Sample{one}, now, 3, time.Minute)
	if !score.Healthy || score.Count != 3 || score.Score <= 0 {
		t.Fatal(score)
	}
	stale := Score([]Sample{one}, now.Add(2*time.Minute), 3, time.Minute)
	if stale.Healthy || stale.Score != 0 {
		t.Fatal(stale)
	}
	one.Rate = .5
	one.Success = false
	if Score([]Sample{one}, now, 3, time.Minute).Healthy {
		t.Fatal("low success ratio qualified")
	}
}
func TestExpansionReservesCapacityForIPv6AndValidatesTruncatedTail(t *testing.T) {
	ips, e := Expand("104.16.0.0/12 2606:4700::/32", 1000)
	if e != nil {
		t.Fatal(e)
	}
	v4, v6 := 0, 0
	for _, ip := range ips {
		if ip.Is4() {
			v4++
		} else {
			v6++
		}
	}
	if v4 == 0 || v6 != 64 || len(ips) > 1000 {
		t.Fatal("IPv4 consumed the IPv6 sampling budget", v4, v6)
	}
	if _, e = Expand("104.16.0.0/12 10.0.0.1", 1); e == nil {
		t.Fatal("a disallowed address after the output bound was ignored")
	}
	ips, e = Expand("2606:4700::/122", 1000)
	if e != nil || len(ips) != 64 {
		t.Fatal("small IPv6 range was sampled with duplicate hashes", len(ips), e)
	}
}
