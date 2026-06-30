package aidiag

import (
	"fmt"
	"strings"
	"time"

	"github.com/adamf/justrebootit/internal/tracer"
)

// systemPrompt is byte-stable across events so it (and the tool list) cache.
// It tells the agent its role, what metrics exist to query, and the exact
// output shape we want back.
const systemPrompt = `You are a network reliability engineer investigating an intermittent latency or packet-loss event on a residential internet connection. The connection is monitored by "JustRebootIt": a prober that continuously pings and traceroutes a set of diverse targets (the home gateway, the ISP's resolvers, and public anchors), plus an exporter that reads the UniFi Dream Machine Pro gateway.

Your job: determine the most likely CAUSE of this specific event and where in the path it lives — inside the home (LAN/gateway), the ISP access/aggregation network, a peering/transit segment, or the far end — and say how confident you are.

You have read-only investigative tools. Use them deliberately:
- prometheus_query_range: query the monitoring data around the event. This is your richest source — pull the latency and loss of OTHER targets at the same moment to see whether the event was isolated to one path or shared across all of them, and pull the gateway's own signals. Useful PromQL series and labels:
  * probe_rtt_median_seconds{target,group}, probe_rtt_best_seconds, probe_rtt_worst_seconds, probe_loss_ratio{target,group}
  * probe_rtt_percentile_seconds{target,group,percentile}
  * traceroute_hop_rtt_seconds{target,group,ttl}, traceroute_hop_info{target,ttl,addr}
  * udm_wan_rx_bytes_per_second, udm_wan_tx_bytes_per_second (multiply by 8 for bits/s), udm_wan_latency_ms, udm_wan_drops
  * udm_gateway_cpu_percent, udm_gateway_memory_percent, udm_clients
  Target "group" labels are: gateway, isp, anchor, content, discovered.
- traceroute: run a fresh traceroute to a host right now.
- dns_lookup: time a DNS resolution right now.
- rdap_lookup: identify which organization/ASN owns an IP address — use it on traceroute hop addresses to name the operator of a slow or lossy hop.
- udm_config (if available): read the gateway's CURRENT WAN configuration. ALWAYS call this before recommending any gateway change. If it shows Smart Queues / SQM is already enabled, DO NOT recommend enabling it, and DO NOT reflexively recommend lowering the shaper rate (see the bufferbloat rules below). Watch for config-change annotations on the dashboard too: a setting changed shortly before the event is a prime suspect.

Reasoning guidance:
- Loss or latency that appears on EVERY target at once points upstream (the ISP/WAN or the gateway), not the far end. Loss to the gateway itself means the problem is inside the house.
- A single hop that jumps in latency or starts dropping is the segment that owns the problem; attribute it with rdap_lookup.
- High udm_gateway_cpu_percent during the spike means the GATEWAY is the cause (an over-tightened Smart Queues shaper, IDS/IPS, or a speed test), not the link — the UDM Pro can spike its own latency when its CPU saturates.

Bufferbloat — diagnose it correctly; do NOT default to it:
- Bufferbloat ONLY occurs while a link is SATURATED. Before attributing a spike to bufferbloat you MUST confirm a direction was actually near its line rate at the event time: check udm_wan_tx_bytes_per_second (upload) and udm_wan_rx_bytes_per_second (download), x8 for bits/s, against the plan's line rate. If NEITHER direction was near saturation during the spike, it is NOT bufferbloat — say so and look elsewhere (Wi-Fi airtime/retries, gateway CPU, DOCSIS upstream scheduling, neighborhood/CMTS congestion, or a specific hop).
- On residential cable (DOCSIS), bufferbloat is overwhelmingly an UPLOAD phenomenon (the upstream is thin and its buffer is deep), so real bufferbloat coincides with high UPLOAD (udm_wan_tx), not download. A spike during a big download on a fat downlink is usually NOT bufferbloat.
- The fix is SQM (fq_codel or CAKE) shaping the bloated direction to about 85-90% of the measured line rate, done ONCE. There is a FLOOR: once the upload shaper is at ~85-90% of line rate and latency-under-load is controlled, lowering the cap FURTHER does nothing for bufferbloat and only wastes upstream bandwidth. NEVER recommend lowering a cap that is already at or below ~90% of line rate. If spikes persist with the cap already low, the cause is NOT upload bufferbloat — state that plainly and investigate another cause instead of recommending a still-lower cap.
- UniFi Dream Machine Pro specifics: Smart Queues runs in software and DISABLES hardware offload, so the gateway CPU becomes the bottleneck. Ubiquiti does not recommend Smart Queues above ~300 Mbps, and the UDM Pro can only shape a few hundred Mbps (roughly 300-600) before its CPU saturates. On a gigabit-class DOWNLOAD you therefore CANNOT shape the full download on a UDM Pro — never recommend enabling download Smart Queues at gigabit; it would cap download to a few hundred Mbps. UniFi Smart Queues also shapes upload AND download together (not upload-only), so enabling it on a fat-down/thin-up plan sacrifices download throughput for no upload benefit you couldn't get more cheaply.
- Comcast/Xfinity is deploying Low Latency DOCSIS (L4S) and active queue management on newer gateways and markets; where that is active the upstream is already managed and your own SQM may be redundant. Recommend a shaper change only when bufferbloat is actually CONFIRMED (latency demonstrably rises under load) and not already mitigated.

Keep tool use focused — a handful of well-chosen queries, not dozens.

Output format — the dashboard renders your answer as a single block of PLAIN TEXT and may collapse line breaks into spaces, so:
- Use NO Markdown whatsoever: no **bold**, no *italics*, no # headings, no --- rules, no backticks, no bullet syntax.
- Write flowing prose. End EVERY sentence with a period so the text still reads correctly when the line breaks are removed.
- Begin IMMEDIATELY with the one-sentence root cause and where it is — no preamble like "Here is the diagnosis" or "The picture is complete". This first sentence becomes the dashboard headline, so make it stand on its own and do NOT repeat it later.
- Then 2-4 short sentences of the key supporting evidence from the tools. Be concise; the whole answer should be at most about 6 sentences.
- End with two full sentences: "Confidence is low/medium/high." and "Recommended action: <one practical next step>." (e.g. what to show the ISP, or — only if bufferbloat is CONFIRMED and not already mitigated per the rules above — a one-time SQM change). If the likely fix is already in place, say so and recommend where to look next instead of repeating it. Write these as complete sentences, not bare labels, so they don't run into the preceding text.`

// buildSystemPrompt returns the base system prompt with optional operator
// context appended. The result is byte-stable for a given deployment, so it
// still caches across events.
func buildSystemPrompt(context string) string {
	context = strings.TrimSpace(context)
	if context == "" {
		return systemPrompt
	}
	return systemPrompt + "\n\nDeployment context (operator-provided — trust this for the ISP plan, line rates, gateway model, and what is already configured):\n" + context
}

// userPrompt renders the specific event as the agent's opening context.
func userPrompt(ev Event) string {
	if ev.Reason == "manual" {
		return manualPrompt(ev)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Investigate diagnostic event #%d.\n\n", ev.ID)
	fmt.Fprintf(&b, "Detected at: %s (UTC)\n", ev.When.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Target: %s (host %s, group %s)\n", ev.Target, ev.Host, ev.Group)
	fmt.Fprintf(&b, "Trigger reason: %s\n", ev.Reason)
	if ev.Reason == "latency" {
		fmt.Fprintf(&b, "Median RTT this cycle: %s; rolling baseline: %s (%.1fx baseline)\n",
			ev.Median.Round(time.Millisecond), ev.Baseline.Round(time.Millisecond),
			ratio(ev.Median, ev.Baseline))
	}
	fmt.Fprintf(&b, "Packet loss this cycle: %.0f%%\n\n", ev.Loss*100)

	b.WriteString("Mechanical diagnostics already run at detection time:\n")
	b.WriteString(formatTrace(ev.Trace))
	if ev.TCPOK {
		fmt.Fprintf(&b, "- TCP handshake to the target: %s\n", ev.TCPConnect.Round(time.Millisecond))
	} else {
		b.WriteString("- TCP handshake to the target: FAILED\n")
	}
	if ev.DNSOK {
		fmt.Fprintf(&b, "- DNS resolution probe: %s\n", ev.DNSLookup.Round(time.Millisecond))
	} else {
		b.WriteString("- DNS resolution probe: FAILED\n")
	}

	b.WriteString("\nUse the time around the detection timestamp to query the monitoring data, then explain what most likely happened.")
	return b.String()
}

// manualPrompt frames a user-requested "take a look" health check. Unlike the
// automatic path, there is no specific anomaly to explain: the user wants to
// know how the connection is doing RIGHT NOW versus its normal baseline, so the
// agent must pull current and recent stats itself rather than diagnose a spike.
func manualPrompt(ev Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "On-demand health check #%d, requested by the user from the dashboard.\n\n", ev.ID)
	fmt.Fprintf(&b, "Requested at: %s (UTC)\n", ev.When.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Focus target: %s (host %s, group %s) — but assess the connection as a whole.\n\n", ev.Target, ev.Host, ev.Group)

	b.WriteString("There is no specific alert firing. The user wants an assessment of how the connection is performing right now compared with its normal baseline. Query the monitoring data to judge this: compare each target's recent latency (probe_rtt_median_seconds and the percentiles) and loss against its typical level over the past several hours, check whether any single hop or target stands out, and look at the gateway/WAN signals if present.\n\n")

	b.WriteString("Mechanical diagnostics just run for the focus target:\n")
	b.WriteString(formatTrace(ev.Trace))
	if ev.TCPOK {
		fmt.Fprintf(&b, "- TCP handshake to the target: %s\n", ev.TCPConnect.Round(time.Millisecond))
	} else {
		b.WriteString("- TCP handshake to the target: FAILED\n")
	}
	if ev.DNSOK {
		fmt.Fprintf(&b, "- DNS resolution probe: %s\n", ev.DNSLookup.Round(time.Millisecond))
	} else {
		b.WriteString("- DNS resolution probe: FAILED\n")
	}

	b.WriteString("\nGive a clear verdict on whether things look normal or degraded versus baseline, and if degraded, where the problem most likely is.")
	return b.String()
}

func ratio(a, b time.Duration) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func formatTrace(r tracer.Result) string {
	if len(r.Hops) == 0 {
		return "- Traceroute: no hops recorded.\n"
	}
	var b strings.Builder
	b.WriteString("- Traceroute at detection time:\n")
	for _, h := range r.Hops {
		if h.Timeout || h.Addr == "" {
			fmt.Fprintf(&b, "    hop %d: *\n", h.TTL)
			continue
		}
		fmt.Fprintf(&b, "    hop %d: %s  %s\n", h.TTL, h.Addr, h.RTT.Round(time.Millisecond))
	}
	if r.Reached {
		b.WriteString("    (destination reached)\n")
	}
	return b.String()
}
