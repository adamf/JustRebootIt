// Package aidiag runs an LLM-driven root-cause investigation when a latency
// event is detected. The mechanical diagnostics (traceroute, TCP, DNS) gather
// raw signal; this package hands that signal to a Claude agent that can call
// read-only investigative tools — query the local Prometheus, run a fresh
// traceroute/DNS lookup, and attribute a hop IP to its operator via RDAP — then
// writes a plain-language explanation of what most likely went wrong.
//
// It is entirely optional: with no API key configured, the prober never
// constructs an Analyzer and events are recorded without an AI writeup.
package aidiag

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/adamf/justrebootit/internal/tracer"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Config controls the AI diagnostics.
type Config struct {
	// Enabled gates the feature. Even when true, an empty APIKey disables it.
	Enabled bool
	// APIKey is the Anthropic API key. When empty, no Analyzer is built.
	APIKey string
	// BaseURL overrides the Anthropic API endpoint (option.WithBaseURL). Empty
	// uses the default. Point it at a local, Anthropic-compatible server to run a
	// local model at zero per-request cost.
	BaseURL string
	// Model is the expensive/default Claude model ID (default claude-opus-4-8).
	Model string
	// ModelCheap is the cheaper model used for far problems and for classes
	// where evaluation found it good enough (default claude-sonnet-4-6).
	ModelCheap string
	// PrometheusURL is the base URL of the JustRebootIt Prometheus, used by the
	// prometheus_query_range tool (e.g. http://prometheus:9090).
	PrometheusURL string
	// UDMExporterURL is the base URL of the udm-exporter (e.g.
	// http://udm-exporter:9431), whose /config endpoint exposes the gateway's
	// WAN configuration to the udm_config tool. Empty disables that tool.
	UDMExporterURL string
	// MaxIterations bounds the agent's tool-use loop.
	MaxIterations int
	// Timeout bounds a single investigation end to end.
	Timeout time.Duration
	// Privileged selects raw ICMP sockets for the traceroute tool.
	Privileged bool
	// TraceMaxHops / TraceTimeout configure the traceroute tool.
	TraceMaxHops int
	TraceTimeout time.Duration
}

// Event is everything the mechanical diagnostics already know about an anomaly,
// handed to the agent as its starting context.
type Event struct {
	ID       int64
	Target   string
	Host     string
	Group    string
	Reason   string // "latency" or "loss"
	Median   time.Duration
	Baseline time.Duration
	Loss     float64
	When     time.Time
	// Signature is the coalescer's fingerprint for this event, used to record
	// the resulting analysis for reuse.
	Signature string
	// Hops is the last-known hop distance to the target (0 = unknown), used to
	// tell our-network problems from distant ones.
	Hops int
	// ScopeKind is the coalescer's classification ("shared"/"near"/"far"), used
	// to pick which model investigates.
	ScopeKind string

	// Results of the mechanical diagnostics already run for this event.
	Trace      tracer.Result
	TCPConnect time.Duration
	TCPOK      bool
	DNSLookup  time.Duration
	DNSOK      bool
}

// Analysis is the agent's verdict.
type Analysis struct {
	EventID int64
	// Headline is a one-line summary (the first line of the model's output).
	Headline string
	// Text is the full writeup (root cause, confidence, recommendation).
	Text string
	// Model is the Claude model that produced this analysis.
	Model string
	// Usage is the token accounting for the investigation that produced this
	// analysis, so callers can tag the annotation and meter spend.
	Usage Usage
}

// Usage is the token accounting for one investigation.
type Usage struct {
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
}

// Total is all tokens that flowed through the request (cached + uncached + out).
func (u Usage) Total() int64 { return u.Input + u.CacheRead + u.CacheWrite + u.Output }

// CacheHit reports whether any of the prompt prefix was served from cache.
func (u Usage) CacheHit() bool { return u.CacheRead > 0 }

// Add accumulates another usage into this one (used to sum a dual-model eval).
func (u Usage) Add(o Usage) Usage {
	return Usage{u.Input + o.Input, u.CacheRead + o.CacheRead, u.CacheWrite + o.CacheWrite, u.Output + o.Output}
}

// Analyzer investigates events with a Claude agent.
type Analyzer struct {
	client anthropic.Client
	cfg    Config
	http   *http.Client
}

// New builds an Analyzer. It returns (nil, nil) when the feature is disabled or
// no API key is set, so callers can unconditionally call New and simply skip
// analysis when it returns nil.
func New(cfg Config) (*Analyzer, error) {
	// The feature needs either a real API key or a local endpoint (BaseURL). A
	// local Anthropic-compatible server usually ignores auth, so allow a missing
	// key when BaseURL is set, supplying a placeholder so the SDK is happy.
	if !cfg.Enabled || (cfg.APIKey == "" && cfg.BaseURL == "") {
		return nil, nil
	}
	if cfg.APIKey == "" {
		cfg.APIKey = "local"
	}
	if cfg.Model == "" {
		cfg.Model = string(anthropic.ModelClaudeOpus4_8)
	}
	if cfg.ModelCheap == "" {
		cfg.ModelCheap = string(anthropic.ModelClaudeSonnet4_6)
	}
	if cfg.MaxIterations == 0 {
		cfg.MaxIterations = 12
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Minute
	}
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	} else {
		// The SDK reads ANTHROPIC_BASE_URL from the environment with os.LookupEnv,
		// which treats a present-but-empty value (e.g. a docker-compose variable
		// that resolves to "") as a real setting and points the client at an empty
		// base URL — breaking every request. Clear it so the SDK falls back to its
		// production default endpoint.
		os.Unsetenv("ANTHROPIC_BASE_URL")
	}
	return &Analyzer{
		client: anthropic.NewClient(opts...),
		cfg:    cfg,
		http:   &http.Client{Timeout: 20 * time.Second},
	}, nil
}

// ExpensiveModel and CheapModel expose the configured model IDs.
func (a *Analyzer) ExpensiveModel() string { return a.cfg.Model }
func (a *Analyzer) CheapModel() string     { return a.cfg.ModelCheap }

// Analyze runs the agent against one event with the given model (empty uses the
// expensive default) and returns its writeup.
func (a *Analyzer) Analyze(ctx context.Context, ev Event, model string) (Analysis, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	if model == "" {
		model = a.cfg.Model
	}

	tools, err := a.tools(ev)
	if err != nil {
		return Analysis{}, err
	}

	// ttl1h keeps the cached prefix warm across investigations spaced minutes-to-
	// an-hour apart (the coalescer makes them sparse), not just within one run.
	ttl1h := anthropic.BetaCacheControlEphemeralParam{TTL: anthropic.BetaCacheControlEphemeralTTLTTL1h}

	runner := a.client.Beta.Messages.NewToolRunner(tools, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: anthropic.BetaMessageNewParams{
			Model:     anthropic.Model(model),
			MaxTokens: 6000,
			// Let Claude decide how much to reason; investigation benefits from it.
			Thinking: anthropic.BetaThinkingConfigParamUnion{
				OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{},
			},
			// Two cache breakpoints. The system block caches the (byte-stable)
			// tool list + system prompt — only the per-event user message varies.
			System: []anthropic.BetaTextBlockParam{{
				Text:         systemPrompt,
				CacheControl: ttl1h,
			}},
			// The agentic loop re-sends the whole growing conversation every
			// iteration; the top-level cache_control auto-places a breakpoint on
			// the last block each turn, so iteration N reads iterations 1..N-1
			// (incl. large tool results) from cache instead of reprocessing them.
			CacheControl: ttl1h,
			Messages: []anthropic.BetaMessageParam{
				anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(userPrompt(ev))),
			},
		},
		MaxIterations: a.cfg.MaxIterations,
	})

	msg, err := runner.RunToCompletion(ctx)
	if err != nil {
		return Analysis{}, fmt.Errorf("agent run: %w", err)
	}

	// Log cache effectiveness so the hit rate is observable: cache_read should
	// dominate input on a multi-iteration run; if it stays 0, a prefix
	// invalidator is at work.
	usage := Usage{
		Input:      msg.Usage.InputTokens,
		CacheRead:  msg.Usage.CacheReadInputTokens,
		CacheWrite: msg.Usage.CacheCreationInputTokens,
		Output:     msg.Usage.OutputTokens,
	}
	log.Printf("ai diagnosis #%d %s [model=%s]: tokens input=%d cache_read=%d cache_write=%d output=%d",
		ev.ID, ev.Target, model, usage.Input, usage.CacheRead, usage.CacheWrite, usage.Output)

	var sb strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return Analysis{}, fmt.Errorf("agent produced no text output")
	}
	return Analysis{
		EventID:  ev.ID,
		Headline: headline(text),
		Text:     stripMarkdown(text),
		Model:    model,
		Usage:    usage,
	}, nil
}

// Judge asks the cheap model whether two analyses identify the same primary root
// cause and location. It is a single, tool-less, low-token call used to decide
// whether the cheaper model was "good enough" for a class of problem.
func (a *Analyzer) Judge(ctx context.Context, expensive, cheap Analysis) (bool, error) {
	jctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	prompt := "Two network-diagnostics analyses were written for the SAME latency event. " +
		"Do they identify the same PRIMARY root cause and the same location in the path " +
		"(e.g. both say local bufferbloat, or both blame the same ISP hop)? Minor wording or " +
		"extra detail doesn't matter — only whether an operator would take the same action. " +
		"Answer with exactly one word: YES or NO.\n\n" +
		"--- Analysis A ---\n" + expensive.Text + "\n\n--- Analysis B ---\n" + cheap.Text

	msg, err := a.client.Messages.New(jctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.cfg.ModelCheap),
		MaxTokens: 16,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return false, err
	}
	var out strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(t.Text)
		}
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(out.String())), "yes"), nil
}

// stripMarkdown converts the model's output to clean plain text, because
// Grafana annotations (and the annotation-list panel) render as plain text — so
// Markdown like **bold**, `code`, headings, and --- rules would otherwise show
// up as literal noise.
func stripMarkdown(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	s = strings.ReplaceAll(s, "`", "")
	out := make([]string, 0, 16)
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if isRule(t) { // drop horizontal rules like --- or ***
			continue
		}
		// Strip a leading heading/quote/bullet marker but keep the content.
		t = strings.TrimLeft(t, "#> ")
		out = append(out, t)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// headline picks a compact, plain-text title: the first substantive line,
// skipping Markdown rules, blank lines, and filler preambles like "Here is the
// diagnosis:" so the dashboard title is the actual root cause.
func headline(s string) string {
	for _, line := range strings.Split(stripMarkdown(s), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || isPreamble(line) {
			continue
		}
		return line
	}
	return strings.TrimSpace(stripMarkdown(s))
}

// isRule reports whether a line is a Markdown horizontal rule.
func isRule(t string) bool {
	if len(t) < 3 {
		return false
	}
	for _, r := range t {
		if r != '-' && r != '*' && r != '_' && r != '=' {
			return false
		}
	}
	return true
}

// isPreamble reports whether a line is filler that shouldn't be the headline.
func isPreamble(line string) bool {
	if strings.HasSuffix(line, ":") {
		return true
	}
	l := strings.ToLower(line)
	for _, p := range []string{
		"here is", "here's", "the picture is", "summary", "in summary",
		"full diagnosis", "based on", "to summarize", "diagnosis:", "analysis:",
	} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}
