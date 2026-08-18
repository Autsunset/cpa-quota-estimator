package main

import "strings"

const (
	mainQuotaScope  = "main"
	sparkQuotaScope = "spark"
)

func quotaScopeForUsage(model, alias string) string {
	normalized := strings.ToLower(strings.TrimSpace(model + " " + alias))
	if strings.Contains(normalized, "codex-spark") {
		return sparkQuotaScope
	}
	return mainQuotaScope
}

func eventQuotaScope(e event) string {
	if strings.TrimSpace(e.QuotaScope) == "" {
		return quotaScopeForUsage(e.Model, e.Alias)
	}
	return strings.TrimSpace(e.QuotaScope)
}
