package main

import "strings"

const (
	mainQuotaScope        = "main"
	weeklyQuotaScope      = "weekly"
	sparkQuotaScope       = "spark"
	sparkWeeklyQuotaScope = "spark_weekly"
	imageQuotaScope       = "image"

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

func quotaScopeForObservation(model, alias, activeLimit string, windowMinutes int64) string {
	activeLimit = strings.ToLower(strings.TrimSpace(activeLimit))
	if activeLimit == "imagegen" || strings.HasPrefix(activeLimit, "imagegen_") {
		return imageQuotaScope
	}
	// Older stores may not retain headers. Only the daily image window is
	// sufficient fallback evidence; historical weekly image usage stays main.
	if activeLimit == "" && windowMinutes == 1440 {
		for _, name := range []string{model, alias} {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "gpt-image-") {
				return imageQuotaScope
			}
		}
	}
	return quotaScopeForUsage(model, alias)
}

func eventQuotaScope(e event) string {
	if strings.TrimSpace(e.QuotaScope) == "" {
		return quotaScopeForObservation(e.Model, e.Alias, "", e.WindowMinutes)
	}
	return strings.TrimSpace(e.QuotaScope)
}

func isFiveHourWindow(windowMinutes int64) bool {
	return windowMinutes >= fiveHourWindowMinutes-fiveHourWindowSlack && windowMinutes <= fiveHourWindowMinutes+fiveHourWindowSlack
}

func isWeeklyWindow(windowMinutes int64) bool {
	return windowMinutes >= weeklyWindowMinutes-weeklyWindowSlack && windowMinutes <= weeklyWindowMinutes+weeklyWindowSlack
}
