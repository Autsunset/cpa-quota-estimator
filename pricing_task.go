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
	return a.startPricingTaskLocked(s, settings)
}

func (a *app) startPricingTaskLocked(s *store, settings pricingSettings) (pricingRecalcTask, error) {
	a.pricingTaskMu.Lock()
	defer a.pricingTaskMu.Unlock()
	if a.pricingTask != nil && (a.pricingTask.Status == "queued" || a.pricingTask.Status == "running" || a.pricingTask.Status == "rolling_back") {
		if settings.CatalogRevision > 0 {
			a.pricingRepricePending = true
		}
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
	a.workWG.Add(1)
	go a.runPricingTask(a.workContext(), s, settings, cfg, previousCfg, task.ID)
	return task, nil
}

func (a *app) runPricingTask(workCtx context.Context, s *store, settings pricingSettings, cfg, previousCfg config, id string) {
	defer a.workWG.Done()
	update := func(f func(*pricingRecalcTask)) {
		a.pricingTaskMu.Lock()
		if a.pricingTask != nil && a.pricingTask.ID == id {
			f(a.pricingTask)
		}
		a.pricingTaskMu.Unlock()
	}
	update(func(task *pricingRecalcTask) { task.Status = "running" })
	ctx, cancel := context.WithTimeout(workCtx, 20*time.Minute)
	defer cancel()
	rollbackCfg := func() {
		update(func(task *pricingRecalcTask) { task.Status = "rolling_back" })
		a.mu.Lock()
		if a.store == s {
			if previousCfg.LearnedFit != nil {
				restoreCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
				_ = s.seedOnlineCycleScales(restoreCtx, previousCfg.LearnedFit)
				done()
			}
			catalog := a.cfg.PriceCatalog
			a.cfg = previousCfg
			a.cfg.PriceCatalog = catalog
		}
		a.mu.Unlock()
	}
	var count int64
	var err error
	if settings.CatalogRevision > 0 {
		var hasUnknown bool
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM quota_segments WHERE instr(flags_json,'"unknown_model"')>0)`).Scan(&hasUnknown)
		if err == nil && hasUnknown {
			err = s.rebuildAllQuotaSegments(ctx)
		}
	}
	if err == nil && (settings.CatalogRevision > 0 || !cfg.fitMatchesReference(cfg.LearnedFit) ||
		cfg.LearnedFit.EligibilityVersion != weightEligibilityVersion) {
		update(func(task *pricingRecalcTask) { task.Progress.Stage = "fitting" })
		var result weightBacktest
		result, err = s.refreshWeightFitProgress(ctx, cfg.weightLearnerOptions(), func(done, total int) {
			update(func(task *pricingRecalcTask) {
				task.Progress = pricingRecalcProgress{Stage: "fitting", FitsDone: done, FitsTotal: total}
			})
		})
		if err == nil {
			cfg.LearnedFit = &result.FittedWeights
			a.mu.Lock()
			if a.store != s || a.cfg.anchorModel() != cfg.anchorModel() {
				err = fmt.Errorf("plugin reference changed while fitting")
			} else if err = s.seedOnlineCycleScales(ctx, cfg.LearnedFit); err == nil {
				a.cfg.LearnedFit = cfg.LearnedFit
			}
			a.mu.Unlock()
		}
	}
	if err == nil {
		count, err = s.recalculatePricingBatches(ctx, settings, cfg, previousCfg, func(progress pricingRecalcProgress) error {
			update(func(task *pricingRecalcTask) { task.Progress = progress })
			if a.pricingProgressHook != nil {
				return a.pricingProgressHook(progress)
			}
			return nil
		}, rollbackCfg)
	}
	if err != nil {
		rollbackCfg()
	} else if cfg.LearnedFit != nil {
		_, _ = s.updateWeightAttributions(ctx, cfg.LearnedFit, cfg.LongContextThreshold)
	}
	s.spawnDetached(func() {
		checkpointCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.checkpointWAL(checkpointCtx)
	})
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
	pendingFit := a.pricingFitPending
	a.pricingRepricePending = false
	a.pricingFitPending = false
	a.pricingTaskMu.Unlock()
	if pending {
		a.priceSyncMu.Lock()
		a.refreshPriceCatalog(workCtx, s)
		a.priceSyncMu.Unlock()
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
