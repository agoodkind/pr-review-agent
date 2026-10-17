package config

// TokenCost separates cache writes from ordinary input to prevent double charging.
type TokenCost struct {
	InputUSD       float64
	CachedInputUSD float64
	CacheWriteUSD  float64
	OutputUSD      float64
}

// Estimate requires cache write pricing and excludes explicit nonstandard service tiers.
func (pricing ModelPricing) Estimate(input, cached, writes, output int64, serviceTier string) (TokenCost, bool) {
	var cost TokenCost
	if serviceTier != "" && serviceTier != "default" {
		return cost, false
	}
	if input < 0 || cached < 0 || writes < 0 || output < 0 || cached > input || writes > input-cached ||
		(writes > 0 && pricing.CacheWritePerMillionTokens == nil) {
		return cost, false
	}
	cost = TokenCost{
		InputUSD:       float64(input-cached-writes) * pricing.InputPerMillionTokens / 1_000_000,
		CachedInputUSD: float64(cached) * pricing.CachedInputPerMillionTokens / 1_000_000,
		CacheWriteUSD:  0,
		OutputUSD:      float64(output) * pricing.OutputPerMillionTokens / 1_000_000,
	}
	if writes > 0 {
		cost.CacheWriteUSD = float64(writes) * *pricing.CacheWritePerMillionTokens / 1_000_000
	}
	return cost, true
}

// TotalUSD sums each priced token category exactly once.
func (cost TokenCost) TotalUSD() float64 {
	return cost.InputUSD + cost.CachedInputUSD + cost.CacheWriteUSD + cost.OutputUSD
}
