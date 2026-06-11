package udm

import (
	"strings"
	"testing"
)

func TestIsWAN(t *testing.T) {
	if !isWAN(map[string]any{"purpose": "wan"}) {
		t.Error("purpose=wan should be WAN")
	}
	if !isWAN(map[string]any{"purpose": "WAN2"}) {
		t.Error("purpose=WAN2 should be WAN")
	}
	if !isWAN(map[string]any{"wan_networkgroup": "WAN"}) {
		t.Error("wan_networkgroup present should be WAN")
	}
	if isWAN(map[string]any{"purpose": "corporate"}) {
		t.Error("a LAN should not be WAN")
	}
}

func TestSanitizeRedactsSecretsAndDropsNoise(t *testing.T) {
	in := map[string]any{
		"name":                   "WAN",
		"purpose":                "wan",
		"wan_smartq_enabled":     true,
		"wan_password":           "hunter2",
		"x_ipsec_pre_shared_key": "topsecret",
		"wppsk":                  "wifi-pw",
		"_id":                    "abc123",
		"site_id":                "def456",
	}
	out := sanitize(in)

	if _, ok := out["_id"]; ok {
		t.Error("_id should be dropped")
	}
	if _, ok := out["site_id"]; ok {
		t.Error("site_id should be dropped")
	}
	if out["wan_password"] != "***redacted***" {
		t.Errorf("wan_password not redacted: %v", out["wan_password"])
	}
	if out["x_ipsec_pre_shared_key"] != "***redacted***" {
		t.Errorf("x_ field not redacted: %v", out["x_ipsec_pre_shared_key"])
	}
	if out["wppsk"] != "***redacted***" {
		t.Errorf("psk field not redacted: %v", out["wppsk"])
	}
	if out["wan_smartq_enabled"] != true {
		t.Errorf("non-secret field should pass through, got %v", out["wan_smartq_enabled"])
	}
	if out["name"] != "WAN" {
		t.Errorf("name should pass through, got %v", out["name"])
	}
}

func TestDiffConfig(t *testing.T) {
	// First observation: no diff even though current is non-empty.
	if d := DiffConfig(nil, map[string]string{"WAN.smartq": "false"}); len(d) != 0 {
		t.Errorf("first observation should yield no diff, got %v", d)
	}

	old := map[string]string{"WAN.wan_smartq_enabled": "false", "WAN.gone": "x"}
	cur := map[string]string{"WAN.wan_smartq_enabled": "true", "WAN.added": "y"}
	d := DiffConfig(old, cur)
	joined := strings.Join(d, "\n")
	if !strings.Contains(joined, "WAN.wan_smartq_enabled: false → true") {
		t.Errorf("missing changed line:\n%s", joined)
	}
	if !strings.Contains(joined, "WAN.added: (added) → y") {
		t.Errorf("missing added line:\n%s", joined)
	}
	if !strings.Contains(joined, "WAN.gone: x → (removed)") {
		t.Errorf("missing removed line:\n%s", joined)
	}
}

func TestFlatten(t *testing.T) {
	wans := []map[string]any{
		{"name": "WAN", "wan_smartq_enabled": true, "wan_smartq_down_rate": 900000},
	}
	flat := Flatten(wans)
	if flat["WAN.wan_smartq_enabled"] != "true" {
		t.Errorf("flatten bool: %v", flat["WAN.wan_smartq_enabled"])
	}
	if flat["WAN.wan_smartq_down_rate"] != "900000" {
		t.Errorf("flatten int: %v", flat["WAN.wan_smartq_down_rate"])
	}
}
