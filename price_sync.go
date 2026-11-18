package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const codexPricingURL = "https://learn.chatgpt.com/docs/pricing"
const priceSyncCooldown = time.Hour

type codexCreditRates struct {
	Input, Cached, Output, Fast float64
}

func (p price) codexCreditPrice() (price, bool) {
	if p.Codex.Input <= 0 {
		return officialCodexCreditPrice(p.Model)
	}
	return price{
		Model: p.Model, Source: codexPricingURL, Input: p.Codex.Input, CacheRead: p.Codex.Cached, Output: p.Codex.Output,
		LongInput: p.Codex.Input, LongRead: p.Codex.Cached, LongOutput: p.Codex.Output,
	}, true
}

func (p price) codexFastMultiplier() float64 {
	if p.Codex.Fast > 0 {
		return p.Codex.Fast
	}
	switch p.Model {
	case "gpt-6.2-astra", "gpt-6.2-sol", "gpt-6-astra", "gpt-6.1-sol", "gpt-6.1-terra",
		"gpt-6-sol", "gpt-6-luna", "gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4":
		return 2
	default:
		return 0
	}
}

func validPriceRates(rates ...float64) bool {
	for _, rate := range rates {
		if rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return false
		}
	}
	return true
}

var (
	creditHeading = regexp.MustCompile(`(?m)^## ([^\n\r]+)`)
	creditTable   = regexp.MustCompile(`(?is)<table\b[^>]*>(.*?)</table>`)
	creditBody    = regexp.MustCompile(`(?is)<tbody\b[^>]*>(.*?)</tbody>`)
	creditRow     = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	creditCell    = regexp.MustCompile(`(?is)<td\b[^>]*>(.*?)</td>`)
	creditBreak   = regexp.MustCompile(`(?i)<br\s*/?>`)
	creditTag     = regexp.MustCompile(`<[^>]+>`)
	creditModelID = regexp.MustCompile(`^gpt-[a-z0-9]+(?:[.-][a-z0-9]+)*$`)
)

func creditSection(doc, heading string) string {
	sections := creditHeading.FindAllStringSubmatchIndex(doc, -1)
	for index, section := range sections {
		if strings.EqualFold(strings.TrimSpace(doc[section[2]:section[3]]), heading) {
			end := len(doc)
			if index+1 < len(sections) {
				end = sections[index+1][0]
			}
			return doc[section[1]:end]
		}
	}
	return ""
}

func markdownPriceRows(section string, visit func([]string)) {
	for _, table := range creditTable.FindAllStringSubmatch(section, -1) {
		body := table[1]
		if match := creditBody.FindStringSubmatch(body); match != nil {
			body = match[1]
		}
		for _, row := range creditRow.FindAllStringSubmatch(body, -1) {
			fields := make([]string, 0, 4)
			for _, cell := range creditCell.FindAllStringSubmatch(row[1], -1) {
				fields = append(fields, cell[1])
			}
			visit(fields)
		}
	}
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|") {
			visit(strings.Split(strings.Trim(line, "|"), "|"))
		}
	}
}

func creditText(value string) string {
	return strings.TrimSpace(html.UnescapeString(creditTag.ReplaceAllString(value, "")))
}

func creditModelIDs(label string) []string {
	label = strings.ToLower(creditText(label))
	model := strings.Join(strings.Fields(label), "-")
	switch model {
	case "daybreak-blue":
		return []string{"gpt-daybreak-blue-latest", model}
	case "daybreak-red":
		return []string{"gpt-daybreak-red-latest", "gpt-5.6-cyber", model}
	case "gpt-5.6-sol":
		return []string{model, "gpt-5.6"}
	}
	if creditModelID.MatchString(model) {
		return []string{model}
	}
	return nil
}

func creditNumber(raw string) (float64, bool) {
	raw = strings.ToLower(creditText(raw))
	raw = strings.TrimSpace(strings.TrimSuffix(raw, " credits"))
	raw = strings.TrimSuffix(strings.TrimSuffix(raw, "x"), "×")
	rate, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", ""), 64)
	return rate, err == nil && validPriceRates(rate)
}

func decodeCodexCredits(doc string) (map[string]codexCreditRates, error) {
	rates := make(map[string]codexCreditRates)
	markdownPriceRows(creditSection(doc, "Token rates"), func(fields []string) {
		if len(fields) != 4 {
			return
		}
		input, inputOK := creditNumber(fields[1])
		cached, cacheOK := creditNumber(fields[2])
		output, outputOK := creditNumber(fields[3])
		if !inputOK || !cacheOK || !outputOK || input <= 0 || output <= 0 {
			return
		}
		for _, model := range creditModelIDs(fields[0]) {
			rates[model] = codexCreditRates{Input: input, Cached: cached, Output: output}
		}
	})
	if len(rates) == 0 {
		return nil, fmt.Errorf("no usable Codex token rates")
	}
	speed := creditSection(doc, "Speed multipliers")
	for _, label := range []string{"\nUltrafast\n", "\n### Ultrafast\n"} {
		if fast, _, found := strings.Cut(speed, label); found {
			speed = fast
		}
	}
	markdownPriceRows(speed, func(fields []string) {
		if len(fields) != 3 {
			return
		}
		fast, ok := creditNumber(fields[2])
		if !ok || fast <= 0 {
			return
		}
		for _, label := range strings.Split(creditBreak.ReplaceAllString(fields[0], "\n"), "\n") {
			for _, model := range creditModelIDs(label) {
				if rate, found := rates[model]; found {
					rate.Fast = fast
					rates[model] = rate
				}
			}
		}
	})
	return rates, nil
}

type priceSyncResult struct {
	Count      int      `json:"count"`
	APICount   int      `json:"api_count"`
	CodexCount int      `json:"codex_count"`
	Warnings   []string `json:"warnings,omitempty"`
}

func fetchPriceSource(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", pluginID+"/"+pluginVersion)
	res, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", res.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return raw, nil
}

func syncPrices(ctx context.Context, s *store, cfg config) (priceSyncResult, error) {
	type apiResult struct {
		prices []price
		err    error
	}
	apiDone := make(chan apiResult, 1)
	go func() {
		var result apiResult
		raw, err := fetchPriceSource(ctx, cfg.PriceSourceURL, 32<<20)
		if err == nil {
			result.prices, err = decodeCatalog(bytes.NewReader(raw))
		}
		result.err = err
		apiDone <- result
	}()
	var credits map[string]codexCreditRates
	raw, creditErr := fetchPriceSource(ctx, cfg.CodexPriceSourceURL, 2<<20)
	if creditErr == nil {
		credits, creditErr = decodeCodexCredits(string(raw))
	}
	api := <-apiDone
	result := priceSyncResult{APICount: len(api.prices), CodexCount: len(credits)}
	var sourceErrors []error
	for _, entry := range []struct {
		source string
		err    error
	}{{"API prices", api.err}, {"Codex Credits", creditErr}} {
		if entry.err != nil {
			sourceErrors = append(sourceErrors, fmt.Errorf("%s: %w", entry.source, entry.err))
			result.Warnings = append(result.Warnings, entry.source)
		}
	}
	if len(sourceErrors) == 2 {
		return result, errors.Join(sourceErrors...)
	}
	if err := s.writePriceCatalog(ctx, api.prices, credits, false, true); err != nil {
		return result, err
	}
	models := make(map[string]bool)
	for _, p := range api.prices {
		models[p.Model] = true
	}
	for model := range credits {
		models[model] = true
	}
	result.Count = len(models)
	return result, nil
}

func (a *app) syncPriceCatalog(ctx context.Context, s *store, cfg config) (priceSyncResult, error) {
	a.priceSyncMu.Lock()
	defer a.priceSyncMu.Unlock()
	if ctx.Err() != nil {
		return priceSyncResult{}, ctx.Err()
	}
	result, err := syncPrices(ctx, s, cfg)
	a.refreshPriceCatalog(ctx, s)
	return result, err
}

func (a *app) priceSyncWorker(ctx context.Context, s *store, cfg config, wake <-chan struct{}) {
	defer a.workWG.Done()
	interval := time.Duration(cfg.PriceSyncIntervalMinutes) * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var retry *time.Timer
	defer func() {
		if retry != nil {
			retry.Stop()
		}
	}()
	var retryAt <-chan time.Time
	var lastAttempt time.Time
	sync := func() {
		if retry != nil {
			retry.Stop()
			retryAt = nil
		}
		lastAttempt = time.Now()
		_, _ = a.syncPriceCatalog(ctx, s, cfg)
	}
	sync()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sync()
		case <-retryAt:
			sync()
		case <-wake:
			if wait := time.Until(lastAttempt.Add(priceSyncCooldown)); wait <= 0 {
				sync()
			} else if retryAt == nil && priceSyncCooldown < interval {
				retry = time.NewTimer(wait)
				retryAt = retry.C
			}
		}
	}
}
