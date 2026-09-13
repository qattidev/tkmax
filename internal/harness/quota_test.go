package harness

import (
	"testing"
	"time"
)

func TestQuotaDeadlines(t *testing.T) {
	now := time.Unix(1800000000, 0)
	w := func(d time.Duration) *Window { return &Window{UsedPercent: 100, ResetsAt: ptr(now.Add(d).Unix())} }
	cases := []struct {
		name string
		q    Quota
		want time.Duration
	}{
		{"unknown", Quota{}, 5 * time.Hour},
		{"five-hour-window", Quota{Bucket: QuotaBucket{Primary: w(time.Hour)}}, time.Hour + 30*time.Second},
		{"weekly-wins", Quota{Bucket: QuotaBucket{Primary: w(time.Hour), Secondary: w(6 * 24 * time.Hour)}}, 6*24*time.Hour + 30*time.Second},
		{"unknown-exhausted-window", Quota{Bucket: QuotaBucket{Primary: w(time.Hour), Secondary: &Window{UsedPercent: 100}}}, 5 * time.Hour},
		{"stale-reset", Quota{Bucket: QuotaBucket{Primary: w(-time.Hour)}}, time.Minute},
		{"unrelated-bucket", Quota{Bucket: QuotaBucket{Primary: w(time.Hour)}, Buckets: map[string]QuotaBucket{"unrelated": {Primary: w(7 * 24 * time.Hour)}}}, time.Hour + 30*time.Second},
		{"model-specific", Quota{Bucket: QuotaBucket{Primary: w(7 * 24 * time.Hour)}, Buckets: map[string]QuotaBucket{"specific": {NormalModelSlug: "model-a", Primary: w(time.Hour)}}}, time.Hour + 30*time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextQuotaCheck(tc.q, "model-a", now, 5*time.Hour)
			if !got.Equal(now.Add(tc.want)) {
				t.Fatalf("got %v, want %v", got, now.Add(tc.want))
			}
		})
	}
	if !hasFutureBlock(Quota{Bucket: QuotaBucket{Primary: w(0)}}, "", now) {
		t.Fatal("reset buffer ignored")
	}
}
