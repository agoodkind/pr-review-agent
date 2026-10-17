package config_test

import (
	"math"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
)

func TestTokenPricingSeparatesCacheReadsWritesAndOrdinaryInput(t *testing.T) {
	writeRate := 0.25
	pricing := config.ModelPricing{
		InputPerMillionTokens: 0.20, CachedInputPerMillionTokens: 0.02,
		CacheWritePerMillionTokens: &writeRate, OutputPerMillionTokens: 1.20,
	}
	cost, priced := pricing.Estimate(1_000_000, 400_000, 200_000, 100_000, "default")
	if !priced || math.Abs(cost.TotalUSD()-0.258) > 1e-12 || math.Abs(cost.CacheWriteUSD-0.05) > 1e-12 {
		t.Fatalf("cache writes did not use their configured rate: %+v, priced=%t", cost, priced)
	}
	pricing.CacheWritePerMillionTokens = nil
	if _, priced := pricing.Estimate(1_000_000, 400_000, 200_000, 100_000, ""); priced {
		t.Fatal("missing cache write pricing produced a complete estimate")
	}
	pricing.CacheWritePerMillionTokens = new(float64)
	cost, priced = pricing.Estimate(1_000_000, 400_000, 200_000, 100_000, "default")
	if !priced || math.Abs(cost.TotalUSD()-0.208) > 1e-12 {
		t.Fatalf("an explicit zero write rate did not price the request: %+v", cost)
	}
	if _, priced := pricing.Estimate(100, 75, 50, 10, "default"); priced {
		t.Fatal("overlapping input categories produced a price")
	}
	for _, tier := range []string{"flex", "priority", "auto"} {
		if _, priced := pricing.Estimate(100, 0, 0, 10, tier); priced {
			t.Fatalf("service tier %s used the standard rates", tier)
		}
	}
}
