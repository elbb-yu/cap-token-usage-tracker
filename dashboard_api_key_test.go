package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestAPIKeyDashboardIsFullModeOnly(t *testing.T) {
	for _, forbidden := range []string{
		`id="apiKeyControls"`,
		`id="apiKeySecurityWarning"`,
		`id="apiKeyLabelDialog"`,
		"selectedAPIKeyHash",
		"params.set('api_key_hash'",
		`"apiKey.defaultSecretWarning"`,
		`"table.apiKey"`,
	} {
		if strings.Contains(dashboardHTML, forbidden) {
			t.Fatalf("ordinary dashboard contains API-key contract %q", forbidden)
		}
	}
	for _, required := range []string{
		`id="apiKeyControls"`,
		`id="apiKeySecurityWarning"`,
		`id="apiKeyFilterSelect"`,
		`id="apiKeyLabelDialog"`,
		"params.set('api_key_hash',selectedAPIKeyHash)",
		"apiKeyLabels[hash]||key||t('apiKey.plaintextUnavailable')",
		"apiKeyOptions=next;if(selectedAPIKeyHash&&!apiKeyOptionForHash(selectedAPIKeyHash)){selectedAPIKeyHash='';",
		"initializeAPIKeyFullMode(payload)",
		"api_key_uses_default_secret",
		"String(url).indexOf(statsURL)===0",
		"String(url).indexOf(requestsURL)===0",
		"String(url).indexOf(costsURL)===0",
		"'X-Full-Mode-Session':fullModeSession",
	} {
		if !strings.Contains(fullDashboardHTML, required) {
			t.Fatalf("full dashboard missing API-key contract %q", required)
		}
	}
	if strings.Contains(fullDashboardHTML, "/*FULL_MODE_APIKEY_") || strings.Contains(dashboardHTML, "/*FULL_MODE_APIKEY_") {
		t.Fatal("generated dashboard contains unresolved API-key placeholder")
	}
}

func TestAPIKeyLocaleCatalog(t *testing.T) {
	required := []string{
		"table.apiKey", "apiKey.filter", "apiKey.all", "apiKey.editLabel",
		"apiKey.label", "apiKey.labelPlaceholder", "apiKey.labelHint",
		"apiKey.saveLabel", "apiKey.deleteLabel", "apiKey.trackingDisabled",
		"apiKey.plaintextUnavailable", "apiKey.defaultSecretWarning",
		"apiKey.labelTooLong", "apiKey.saveFailed",
	}
	for _, code := range []string{"en", "zh-CN", "zh-TW", "ru"} {
		data, err := localeFS.ReadFile("locales/" + code + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var catalog map[string]string
		if err := json.Unmarshal(data, &catalog); err != nil {
			t.Fatalf("locale %s: %v", code, err)
		}
		for _, key := range required {
			if catalog[key] == "" {
				t.Fatalf("locale %s missing %q", code, key)
			}
		}
		if !strings.Contains(catalog["apiKey.defaultSecretWarning"], defaultAPIKeySecret) || !strings.Contains(catalog["apiKey.defaultSecretWarning"], "32") {
			t.Fatalf("locale %s warning does not explain the default and minimum strength", code)
		}
	}
}

func TestGeneratedDashboardJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}
	for name, html := range map[string]string{"ordinary": dashboardHTML, "full": fullDashboardHTML} {
		t.Run(name, func(t *testing.T) {
			remaining := html
			for index := 0; ; index++ {
				start := strings.Index(remaining, "<script")
				if start < 0 {
					break
				}
				openEnd := strings.Index(remaining[start:], ">")
				if openEnd < 0 {
					t.Fatal("unterminated script tag")
				}
				openEnd += start
				closeAt := strings.Index(remaining[openEnd+1:], "</script>")
				if closeAt < 0 {
					t.Fatal("unterminated script body")
				}
				closeAt += openEnd + 1
				script := remaining[openEnd+1 : closeAt]
				command := exec.Command(node, "--check", "-")
				command.Stdin = strings.NewReader(script)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("script %d syntax: %v\n%s", index, err, output)
				}
				remaining = remaining[closeAt+len("</script>"):]
			}
		})
	}
}
