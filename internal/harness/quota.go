package harness

import "time"

type Window struct {
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    *int64  `json:"resetsAt"`
}

type QuotaBucket struct {
	LimitID              string  `json:"limitId"`
	NormalModelSlug      string  `json:"normalModelSlug"`
	Primary              *Window `json:"primary"`
	Secondary            *Window `json:"secondary"`
	RateLimitReachedType string  `json:"rateLimitReachedType"`
}

type Quota struct {
	Allowed *bool                  `json:"ordinaryUsageAllowed"`
	Bucket  QuotaBucket            `json:"rateLimits"`
	Buckets map[string]QuotaBucket `json:"rateLimitsByLimitId"`
}

// nextQuotaCheck uses only the selected model's buckets (or the legacy view).
// Percentages choose a check time; they never prove allowance has recovered.
func nextQuotaCheck(q Quota, model string, now time.Time, fallback time.Duration) time.Time {
	var latest time.Time
	unknown, stale := false, false
	for _, b := range applicableBuckets(q, model) {
		for _, w := range []*Window{b.Primary, b.Secondary} {
			if w == nil || w.UsedPercent < 100 {
				continue
			}
			if w.ResetsAt == nil {
				unknown = true
				continue
			}
			reset := time.Unix(*w.ResetsAt, 0).Add(30 * time.Second)
			if !reset.After(now) {
				stale = true
				continue
			}
			if reset.After(latest) {
				latest = reset
			}
		}
	}
	if unknown {
		if t := now.Add(fallback); t.After(latest) {
			latest = t
		}
	}
	if !latest.IsZero() {
		return latest
	}
	if stale {
		return now.Add(time.Minute)
	}
	return now.Add(fallback)
}

func applicableBuckets(q Quota, model string) []QuotaBucket {
	var matches []QuotaBucket
	for id, b := range q.Buckets {
		if model != "" && (b.NormalModelSlug == model || id == model) {
			matches = append(matches, b)
		}
	}
	if len(matches) != 0 {
		return matches
	}
	if b, ok := q.Buckets[q.Bucket.LimitID]; ok {
		return []QuotaBucket{b}
	}
	return []QuotaBucket{q.Bucket}
}

func hasFutureBlock(q Quota, model string, now time.Time) bool {
	for _, b := range applicableBuckets(q, model) {
		for _, w := range []*Window{b.Primary, b.Secondary} {
			if w != nil && w.UsedPercent >= 100 && w.ResetsAt != nil && time.Unix(*w.ResetsAt, 0).Add(30*time.Second).After(now) {
				return true
			}
		}
	}
	return false
}
