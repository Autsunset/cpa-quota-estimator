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

func (a *app) startPricingTask(s *store, settings pricingSettings, cfg config) (pricingRecalcTask, error) {
	a.pricingTaskMu.Lock()
	defer a.pricingTaskMu.Unlock()
	if a.pricingTask != nil && (a.pricingTask.Status == "queued" || a.pricingTask.Status == "running") {
		return *a.pricingTask, fmt.Errorf("pricing recalculation already running")
	}
	task := pricingRecalcTask{ID: strconv.FormatInt(time.Now().UnixNano(), 36), Status: "queued", PricingMode: settings.PricingMode, AnchorModel: settings.AnchorModel, StartedAt: time.Now().Unix()}
	a.pricingTask = &task
	go a.runPricingTask(s, settings, cfg, task.ID)
	return task, nil
}

func (a *app) runPricingTask(s *store, settings pricingSettings, cfg config, id string) {
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
	count, err := s.recalculatePricing(ctx, settings, cfg, func(progress pricingRecalcProgress) error {
		update(func(task *pricingRecalcTask) { task.Progress = progress })
		if a.pricingProgressHook != nil {
			return a.pricingProgressHook(progress)
		}
		return nil
	})
	if err == nil {
		a.mu.Lock()
		if a.store == s {
			a.cfg = a.cfg.withPricingSettings(settings)
		} else {
			err = fmt.Errorf("store changed during recalculation")
		}
		a.mu.Unlock()
	}
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
	a.pricingRepricePending = false
	a.pricingRepriceIncludeCustom = false
	a.pricingTaskMu.Unlock()
	if pending {
		a.mu.RLock()
		currentStore, currentCfg := a.store, a.cfg
		a.mu.RUnlock()
		if currentStore == s {
			a.queuePricingReprice(s, currentCfg, includeCustom)
		}
	}
}

func (a *app) queueFitRepricing(s *store, cfg config) {
	a.queuePricingReprice(s, cfg, false)
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
