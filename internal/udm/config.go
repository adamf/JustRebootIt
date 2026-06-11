package udm

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// networkConfResp is the /rest/networkconf response: the gateway's network
// configurations (LAN, WAN, VLANs). WAN entries carry the settings most
// relevant to latency tuning — Smart Queues / QoS rate limits, WAN type, MTU.
type networkConfResp struct {
	Data []map[string]any `json:"data"`
}

// sensitiveKey matches configuration keys whose values are secrets and must
// never be exported, sent to the LLM, or written into a Grafana annotation.
// UniFi prefixes hidden/sensitive fields with "x_".
var sensitiveKey = regexp.MustCompile(`(?i)(password|passwd|psk|secret|token|^x_)`)

// noisyKey matches stable-but-uninteresting identifiers we drop to keep the
// config (and its diffs) readable.
var noisyKey = map[string]bool{
	"_id": true, "site_id": true, "attr_hidden_id": true, "attr_no_edit": true,
}

// WANConfig returns the gateway's WAN network configuration objects, with
// secrets redacted and noise removed, in a stable order. These are what the AI
// reads to learn whether Smart Queues is already on (and at what rate) before
// recommending a change, and what the change-watcher diffs.
func (c *Client) WANConfig(ctx context.Context) ([]map[string]any, error) {
	var resp networkConfResp
	if err := c.getJSON(ctx, c.path("/rest/networkconf"), &resp); err != nil {
		return nil, err
	}
	var wans []map[string]any
	for _, n := range resp.Data {
		if isWAN(n) {
			wans = append(wans, sanitize(n))
		}
	}
	sort.Slice(wans, func(i, j int) bool { return netName(wans[i]) < netName(wans[j]) })
	return wans, nil
}

// isWAN reports whether a network config object describes a WAN uplink.
func isWAN(n map[string]any) bool {
	if p, ok := n["purpose"].(string); ok && strings.Contains(strings.ToLower(p), "wan") {
		return true
	}
	_, hasGroup := n["wan_networkgroup"]
	return hasGroup
}

// netName returns a stable label for a network config object.
func netName(n map[string]any) string {
	if s, ok := n["name"].(string); ok && s != "" {
		return s
	}
	if s, ok := n["purpose"].(string); ok && s != "" {
		return s
	}
	return "wan"
}

// sanitize copies a config object, redacting sensitive values and dropping
// noisy identifiers.
func sanitize(n map[string]any) map[string]any {
	out := make(map[string]any, len(n))
	for k, v := range n {
		if noisyKey[k] {
			continue
		}
		if sensitiveKey.MatchString(k) {
			out[k] = "***redacted***"
			continue
		}
		out[k] = v
	}
	return out
}

// Flatten turns the WAN config objects into a flat "name.key" -> value map for
// cheap change detection.
func Flatten(wans []map[string]any) map[string]string {
	flat := make(map[string]string)
	for _, n := range wans {
		name := netName(n)
		for k, v := range n {
			flat[name+"."+k] = fmt.Sprint(v)
		}
	}
	return flat
}

// DiffConfig compares two flattened configs and returns human-readable lines
// describing what changed ("name.key: old → new"). An empty old map (first
// observation) yields no diff, so startup doesn't look like a change.
func DiffConfig(old, current map[string]string) []string {
	if len(old) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var lines []string
	for k, nv := range current {
		seen[k] = true
		if ov, ok := old[k]; !ok {
			lines = append(lines, fmt.Sprintf("%s: (added) → %s", k, nv))
		} else if ov != nv {
			lines = append(lines, fmt.Sprintf("%s: %s → %s", k, ov, nv))
		}
	}
	for k, ov := range old {
		if !seen[k] {
			lines = append(lines, fmt.Sprintf("%s: %s → (removed)", k, ov))
		}
	}
	sort.Strings(lines)
	return lines
}
