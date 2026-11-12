package config

import (
	"strings"
)

// Metrics prices the tokens of the sessions' transcripts and sets the
// thresholds at which the watch reports a runaway session.
type Metrics struct {
	// Models maps a model id, or the prefix of one, to its price and context
	// window. A configured entry replaces the default of the same key; a
	// model no entry matches has no cost ("cost unknown").
	Models map[string]Model `yaml:"models"`
	// Runaway holds the watch's thresholds; a negative one turns its
	// figure off.
	Runaway Runaway `yaml:"runaway"`
}

// Model is a model's price in US dollars per million tokens and its context
// window in tokens.
type Model struct {
	Input        float64 `yaml:"input" json:"input"`
	Output       float64 `yaml:"output" json:"output"`
	CacheWrite5m float64 `yaml:"cacheWrite5m" json:"cacheWrite5m"`
	CacheWrite1h float64 `yaml:"cacheWrite1h" json:"cacheWrite1h"`
	CacheRead    float64 `yaml:"cacheRead" json:"cacheRead"`
	// Fast multiplies every price of a request in fast mode; zero (the
	// defaults): a fast request's cost is unknown.
	Fast          float64 `yaml:"fast" json:"fast,omitempty"`
	ContextWindow int     `yaml:"contextWindow" json:"contextWindow"`
	// LongContextThreshold counts all input tokens, including cache reads
	// and writes. Requests above it multiply every price by LongContextMultiplier.
	LongContextThreshold  int64   `yaml:"longContextThreshold" json:"longContextThreshold,omitempty"`
	LongContextMultiplier float64 `yaml:"longContextMultiplier" json:"longContextMultiplier,omitempty"`
}

// Runaway are the per-session figures over which the watch prints one line.
type Runaway struct {
	// GitHubCallsPerHour: gh and devctl commands and GitHub MCP tool calls
	// in the last hour.
	GitHubCallsPerHour int `yaml:"githubCallsPerHour"`
	// SameErrorRepeats: the same failing tool call in the last hour.
	SameErrorRepeats int `yaml:"sameErrorRepeats"`
	// ContextFill: the last request's context over the model's window.
	ContextFill float64 `yaml:"contextFill"`
}

// DefaultModels are the Claude API list prices (platform.claude.com/docs/en/
// about-claude/pricing): cache writes cost 1.25 times the input price for
// the 5-minute TTL and twice for the 1-hour TTL, cache reads a tenth,
// except where a model's own rate differs (Opus 5.5 $0.20, Fable 5.1 $0.25).
var DefaultModels = map[string]Model{
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheWrite5m: 12.5, CacheWrite1h: 20, CacheRead: 0.25, ContextWindow: 1_000_000},
	"claude-fable-5":    {Input: 10, Output: 50, CacheWrite5m: 12.5, CacheWrite1h: 20, CacheRead: 1, ContextWindow: 1_000_000},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheWrite5m: 5, CacheWrite1h: 8, CacheRead: 0.2, ContextWindow: 1_000_000},
	"claude-opus-5":     {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, ContextWindow: 1_000_000},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, ContextWindow: 1_000_000},
	"claude-opus-4-7":   {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, ContextWindow: 1_000_000},
	"claude-opus-4-6":   {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.5, ContextWindow: 1_000_000},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.2, ContextWindow: 1_000_000},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.2, ContextWindow: 1_000_000},
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.3, ContextWindow: 1_000_000},
	"claude-haiku-5-5":  {Input: 0.1, Output: 0.5, CacheWrite5m: 0.125, CacheWrite1h: 0.2, CacheRead: 0.01, ContextWindow: 1_000_000, LongContextThreshold: 100_000, LongContextMultiplier: 5},
	"claude-haiku-4-5":  {Input: 1, Output: 5, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.1, ContextWindow: 200_000},
}

// Model returns the entry of model: its id, its dated snapshot
// ("claude-haiku-4-5-20251001"), or its agy effort selector
// ("claude-opus-5-5-high"). Another model of the same family is not priced
// like it.
func (m Metrics) Model(model string) (Model, bool) {
	if p, ok := m.Models[model]; ok {
		return p, true
	}
	if i := len(model) - len("-20060102"); i > 0 && model[i] == '-' && isDigits(model[i+1:]) {
		p, ok := m.Models[model[:i]]
		return p, ok
	}
	for _, suf := range []string{"-low", "-medium", "-high"} {
		if base, ok := strings.CutSuffix(model, suf); ok {
			if p, ok := m.Models[base]; ok {
				return p, true
			}
		}
	}
	return Model{}, false
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
