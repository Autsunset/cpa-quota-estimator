package main

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

type pricingRecalcTask struct {
	ID                 string                `json:"task_id"`
	Status             string                `json:"status"`
	Progress           pricingRecalcProgress `json:"progress"`
	PricingMode        string                `json:"pricing_mode"`
	AnchorModel        string                `json:"anchor_model"`
	RecalculatedEvents int64                 `json:"recalculated_events,omitempty"`
	StartedAt          int64                 `json:"started_at"`
	FinishedAt         int64                 `json:"finished_at,omitempty"`
	Error              string                `json:"error,omitempty"`
}

func (a *app) latestPricingTask(id string) (pricingRecalcTask, bool) {
	a.pricingTaskMu.Lock()
	defer a.pricingTaskMu.Unlock()
	if id != "" && (a.pricingTask == nil || a.pricingTask.ID != id) {
		old, ok := a.pricingTaskHistory[id]
		return old, ok
	}
	if a.pricingTask == nil {
		return pricingRecalcTask{}, false
	}
	return *a.pricingTask, true
}

func (a *app) pricingTaskActive() (bool, string) {
	task, ok := a.latestPricingTask("")
	if !ok {
		return false, ""
	}
	return task.Status == "queued" || task.Status == "running" || task.Status == "rolling_back", task.ID
}

func (a *app) startPricingTask(s *store, settings pricingSettings, _ config) (pricingRecalcTask, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pricingTaskMu.Lock()
	defer a.pricingTaskMu.Unlock()
	if a.pricingTask != nil && (a.pricingTask.Status == "queued" || a.pricingTask.Status == "running" || a.pricingTask.Status == "rolling_back") {
		return *a.pricingTask, fmt.Errorf("pricing recalculation already running")
	}
	if a.store != s {
		return pricingRecalcTask{}, fmt.Errorf("plugin store changed")
	}
	previousCfg := a.cfg
	a.cfg = a.cfg.withPricingSettings(settings)
	cfg := a.cfg
	task := pricingRecalcTask{ID: strconv.FormatInt(time.Now().UnixNano(), 36), Status: "queued", PricingMode: settings.PricingMode, AnchorModel: settings.AnchorModel, StartedAt: time.Now().Unix()}
	a.pricingTask = &task
	go a.runPricingTask(s, settings, cfg, previousCfg, task.ID)
	return task, nil
}

func (a *app) runPricingTask(s *store, settings pricingSettings, cfg, previousCfg config, id string) {
	update := func(f func(*pricingRecalcTask)) {
		a.pricingTaskMu.Lock()
		if a.pricingTask != nil && a.pricingTask.ID == id {
			f(a.pricingTask)
		}
		a.pricingTaskMu.Unlock()
	}
	update(func(task *pricingRecalcTask) { task.Status = "running" })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	rollbackCfg := func() {
		update(func(task *pricingRecalcTask) { task.Status = "rolling_back" })
		a.mu.Lock()
		if a.store == s {
			a.cfg = previousCfg
		}
		a.mu.Unlock()
	}
	count, err := s.recalculatePricingBatches(ctx, settings, cfg, previousCfg, func(progress pricingRecalcProgress) error {
		update(func(task *pricingRecalcTask) { task.Progress = progress })
		if a.pricingProgressHook != nil {
			return a.pricingProgressHook(progress)
		}
		return nil
	}, rollbackCfg)
	if err != nil {
		a.mu.Lock()
		if a.store == s {
			a.cfg = previousCfg
		}
		a.mu.Unlock()
	}
	go func() {
		checkpointCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.checkpointWAL(checkpointCtx)
	}()
	update(func(task *pricingRecalcTask) {
		task.FinishedAt = time.Now().Unix()
		if err != nil {
			task.Status = "failed"
			task.Error = err.Error()
		} else {
			task.Status = "succeeded"
			task.RecalculatedEvents = count
		}
	})
	a.pricingTaskMu.Lock()
	if a.pricingTaskHistory == nil {
		a.pricingTaskHistory = make(map[string]pricingRecalcTask)
	}
	if a.pricingTask != nil && a.pricingTask.ID == id {
		a.pricingTaskHistory[id] = *a.pricingTask
	}
	if err == nil {
		a.lastPricedFit = cfg.LearnedFit
	}
	if len(a.pricingTaskHistory) > 32 {
		oldest := ""
		for key, item := range a.pricingTaskHistory {
			if oldest == "" || item.FinishedAt < a.pricingTaskHistory[oldest].FinishedAt {
				oldest = key
			}
		}
		delete(a.pricingTaskHistory, oldest)
	}
	pending := a.pricingRepricePending
	includeCustom := a.pricingRepriceIncludeCustom
	pendingFit := a.pricingFitPending
	a.pricingRepricePending = false
	a.pricingRepriceIncludeCustom = false
	a.pricingFitPending = false
	a.pricingTaskMu.Unlock()
	if pending {
		a.mu.RLock()
		currentStore, currentCfg := a.store, a.cfg
		a.mu.RUnlock()
		if currentStore == s {
			a.queuePricingReprice(s, currentCfg, includeCustom)
		}
	}
	if pendingFit {
		a.mu.RLock()
		currentStore, currentCfg := a.store, a.cfg
		a.mu.RUnlock()
		if currentStore == s {
			a.queueFitRepricing(s, currentCfg)
		}
	}
}

func (a *app) queuePricingReprice(s *store, cfg config, includeCustom bool) {
	if !includeCustom && normalizePricingMode(cfg.PricingMode) == pricingModeCustom {
		return
	}
	if _, err := a.startPricingTask(s, cfg.pricingSettings(), cfg); err != nil {
		a.pricingTaskMu.Lock()
		a.pricingRepricePending = true
		a.pricingRepriceIncludeCustom = a.pricingRepriceIncludeCustom || includeCustom
		a.pricingTaskMu.Unlock()
	}
}
