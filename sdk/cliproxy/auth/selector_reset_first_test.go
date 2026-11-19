package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeAuth(id string, reset7d, reset5h time.Time) *Auth {
	signals := map[string]string{}
	if !reset7d.IsZero() {
		signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(reset7d.Unix(), 10)
	}
	if !reset5h.IsZero() {
		signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(reset5h.Unix(), 10)
	}
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{Signals: signals}}
}

func pickResetFirst(t *testing.T, now time.Time, auths ...*Auth) string {
	t.Helper()
	selector := &ResetFirstSelector{nowFunc: func() time.Time { return now }}
	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	return got.ID
}

func TestResetFirstSelector_PicksSoonestWeeklyReset(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	got := pickResetFirst(t, now,
		claudeAuth("a", now.Add(6*24*time.Hour), now.Add(time.Hour)),
		claudeAuth("b", now.Add(2*24*time.Hour), now.Add(4*time.Hour)),
		claudeAuth("c", now.Add(5*24*time.Hour), now.Add(time.Minute)),
	)
	if got != "b" {
		t.Fatalf("got %q, want b", got)
	}
}

func TestResetFirstSelector_UnknownProbedFirst(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	got := pickResetFirst(t, now,
		claudeAuth("a", now.Add(time.Hour), time.Time{}),
		claudeAuth("b", time.Time{}, time.Time{}),
	)
	if got != "b" {
		t.Fatalf("got %q, want unknown credential b", got)
	}
}

func TestResetFirstSelector_PassedResetCountsAsUnknown(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	got := pickResetFirst(t, now,
		claudeAuth("a", now.Add(time.Hour), time.Time{}),
		claudeAuth("b", now.Add(-time.Hour), time.Time{}),
	)
	if got != "b" {
		t.Fatalf("got %q, want rolled-over credential b", got)
	}
}

func TestResetFirstSelector_TieBreaksOnShortWindowThenID(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	week := now.Add(3 * 24 * time.Hour)
	if got := pickResetFirst(t, now,
		claudeAuth("a", week, now.Add(4*time.Hour)),
		claudeAuth("b", week, now.Add(time.Hour)),
	); got != "b" {
		t.Fatalf("got %q, want b", got)
	}
	if got := pickResetFirst(t, now, claudeAuth("b", week, time.Time{}), claudeAuth("a", week, time.Time{})); got != "a" {
		t.Fatalf("got %q, want a", got)
	}
}

func TestResetFirstSelector_RespectsPriorityTier(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	low := claudeAuth("low", now.Add(time.Hour), time.Time{})
	high := claudeAuth("high", now.Add(6*24*time.Hour), time.Time{})
	high.Attributes = map[string]string{"priority": "10"}
	if got := pickResetFirst(t, now, low, high); got != "high" {
		t.Fatalf("got %q, want high", got)
	}
}

func TestQuotaResetTimes_Codex(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	auth := &Auth{ID: "x", Provider: "codex", Quota: QuotaState{
		ObservedAt: now.Add(-10 * time.Minute),
		Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes":      "300",
			"X-Codex-Primary-Reset-After-Seconds": "3600",
			"X-Codex-Secondary-Window-Minutes":    "10080",
			"X-Codex-Secondary-Reset-At":          strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10),
			"X-Codex-Secondary-Used-Percent":      "40",
			"X-Codex-Primary-Used-Percent":        "10",
		},
	}}
	long, short := quotaResetTimes(auth, now)
	if !long.Equal(now.Add(48 * time.Hour)) {
		t.Fatalf("long = %v, want %v", long, now.Add(48*time.Hour))
	}
	if !short.Equal(now.Add(50 * time.Minute)) {
		t.Fatalf("short = %v, want %v", short, now.Add(50*time.Minute))
	}
}

func withUtil(a *Auth, window string, util string) *Auth {
	a.Quota.Signals["Anthropic-Ratelimit-Unified-"+window+"-Utilization"] = util
	return a
}

func TestResetFirstSelector_SkipsNearlySpentAccount(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	got := pickResetFirst(t, now,
		withUtil(claudeAuth("a", now.Add(time.Hour), now.Add(2*time.Hour)), "5h", "0.93"),
		claudeAuth("b", now.Add(3*24*time.Hour), now.Add(time.Hour)),
	)
	if got != "b" {
		t.Fatalf("got %q, want b (a is nearly spent in its 5h window)", got)
	}
}

func TestResetFirstSelector_NearlySpentStillUsedWhenAllAre(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	got := pickResetFirst(t, now,
		withUtil(claudeAuth("a", now.Add(time.Hour), time.Time{}), "7d", "0.97"),
		withUtil(claudeAuth("b", now.Add(3*24*time.Hour), time.Time{}), "7d", "0.99"),
	)
	if got != "a" {
		t.Fatalf("got %q, want a (soonest reset among low accounts)", got)
	}
}

func TestResetFirstSelector_ExpiredWindowUtilizationIgnored(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	a := withUtil(claudeAuth("a", now.Add(time.Hour), now.Add(-time.Minute)), "5h", "1.0")
	got := pickResetFirst(t, now, a, claudeAuth("b", now.Add(3*24*time.Hour), time.Time{}))
	if got != "a" {
		t.Fatalf("got %q, want a (its 5h window already reset)", got)
	}
}
