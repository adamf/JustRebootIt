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
	"net/http"
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
	if !cfg.Enabled || cfg.APIKey == "" {
		return nil, nil
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
	return &Analyzer{
		client: anthropic.NewClient(option.WithAPIKey(cfg.APIKey)),
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

	runner := a.client.Beta.Messages.NewToolRunner(tools, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: anthropic.BetaMessageNewParams{
			Model:     anthropic.Model(model),
			MaxTokens: 6000,
			// Let Claude decide how much to reason; investigation benefits from it.
			Thinking: anthropic.BetaThinkingConfigParamUnion{
				OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{},
			},
			// The system prompt and tool list are byte-stable across events, so
			// cache them; only the per-event user message varies.
			System: []anthropic.BetaTextBlockParam{{
				Text:         systemPrompt,
				CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			}},
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
		Headline: firstLine(text),
		Text:     text,
		Model:    model,
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

// firstLine returns the first non-empty line, stripped of a leading markdown
// heading marker, for use as a compact annotation title.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "# ")
		if line != "" {
			return line
		}
	}
	return s
}
