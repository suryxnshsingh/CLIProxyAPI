package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetFirstSelector drains the credential whose long (weekly) usage window
// resets soonest, so quota that would be lost at reset is spent first and
// credentials that reset later stay in reserve.
//
// Reset times come from the passive quota snapshot (Auth.Quota.Signals) that
// every Claude and Codex response already records. A credential with no
// usable snapshot (never used, or its window has already rolled over) is
// picked first so one request reveals its reset time. Priority tiers and
// cooldowns apply exactly as in FillFirstSelector; ties keep ID order.
//
// A credential that is nearly spent in either window (see lowHeadroom) moves
// behind every credential with room left, so a long session lands on an
// account that can finish it instead of hitting the limit mid-run. Because the
// threshold is a share of the account's own window, a smaller plan (Max 5x)
// reaches it sooner in absolute terms than a larger one (Max 20x).
type ResetFirstSelector struct {
	// nowFunc is overridable in tests.
	nowFunc func() time.Time
}

// Pick selects the available credential with the earliest known long-window reset.
func (s *ResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s != nil && s.nowFunc != nil {
		now = s.nowFunc()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	best := available[0]
	bestLong, bestShort := quotaResetTimes(best, now)
	bestLow := lowHeadroom(best, now)
	for _, candidate := range available[1:] {
		long, short := quotaResetTimes(candidate, now)
		low := lowHeadroom(candidate, now)
		if low != bestLow {
			if !low {
				best, bestLong, bestShort, bestLow = candidate, long, short, low
			}
			continue
		}
		if resetEarlier(long, short, bestLong, bestShort) {
			best, bestLong, bestShort, bestLow = candidate, long, short, low
		}
	}
	return best, nil
}

// Utilization at or above which a window counts as nearly spent.
const (
	lowHeadroomShort = 0.90 // 5-hour window
	lowHeadroomLong  = 0.95 // weekly window
)

// lowHeadroom reports whether the credential's last snapshot shows a usage
// window that is still open and nearly spent. Unknown utilization is not low.
func lowHeadroom(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return false
	}
	signals := auth.Quota.Signals
	get := func(name string) string {
		return strings.TrimSpace(signals[http.CanonicalHeaderKey(name)])
	}
	spent := func(window string, limit float64) bool {
		util, err := strconv.ParseFloat(get("Anthropic-Ratelimit-Unified-"+window+"-Utilization"), 64)
		if err != nil || util < limit {
			return false
		}
		return parseEpochSeconds(get("Anthropic-Ratelimit-Unified-" + window + "-Reset")).After(now)
	}
	return spent("5h", lowHeadroomShort) || spent("7d", lowHeadroomLong)
}

// resetEarlier orders by long-window reset (unknown first), then short-window reset.
func resetEarlier(long, short, bestLong, bestShort time.Time) bool {
	if long.IsZero() != bestLong.IsZero() {
		return long.IsZero()
	}
	if !long.Equal(bestLong) {
		return long.Before(bestLong)
	}
	if short.IsZero() || bestShort.IsZero() {
		return false
	}
	return short.Before(bestShort)
}

// quotaResetTimes returns the future reset times of the credential's long and
// short usage windows from its last observed quota snapshot. A zero time means
// unknown or already passed.
func quotaResetTimes(auth *Auth, now time.Time) (long, short time.Time) {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return time.Time{}, time.Time{}
	}
	signals := auth.Quota.Signals
	get := func(name string) string {
		return strings.TrimSpace(signals[http.CanonicalHeaderKey(name)])
	}
	future := func(t time.Time) time.Time {
		if t.After(now) {
			return t
		}
		return time.Time{}
	}

	if raw := get("Anthropic-Ratelimit-Unified-7d-Reset"); raw != "" || get("Anthropic-Ratelimit-Unified-5h-Reset") != "" {
		long = future(parseEpochSeconds(raw))
		short = future(parseEpochSeconds(get("Anthropic-Ratelimit-Unified-5h-Reset")))
		return long, short
	}

	type window struct {
		minutes int64
		reset   time.Time
	}
	var windows []window
	for _, name := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + name + "-"
		reset := parseEpochSeconds(get(prefix + "Reset-At"))
		if reset.IsZero() && !auth.Quota.ObservedAt.IsZero() {
			if secs, errParse := strconv.ParseInt(get(prefix+"Reset-After-Seconds"), 10, 64); errParse == nil && secs >= 0 {
				reset = auth.Quota.ObservedAt.Add(time.Duration(secs) * time.Second)
			}
		}
		if reset.IsZero() {
			continue
		}
		minutes, _ := strconv.ParseInt(get(prefix+"Window-Minutes"), 10, 64)
		windows = append(windows, window{minutes: minutes, reset: reset})
	}
	switch len(windows) {
	case 0:
		return time.Time{}, time.Time{}
	case 1:
		return future(windows[0].reset), time.Time{}
	}
	if windows[0].minutes > windows[1].minutes {
		windows[0], windows[1] = windows[1], windows[0]
	}
	return future(windows[1].reset), future(windows[0].reset)
}

func parseEpochSeconds(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if secs, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && secs > 0 {
		return time.Unix(secs, 0)
	}
	if t, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return t
	}
	return time.Time{}
}
