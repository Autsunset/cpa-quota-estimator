package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const calibrationMetadataPrefix = "guided_calibration_v1:"

type calibrationPhase struct {
	Model                string  `json:"model"`
	StartEventID         int64   `json:"start_event_id"`
	EndEventID           int64   `json:"end_event_id,omitempty"`
	StartPercent         float64 `json:"start_percent"`
	EndPercent           float64 `json:"end_percent"`
	ConsumedPercent      float64 `json:"consumed_percent"`
	Crossings            int     `json:"crossings"`
	TotalValue           float64 `json:"total_value"`
	ContaminationValue   float64 `json:"contamination_value"`
	ContaminationPercent float64 `json:"contamination_percent"`
	PollutionWarning     bool    `json:"pollution_warning"`
}

type calibrationSession struct {
	Account       string           `json:"account"`
	ModelA        string           `json:"model_a"`
	ModelB        string           `json:"model_b"`
	TargetPercent float64          `json:"target_percent"`
	CycleID       int64            `json:"cycle_id"`
	ResetAt       int64            `json:"reset_at"`
	Status        string           `json:"status"` // phase1, phase2, completed, invalid, cancelled
	InvalidReason string           `json:"invalid_reason,omitempty"`
	StartedAt     int64            `json:"started_at"`
	CompletedAt   int64            `json:"completed_at,omitempty"`
	PhaseA        calibrationPhase `json:"phase_a"`
	PhaseB        calibrationPhase `json:"phase_b"`
	Before        *weightEstimate  `json:"before,omitempty"`
	After         *weightEstimate  `json:"after,omitempty"`
	RefitStatus   string           `json:"refit_status,omitempty"`
}

func calibrationKey(account string) string { return calibrationMetadataPrefix + account }

func (s *store) calibrationSession(ctx context.Context, account string) (calibrationSession, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, calibrationKey(account)).Scan(&raw)
	if err == sql.ErrNoRows {
		return calibrationSession{}, false, nil
	}
	if err != nil {
		return calibrationSession{}, false, err
	}
	var session calibrationSession
	if err = json.Unmarshal([]byte(raw), &session); err != nil {
		return calibrationSession{}, false, err
	}
	decorateCalibration(&session)
	return session, true, nil
}

func decorateCalibration(session *calibrationSession) {
	for _, phase := range []*calibrationPhase{&session.PhaseA, &session.PhaseB} {
		if phase.TotalValue > 0 {
			phase.ContaminationPercent = 100 * phase.ContaminationValue / phase.TotalValue
		}
		phase.PollutionWarning = phase.ContaminationPercent > 5
	}
}

func saveCalibration(ctx context.Context, tx *sql.Tx, session calibrationSession) error {
	raw, err := json.Marshal(session)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, calibrationKey(session.Account), string(raw))
	return err
}

func (s *store) startCalibration(ctx context.Context, account, modelA, modelB string, target float64, before *weightEstimate) (calibrationSession, error) {
	account = strings.TrimSpace(account)
	modelA, modelB = normalizeModel(modelA), normalizeModel(modelB)
	if account == "" || modelA == "" || modelB == "" || modelA == modelB {
		return calibrationSession{}, fmt.Errorf("account and distinct models are required")
	}
	if !isFinitePositive(target) || target < 1 || target > 25 {
		return calibrationSession{}, fmt.Errorf("target_percent must be between 1 and 25")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return calibrationSession{}, err
	}
	defer tx.Rollback()
	var priorRaw string
	priorErr := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, calibrationKey(account)).Scan(&priorRaw)
	if priorErr != nil && priorErr != sql.ErrNoRows {
		return calibrationSession{}, priorErr
	}
	if priorErr == nil {
		var prior calibrationSession
		if err = json.Unmarshal([]byte(priorRaw), &prior); err != nil {
			return calibrationSession{}, err
		}
		if prior.Status == "phase1" || prior.Status == "phase2" {
			return calibrationSession{}, fmt.Errorf("cancel the active calibration before starting another")
		}
	}
	var cycleID, resetAt int64
	err = tx.QueryRowContext(ctx, `SELECT id,reset_at FROM quota_cycles WHERE account=? AND ended_at=0 ORDER BY id DESC LIMIT 1`, account).Scan(&cycleID, &resetAt)
	if err != nil {
		return calibrationSession{}, fmt.Errorf("current quota cycle required: %w", err)
	}
	var used float64
	err = tx.QueryRowContext(ctx, `SELECT used_percent FROM quota_samples WHERE cycle_id=? ORDER BY sampled_at DESC,id DESC LIMIT 1`, cycleID).Scan(&used)
	if err != nil {
		return calibrationSession{}, fmt.Errorf("current quota sample required: %w", err)
	}
	var lastEventID int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM usage_events`).Scan(&lastEventID); err != nil {
		return calibrationSession{}, err
	}
	session := calibrationSession{Account: account, ModelA: modelA, ModelB: modelB, TargetPercent: target, CycleID: cycleID, ResetAt: resetAt, Status: "phase1", StartedAt: time.Now().Unix(), Before: before,
		PhaseA: calibrationPhase{Model: modelA, StartEventID: lastEventID + 1, StartPercent: used, EndPercent: used},
		PhaseB: calibrationPhase{Model: modelB}}
	if err = saveCalibration(ctx, tx, session); err != nil {
		return calibrationSession{}, err
	}
	if err = tx.Commit(); err != nil {
		return calibrationSession{}, err
	}
	return session, nil
}

func (s *store) advanceCalibrationForEvent(ctx context.Context, tx *sql.Tx, e event, cycleID, eventID int64) error {
	if eventQuotaScope(e) != mainQuotaScope {
		return nil
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, calibrationKey(e.Account)).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var session calibrationSession
	if err = json.Unmarshal([]byte(raw), &session); err != nil {
		return err
	}
	if session.Status != "phase1" && session.Status != "phase2" {
		return nil
	}
	if e.UsedPercent != nil && cycleID > 0 && cycleID != session.CycleID {
		session.Status = "invalid"
		session.InvalidReason = "quota_reset"
		session.CompletedAt = eventObservationTime(e)
		return saveCalibration(ctx, tx, session)
	}
	var phase *calibrationPhase
	if session.Status == "phase1" {
		phase = &session.PhaseA
	} else {
		phase = &session.PhaseB
	}
	if eventID < phase.StartEventID {
		return nil
	}
	if e.CostUSD > 0 {
		phase.TotalValue += e.CostUSD
		if normalizeModel(e.Model) != phase.Model {
			phase.ContaminationValue += e.CostUSD
		}
	}
	if !e.Failed && e.UsedPercent != nil && cycleID == session.CycleID {
		if *e.UsedPercent > phase.EndPercent {
			oldInteger := int(math.Floor(phase.EndPercent + 1e-7))
			newInteger := int(math.Floor(*e.UsedPercent + 1e-7))
			if newInteger > oldInteger {
				phase.Crossings += newInteger - oldInteger
			}
			phase.EndPercent = *e.UsedPercent
			phase.ConsumedPercent = math.Max(0, phase.EndPercent-phase.StartPercent)
		}
		if phase.ConsumedPercent >= session.TargetPercent {
			phase.EndEventID = eventID
			if session.Status == "phase1" {
				session.Status = "phase2"
				session.PhaseB.StartEventID = eventID + 1
				session.PhaseB.StartPercent = phase.EndPercent
				session.PhaseB.EndPercent = phase.EndPercent
			} else {
				session.Status = "completed"
				session.CompletedAt = eventObservationTime(e)
				session.RefitStatus = "pending"
			}
		}
	}
	return saveCalibration(ctx, tx, session)
}

func (s *store) endOrCancelCalibration(ctx context.Context, account string, cancel bool) (calibrationSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return calibrationSession{}, err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, calibrationKey(account)).Scan(&raw); err != nil {
		return calibrationSession{}, err
	}
	var session calibrationSession
	if err = json.Unmarshal([]byte(raw), &session); err != nil {
		return calibrationSession{}, err
	}
	if session.Status != "phase1" && session.Status != "phase2" {
		return session, fmt.Errorf("no active calibration phase")
	}
	var lastEventID int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM usage_events WHERE account=?`, account).Scan(&lastEventID); err != nil {
		return calibrationSession{}, err
	}
	if cancel {
		session.Status = "cancelled"
		session.CompletedAt = time.Now().Unix()
	} else if session.Status == "phase1" {
		session.PhaseA.EndEventID = lastEventID
		session.Status = "phase2"
		session.PhaseB.StartEventID = lastEventID + 1
		session.PhaseB.StartPercent = session.PhaseA.EndPercent
		session.PhaseB.EndPercent = session.PhaseA.EndPercent
	} else {
		session.PhaseB.EndEventID = lastEventID
		session.Status = "completed"
		session.CompletedAt = time.Now().Unix()
		session.RefitStatus = "pending"
	}
	if err = saveCalibration(ctx, tx, session); err != nil {
		return calibrationSession{}, err
	}
	if err = tx.Commit(); err != nil {
		return calibrationSession{}, err
	}
	decorateCalibration(&session)
	return session, nil
}

func calibrationWeight(fit *weightFit, model string) *weightEstimate {
	if fit == nil || !fit.Available {
		return nil
	}
	for _, row := range fit.Models {
		if row.Model == normalizeModel(model) {
			value := row.Input
			return &value
		}
	}
	return nil
}

func (s *store) claimCalibrationRefit(ctx context.Context, account string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, calibrationKey(account)).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var session calibrationSession
	if err = json.Unmarshal([]byte(raw), &session); err != nil {
		return false, err
	}
	if session.Status != "completed" || session.RefitStatus != "pending" {
		return false, nil
	}
	session.RefitStatus = "running"
	if err = saveCalibration(ctx, tx, session); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *store) finishCalibrationRefit(ctx context.Context, account string, after *weightEstimate, fitErr error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, calibrationKey(account)).Scan(&raw); err != nil {
		return err
	}
	var session calibrationSession
	if err = json.Unmarshal([]byte(raw), &session); err != nil {
		return err
	}
	if session.Status != "completed" || session.RefitStatus != "running" {
		return nil
	}
	if fitErr != nil {
		session.RefitStatus = "failed"
	} else {
		session.RefitStatus = "done"
		session.After = after
	}
	if err = saveCalibration(ctx, tx, session); err != nil {
		return err
	}
	return tx.Commit()
}
