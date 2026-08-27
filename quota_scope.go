package main

import "strings"

const (
	mainQuotaScope   = "main"
	weeklyQuotaScope = "weekly"
	sparkQuotaScope  = "spark"

	fiveHourWindowMinutes = int64(300)
	fiveHourWindowSlack   = int64(5)
	weeklyWindowMinutes   = int64(10080)
	weeklyWindowSlack     = int64(60)
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

func isFiveHourWindow(windowMinutes int64) bool {
	return windowMinutes >= fiveHourWindowMinutes-fiveHourWindowSlack && windowMinutes <= fiveHourWindowMinutes+fiveHourWindowSlack
}

func isWeeklyWindow(windowMinutes int64) bool {
	return windowMinutes >= weeklyWindowMinutes-weeklyWindowSlack && windowMinutes <= weeklyWindowMinutes+weeklyWindowSlack
}
