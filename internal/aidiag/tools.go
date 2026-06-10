package aidiag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/adamf/justrebootit/internal/tracer"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// maxToolResultChars caps how much a single tool result can return, to keep the
// agent's context (and token spend) bounded.
const maxToolResultChars = 6000

// tools builds the investigative tool set, closing over the event so handlers
// know its target and timestamp.
func (a *Analyzer) tools(ev Event) ([]anthropic.BetaTool, error) {
	var tools []anthropic.BetaTool
	add := func(t anthropic.BetaTool, err error) error {
		if err != nil {
			return err
		}
		tools = append(tools, t)
		return nil
	}

	if err := add(toolrunner.NewBetaToolFromJSONSchema(
		"prometheus_query_range",
		"Run a Prometheus range query against the JustRebootIt monitoring data. Returns the matching time series as JSON. Use this to see how latency, loss, traceroute hops, and gateway/WAN signals behaved around the event. The time window is centered on the event automatically; you choose how far back to look.",
		a.queryRange(ev),
	)); err != nil {
		return nil, err
	}

	if err := add(toolrunner.NewBetaToolFromJSONSchema(
		"traceroute",
		"Run a fresh ICMP traceroute to a host right now and return the per-hop addresses and round-trip times.",
		a.traceroute,
	)); err != nil {
		return nil, err
	}

	if err := add(toolrunner.NewBetaToolFromJSONSchema(
		"dns_lookup",
		"Resolve a hostname now and report how long resolution took plus the addresses returned.",
		a.dnsLookup,
	)); err != nil {
		return nil, err
	}

	if err := add(toolrunner.NewBetaToolFromJSONSchema(
		"rdap_lookup",
		"Look up the organization, network name, ASN, and country that owns an IP address (via RDAP). Use it on a traceroute hop address to attribute that segment to an operator such as the ISP or a transit provider.",
		a.rdapLookup,
	)); err != nil {
		return nil, err
	}

	return tools, nil
}

func textResult(s string) anthropic.BetaToolResultBlockParamContentUnion {
	if len(s) > maxToolResultChars {
		s = s[:maxToolResultChars] + "\n…(truncated)"
	}
	return anthropic.BetaToolResultBlockParamContentUnion{
		OfText: &anthropic.BetaTextBlockParam{Text: s},
	}
}

type queryRangeInput struct {
	Query           string `json:"query" jsonschema:"required,description=The PromQL expression to evaluate (e.g. probe_rtt_median_seconds or probe_loss_ratio{group=\"isp\"})."`
	LookbackMinutes int    `json:"lookback_minutes" jsonschema:"description=How many minutes before the event to start the window. Defaults to 15."`
	StepSeconds     int    `json:"step_seconds" jsonschema:"description=Resolution of the returned series in seconds. Defaults to 15."`
}

func (a *Analyzer) queryRange(ev Event) func(context.Context, queryRangeInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	return func(ctx context.Context, in queryRangeInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
		if a.cfg.PrometheusURL == "" {
			return textResult("prometheus_query_range is unavailable: no Prometheus URL is configured."), nil
		}
		lookback := in.LookbackMinutes
		if lookback <= 0 {
			lookback = 15
		}
		step := in.StepSeconds
		if step <= 0 {
			step = 15
		}
		start := ev.When.Add(-time.Duration(lookback) * time.Minute)
		end := ev.When.Add(1 * time.Minute)

		q := url.Values{}
		q.Set("query", in.Query)
		q.Set("start", strconv.FormatInt(start.Unix(), 10))
		q.Set("end", strconv.FormatInt(end.Unix(), 10))
		q.Set("step", strconv.Itoa(step)+"s")
		endpoint := strings.TrimRight(a.cfg.PrometheusURL, "/") + "/api/v1/query_range?" + q.Encode()

		body, err := a.get(ctx, endpoint)
		if err != nil {
			return textResult("query failed: " + err.Error()), nil
		}
		return textResult(body), nil
	}
}

type traceInput struct {
	Host string `json:"host" jsonschema:"required,description=Hostname or IP to traceroute."`
}

func (a *Analyzer) traceroute(ctx context.Context, in traceInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	tr := tracer.New(a.cfg.TraceMaxHops, a.cfg.TraceTimeout, a.cfg.Privileged)
	res, err := tr.Trace(ctx, in.Host)
	if err != nil {
		return textResult("traceroute failed: " + err.Error()), nil
	}
	return textResult(formatTrace(res)), nil
}

type dnsInput struct {
	Name string `json:"name" jsonschema:"required,description=Hostname to resolve."`
}

func (a *Analyzer) dnsLookup(ctx context.Context, in dnsInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	var r net.Resolver
	start := time.Now()
	addrs, err := r.LookupHost(ctx, in.Name)
	elapsed := time.Since(start)
	if err != nil {
		return textResult(fmt.Sprintf("DNS lookup of %s failed after %s: %v", in.Name, elapsed.Round(time.Millisecond), err)), nil
	}
	return textResult(fmt.Sprintf("Resolved %s in %s: %s", in.Name, elapsed.Round(time.Millisecond), strings.Join(addrs, ", "))), nil
}

type rdapInput struct {
	IP string `json:"ip" jsonschema:"required,description=The IP address to look up."`
}

// rdapResponse captures the handful of RDAP fields worth summarizing.
type rdapResponse struct {
	Name     string `json:"name"`
	Handle   string `json:"handle"`
	Country  string `json:"country"`
	Type     string `json:"type"`
	Entities []struct {
		Roles      []string `json:"roles"`
		VCardArray []any    `json:"vcardArray"`
	} `json:"entities"`
}

func (a *Analyzer) rdapLookup(ctx context.Context, in rdapInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	if net.ParseIP(in.IP) == nil {
		return textResult("not a valid IP address: " + in.IP), nil
	}
	body, err := a.get(ctx, "https://rdap.org/ip/"+url.PathEscape(in.IP))
	if err != nil {
		return textResult("rdap lookup failed: " + err.Error()), nil
	}
	var r rdapResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return textResult("rdap returned unparseable data"), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "RDAP for %s:\n", in.IP)
	if r.Name != "" {
		fmt.Fprintf(&b, "  network name: %s\n", r.Name)
	}
	if r.Handle != "" {
		fmt.Fprintf(&b, "  handle/range: %s\n", r.Handle)
	}
	if r.Country != "" {
		fmt.Fprintf(&b, "  country: %s\n", r.Country)
	}
	if org := rdapOrg(r); org != "" {
		fmt.Fprintf(&b, "  organization: %s\n", org)
	}
	return textResult(b.String()), nil
}

// rdapOrg digs the organization/full name out of the first entity's vCard.
func rdapOrg(r rdapResponse) string {
	for _, e := range r.Entities {
		// vcardArray is ["vcard", [ [name, {}, type, value], ... ]]
		if len(e.VCardArray) < 2 {
			continue
		}
		props, ok := e.VCardArray[1].([]any)
		if !ok {
			continue
		}
		for _, p := range props {
			row, ok := p.([]any)
			if !ok || len(row) < 4 {
				continue
			}
			if key, _ := row[0].(string); key == "fn" || key == "org" {
				if v, ok := row[3].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// get performs a bounded GET and returns the response body as a string.
func (a *Analyzer) get(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}
