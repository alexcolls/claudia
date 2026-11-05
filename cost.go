package main

import "strings"

// USD states recorded on a run, from most to least trustworthy. All of them are
// estimates: even a CLI's own figure is computed client-side.
const (
	USDReported     = "reported"     // the CLI's own figure
	USDPriced       = "priced"       // tokens times the price table
	USDEstimated    = "estimated"    // no usable result: tokens seen in the stream
	USDUnpriced     = "unpriced"     // no price known for at least one model
	USDSubscription = "subscription" // tokens only: a plan, not per-token billing
)

// Price is USD per million tokens.
type Price struct {
	Model      string  `json:"model"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	CheckedAt  string  `json:"checked_at"`
}

func (p Price) cost(t Tokens) float64 {
	return (float64(t.Input)*p.Input + float64(t.Output)*p.Output +
		float64(t.CacheRead)*p.CacheRead + float64(t.CacheWrite)*p.CacheWrite) / 1e6
}

// advanceMarks returns what a run added, given the session's high-water marks,
// and moves the marks up. Every CLI reports session-wide totals, so a run's own
// usage is the difference. Marks never go down, because a crash can report
// zeros and a resume can restore less than was actually spent.
func advanceMarks(marks, cumulative Usage) Usage {
	delta := Usage{}
	for model, c := range cumulative {
		m := marks[model]
		d := Tokens{
			Input:      max(c.Input-m.Input, 0),
			Output:     max(c.Output-m.Output, 0),
			CacheRead:  max(c.CacheRead-m.CacheRead, 0),
			CacheWrite: max(c.CacheWrite-m.CacheWrite, 0),
			USD:        max(c.USD-m.USD, 0),
			Basis:      c.Basis,
		}
		if d.total() > 0 || d.USD > 0 {
			delta[model] = d
		}
		marks[model] = Tokens{
			Input:      max(c.Input, m.Input),
			Output:     max(c.Output, m.Output),
			CacheRead:  max(c.CacheRead, m.CacheRead),
			CacheWrite: max(c.CacheWrite, m.CacheWrite),
			USD:        max(c.USD, m.USD),
		}
	}
	return delta
}

// Settled is the final accounting for a run.
type Settled struct {
	Usage Usage
	USD   float64
	State string
}

// settle decides a run's cost. res is nil when the CLI never reported a result.
// delta is what the result added and live is what the stream showed.
//
// A result that isn't a success can undercount: Claude Code reports zero for a
// response it aborted, or for a crash. In that case the stream's count wins if
// it's larger, and the run is flagged as estimated.
func settle(res *Result, delta, live Usage, subscription bool, prices map[string]Price) Settled {
	usage, estimated := delta, false
	if res == nil || (!res.OK && live.total() > delta.total()) {
		usage, estimated = live, true
	}
	s := Settled{Usage: usage}
	if subscription {
		s.State = USDSubscription
		return s
	}
	unpriced, priced := false, false
	for model, t := range usage {
		switch {
		case !estimated && t.USD > 0 && t.Basis != "unknown":
			s.USD += t.USD
		default:
			p, ok := prices[model]
			if !ok {
				unpriced = unpriced || t.total() > 0
				continue
			}
			s.USD += p.cost(t)
			priced = true
		}
	}
	switch {
	case unpriced:
		s.State = USDUnpriced
	case estimated:
		s.State = USDEstimated
	case priced:
		s.State = USDPriced
	default:
		s.State = USDReported
	}
	return s
}

// liveUSD prices usage seen so far during a run. Unknown models count as zero,
// which is why dispatch refuses them while a USD budget is active.
func liveUSD(u Usage, prices map[string]Price) float64 {
	var total float64
	for m, t := range u {
		if p, ok := prices[m]; ok {
			total += p.cost(t)
		}
	}
	return total
}

// PauseUsageLimit is the pause reason for a subscription out of quota.
const PauseUsageLimit = "usage limit reached"

// pauseClass says whether a CLI error needs the owner rather than a task retry.
func pauseClass(category string) string {
	switch category {
	case "authentication_failed", "oauth_org_not_allowed", "cloud_credential_error":
		return "authentication failed"
	case "billing_error", "account_on_hold":
		return "billing problem"
	case "rate_limit":
		return "rate limited"
	}
	return ""
}

// pauseFromText does the same for CLIs that report no error category. The strings
// are from real responses: OpenAI's 401 through Codex and Google's 400 through agy.
func pauseFromText(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "hit your limit"), strings.Contains(t, "usage limit reached"):
		return PauseUsageLimit
	case strings.Contains(t, "401 unauthorized"), strings.Contains(t, "incorrect api key"), strings.Contains(t, "api key not valid"),
		strings.Contains(t, "invalid api key"), strings.Contains(t, "authentication failed"), strings.Contains(t, "authentication required"):
		return "authentication failed"
	case strings.Contains(t, "insufficient_quota"), strings.Contains(t, "credit balance"), strings.Contains(t, "billing"):
		return "billing problem"
	case strings.Contains(t, "429 too many"), strings.Contains(t, "rate limit"), strings.Contains(t, "resource_exhausted"):
		return "rate limited"
	}
	return ""
}

// seededPrices were checked on the providers' pricing pages on checkedAt. The owner
// can edit them, and they go stale: the table's dates are shown for that reason.
// Gemini's published increases are already included (Dec 2026 and Jan 2027), since a
// budget that runs slightly high is safer than one that quietly undercounts.
const pricesCheckedAt = "2026-11-04"

var seededPrices = []Price{
	{Model: "claude-fable-5-1", Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	{Model: "claude-opus-5-5", Input: 4, Output: 20, CacheRead: 0.20, CacheWrite: 5},
	{Model: "claude-sonnet-5-5", Input: 2, Output: 10, CacheRead: 0.10, CacheWrite: 2.5},
	{Model: "claude-haiku-5-5", Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.125}, // prompts up to 100k tokens
	{Model: "claude-sonnet-4-5", Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75},
	{Model: "gpt-6.1-sol", Input: 2, Output: 10, CacheRead: 0.10, CacheWrite: 2},
	{Model: "gpt-6.1-terra", Input: 1, Output: 5, CacheRead: 0.05, CacheWrite: 1},
	{Model: "gpt-6-luna", Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.10},
	{Model: "gpt-5.3-codex", Input: 1.75, Output: 14, CacheRead: 0.175, CacheWrite: 1.75},
	{Model: "gemini-4-argon", Input: 4, Output: 20, CacheRead: 0.20},
	{Model: "gemini-3.8-flash", Input: 1.50, Output: 7.50, CacheRead: 0.15},
	{Model: "gemini-3.5-flash-lite", Input: 0.30, Output: 2.50, CacheRead: 0.03},
}

// seedPrices fills the table once, so deleting a row sticks.
func (a *App) seedPrices() error {
	if a.setting("prices_seeded") == "1" {
		return nil
	}
	for _, p := range seededPrices {
		p.CheckedAt = pricesCheckedAt
		if err := a.setPrice(p); err != nil {
			return err
		}
	}
	return a.setSetting("prices_seeded", "1")
}
