package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

const (
	resetPercentTolerance       = 1.0
	resetLowPercent             = 5.0
	resetConfirmationSamples    = 3
	resetConfirmationMinSeconds = 60
	scheduledResetTolerance     = 300
	quotaExhaustedPercent       = 100.0
)

type recordedQuotaEvent struct {
	ID            int64
	RequestedAt   int64
	ObservedAt    int64
	UsedPercent   float64
	ResetAt       int64
	WindowMinutes int64
	PlanType      string
	Failed        bool
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

type legacyCycleGroup struct {
	Account       string
	ResetAt       int64
	WindowMinutes int64
	FirstSampleAt int64
	LastSampleAt  int64
	StartPercent  float64
	PeakPercent   float64
	PlanType      string
}

func (s *store) backfillCycles() error {
	rows, err := s.db.Query(`
SELECT account,reset_at,MAX(window_minutes),MIN(sampled_at),MAX(sampled_at),MIN(used_percent),MAX(used_percent),MAX(plan_type)
FROM quota_samples
WHERE cycle_id=0
GROUP BY account,reset_at
ORDER BY account,MIN(sampled_at)`)
	if err != nil {
		return err
	}
	var groups []legacyCycleGroup
	for rows.Next() {
		var group legacyCycleGroup
		if err = rows.Scan(&group.Account, &group.ResetAt, &group.WindowMinutes, &group.FirstSampleAt, &group.LastSampleAt, &group.StartPercent, &group.PeakPercent, &group.PlanType); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, group)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type insertedCycle struct {
		ID        int64
		Account   string
		StartedAt int64
	}
	var inserted []insertedCycle
	var previousID int64
	var previousAccount string
	for _, group := range groups {
		startedAt := group.ResetAt - group.WindowMinutes*60
		if startedAt <= 0 || startedAt > group.FirstSampleAt {
			startedAt = group.FirstSampleAt
		}
		if previousID > 0 && previousAccount == group.Account {
			if _, err = tx.Exec(`UPDATE quota_cycles SET ended_at=?,close_reason='observed_reset',end_used_percent=peak_used_percent WHERE id=?`, startedAt, previousID); err != nil {
				return err
			}
		}
		result, errInsert := tx.Exec(`INSERT INTO quota_cycles(account,started_at,reset_at,window_minutes,plan_type,first_sample_at,last_sample_at,start_used_percent,end_used_percent,peak_used_percent) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			group.Account, startedAt, group.ResetAt, group.WindowMinutes, group.PlanType, group.FirstSampleAt, group.LastSampleAt, group.StartPercent, group.PeakPercent, group.PeakPercent)
		if errInsert != nil {
			return errInsert
		}
		cycleID, errID := result.LastInsertId()
		if errID != nil {
			return errID
		}
		if _, err = tx.Exec(`UPDATE quota_samples SET cycle_id=? WHERE cycle_id=0 AND account=? AND reset_at=?`, cycleID, group.Account, group.ResetAt); err != nil {
			return err
		}
		inserted = append(inserted, insertedCycle{ID: cycleID, Account: group.Account, StartedAt: startedAt})
		previousID, previousAccount = cycleID, group.Account
	}
	for index, cycle := range inserted {
		endAt := int64(math.MaxInt64)
		if index+1 < len(inserted) && inserted[index+1].Account == cycle.Account {
			endAt = inserted[index+1].StartedAt
		}
		if _, err = tx.Exec(`UPDATE usage_events SET cycle_id=? WHERE cycle_id=0 AND account=? AND quota_scope=? AND requested_at>=? AND requested_at<?`, cycle.ID, cycle.Account, mainQuotaScope, cycle.StartedAt, endAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) ensureEventCycle(ctx context.Context, tx *sql.Tx, e event) (quotaCycle, bool, error) {
	if eventQuotaScope(e) != mainQuotaScope {
		return quotaCycle{}, false, nil
	}
	current, err := openCycle(ctx, tx, e.Account)
	if err != nil && err != sql.ErrNoRows {
		return quotaCycle{}, false, err
	}
	hasQuota := e.UsedPercent != nil && e.ResetAt > 0 && e.WindowMinutes > 0
	if err == sql.ErrNoRows {
		if !hasQuota {
			return quotaCycle{}, false, nil
		}
		created, errCreate := createCycle(ctx, tx, e.Account, eventCycleStart(e, 0), e)
		if errCreate != nil {
			return quotaCycle{}, false, errCreate
		}
		if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=? WHERE cycle_id=0 AND account=? AND quota_scope=? AND requested_at>=? AND requested_at<=?`, created.ID, e.Account, mainQuotaScope, created.StartedAt, e.RequestedAt); err != nil {
			return quotaCycle{}, false, err
		}
		return created, true, nil
	}
	if !hasQuota {
		return current, false, nil
	}

	resetAtChanged := current.ResetAt != e.ResetAt
	regimeChanged := (current.WindowMinutes > 0 && current.WindowMinutes != e.WindowMinutes) ||
		(current.PlanType != "" && e.PlanType != "" && current.PlanType != e.PlanType)

	// A newly allocated window can temporarily keep reporting the exhausted
	// percentage from the preceding window while its reset_at moves forward
	// with each request. Such a pending cycle has deliberately not accepted a
	// quota sample yet. Keep its schedule current, but do not let the stale
	// 100% reading become the new cycle peak.
	if current.FirstSampleAt == 0 && !regimeChanged {
		if resetAtChanged {
			if e.ResetAt+scheduledResetTolerance < current.ResetAt {
				return current, false, nil
			}
			startedAt := pendingCycleStart(current, e)
			if _, err = tx.ExecContext(ctx, `UPDATE quota_cycles SET started_at=?,reset_at=?,window_minutes=?,plan_type=CASE WHEN ?='' THEN plan_type ELSE ? END WHERE id=?`, startedAt, e.ResetAt, e.WindowMinutes, e.PlanType, e.PlanType, current.ID); err != nil {
				return quotaCycle{}, false, err
			}
			current.StartedAt = startedAt
			current.ResetAt = e.ResetAt
			current.WindowMinutes = e.WindowMinutes
			if e.PlanType != "" {
				current.PlanType = e.PlanType
			}
		}
		if e.Failed || *e.UsedPercent >= quotaExhaustedPercent {
			return current, false, nil
		}
		return current, true, nil
	}

	// Repair a cycle already contaminated by the old behavior. The raw event
	// history still contains the expired reset boundary even if quota_cycles
	// was later extended in place. A lower reading after an exhausted boundary
	// is the first trustworthy sample of the replacement window.
	if current.PeakPercent >= quotaExhaustedPercent && !e.Failed && *e.UsedPercent < quotaExhaustedPercent {
		oldResetAt, found, errFind := findExhaustedScheduleAdvance(ctx, tx, current, e)
		if errFind != nil {
			return quotaCycle{}, false, errFind
		}
		if found {
			return repairExhaustedScheduleCarryover(ctx, tx, current, oldResetAt, e)
		}
	}

	// Once an exhausted window has actually expired, one forward reset_at is
	// enough to establish the next cycle. Waiting for two identical reset_at
	// values fails for unstarted 5-hour windows because that timestamp slides
	// forward until a fresh window is activated.
	if resetAtChanged && current.PeakPercent >= quotaExhaustedPercent && expiredScheduleAdvance(current, e) {
		return startAdvancedCycle(ctx, tx, current, e)
	}

	if resetAtChanged && scheduledResetObservation(current, e) {
		confirmed, errConfirm := confirmScheduledReset(ctx, tx, current, e)
		if errConfirm != nil {
			return quotaCycle{}, false, errConfirm
		}
		if !confirmed {
			return current, false, nil
		}
		startedAt := current.ResetAt
		if err = closeCycle(ctx, tx, current.ID, startedAt, "scheduled_reset"); err != nil {
			return quotaCycle{}, false, err
		}
		created, errCreate := createCycle(ctx, tx, e.Account, startedAt, e)
		if errCreate != nil {
			return quotaCycle{}, false, errCreate
		}
		if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=? WHERE cycle_id=? AND quota_scope=? AND requested_at>=?`, created.ID, current.ID, mainQuotaScope, startedAt); err != nil {
			return quotaCycle{}, false, err
		}
		return created, true, nil
	}
	if resetAtChanged && e.Failed {
		return current, false, nil
	}

	peak := current.PeakPercent
	if !resetAtChanged && peak > 0 && !e.Failed && resetCandidate(peak, *e.UsedPercent) {
		first, confirmed, errConfirm := confirmEarlyReset(ctx, tx, current, e)
		if errConfirm != nil {
			return quotaCycle{}, false, errConfirm
		}
		if !confirmed {
			return current, false, nil
		}
		if err = closeCycle(ctx, tx, current.ID, first.RequestedAt, "early_reset"); err != nil {
			return quotaCycle{}, false, err
		}
		firstUsed := first.UsedPercent
		firstEvent := event{RequestedAt: first.RequestedAt, ObservedAt: first.ObservedAt, Account: e.Account, UsedPercent: &firstUsed, ResetAt: first.ResetAt, WindowMinutes: first.WindowMinutes, PlanType: first.PlanType}
		created, errCreate := createCycle(ctx, tx, e.Account, first.RequestedAt, firstEvent)
		if errCreate != nil {
			return quotaCycle{}, false, errCreate
		}
		if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=? WHERE cycle_id=? AND quota_scope=? AND (requested_at>? OR (requested_at=? AND id>=?))`, created.ID, current.ID, mainQuotaScope, first.RequestedAt, first.RequestedAt, first.ID); err != nil {
			return quotaCycle{}, false, err
		}
		if err = insertSampleFromRecordedEvent(ctx, tx, created, first.ID, firstEvent); err != nil {
			return quotaCycle{}, false, err
		}
		return created, true, nil
	}

	if resetAtChanged {
		regimeChanged = true
	}
	if regimeChanged {
		if _, err = tx.ExecContext(ctx, `UPDATE quota_cycles SET reset_at=?,window_minutes=?,plan_type=CASE WHEN ?='' THEN plan_type ELSE ? END WHERE id=?`, e.ResetAt, e.WindowMinutes, e.PlanType, e.PlanType, current.ID); err != nil {
			return quotaCycle{}, false, err
		}
		current.ResetAt = e.ResetAt
		current.WindowMinutes = e.WindowMinutes
		if e.PlanType != "" {
			current.PlanType = e.PlanType
		}
	}
	return current, true, nil
}

func pendingCycleStart(current quotaCycle, e event) int64 {
	startedAt := eventCycleStart(e, 0)
	if startedAt < current.StartedAt {
		return current.StartedAt
	}
	return startedAt
}

func expiredScheduleAdvance(current quotaCycle, e event) bool {
	if current.ResetAt <= 0 || e.ResetAt <= current.ResetAt+scheduledResetTolerance || e.WindowMinutes <= 0 {
		return false
	}
	if current.WindowMinutes > 0 && current.WindowMinutes != e.WindowMinutes || !compatiblePlan(current.PlanType, e.PlanType) {
		return false
	}
	observedAt := eventObservationTime(e)
	if observedAt < current.ResetAt {
		return false
	}
	declaredStart := e.ResetAt - e.WindowMinutes*60
	return declaredStart >= current.ResetAt-scheduledResetTolerance && declaredStart <= observedAt+scheduledResetTolerance
}

func startAdvancedCycle(ctx context.Context, tx *sql.Tx, current quotaCycle, e event) (quotaCycle, bool, error) {
	oldResetAt := current.ResetAt
	startedAt := eventCycleStart(e, current.StartedAt)
	if startedAt < oldResetAt {
		startedAt = oldResetAt
	}
	if err := closeCycle(ctx, tx, current.ID, oldResetAt, "scheduled_reset"); err != nil {
		return quotaCycle{}, false, err
	}
	created, err := createCycle(ctx, tx, e.Account, startedAt, e)
	if err != nil {
		return quotaCycle{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=? WHERE cycle_id=? AND quota_scope=? AND requested_at>=?`, created.ID, current.ID, mainQuotaScope, oldResetAt); err != nil {
		return quotaCycle{}, false, err
	}
	acceptSample := !e.Failed && !(current.PeakPercent >= quotaExhaustedPercent && *e.UsedPercent >= quotaExhaustedPercent)
	return created, acceptSample, nil
}

func findExhaustedScheduleAdvance(ctx context.Context, tx *sql.Tx, current quotaCycle, e event) (int64, bool, error) {
	events, err := quotaEventsAscending(ctx, tx, current.ID)
	if err != nil {
		return 0, false, err
	}
	observedAt := eventObservationTime(e)
	for index := 1; index < len(events); index++ {
		previous, next := events[index-1], events[index]
		if previous.Failed || previous.UsedPercent < quotaExhaustedPercent || next.ResetAt <= previous.ResetAt+scheduledResetTolerance {
			continue
		}
		if previous.WindowMinutes <= 0 || previous.WindowMinutes != next.WindowMinutes || !compatiblePlan(previous.PlanType, next.PlanType) {
			continue
		}
		declaredStart := next.ResetAt - next.WindowMinutes*60
		if next.ObservedAt < previous.ResetAt || declaredStart < previous.ResetAt-scheduledResetTolerance || declaredStart > next.ObservedAt+scheduledResetTolerance {
			continue
		}
		if observedAt < previous.ResetAt || e.ResetAt <= previous.ResetAt || e.WindowMinutes != next.WindowMinutes || !compatiblePlan(e.PlanType, next.PlanType) {
			continue
		}
		return previous.ResetAt, true, nil
	}
	return 0, false, nil
}

func quotaEventsAscending(ctx context.Context, tx *sql.Tx, cycleID int64) ([]recordedQuotaEvent, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,requested_at,CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,used_percent,reset_at,window_minutes,plan_type,failed
FROM usage_events
WHERE cycle_id=? AND quota_scope=? AND used_percent IS NOT NULL AND reset_at>0 AND window_minutes>0
ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END ASC,id ASC`, cycleID, mainQuotaScope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []recordedQuotaEvent
	for rows.Next() {
		var item recordedQuotaEvent
		if err = rows.Scan(&item.ID, &item.RequestedAt, &item.ObservedAt, &item.UsedPercent, &item.ResetAt, &item.WindowMinutes, &item.PlanType, &item.Failed); err != nil {
			return nil, err
		}
		events = append(events, item)
	}
	return events, rows.Err()
}

func repairExhaustedScheduleCarryover(ctx context.Context, tx *sql.Tx, current quotaCycle, oldResetAt int64, e event) (quotaCycle, bool, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM quota_samples WHERE cycle_id=? AND sampled_at>=?`, current.ID, oldResetAt); err != nil {
		return quotaCycle{}, false, err
	}
	if err := refreshCycleSampleSummary(ctx, tx, current.ID); err != nil {
		return quotaCycle{}, false, err
	}
	if err := closeCycle(ctx, tx, current.ID, oldResetAt, "scheduled_reset"); err != nil {
		return quotaCycle{}, false, err
	}
	startedAt := eventCycleStart(e, current.StartedAt)
	if startedAt < oldResetAt {
		startedAt = oldResetAt
	}
	created, err := createCycle(ctx, tx, e.Account, startedAt, e)
	if err != nil {
		return quotaCycle{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=? WHERE cycle_id=? AND quota_scope=? AND requested_at>=?`, created.ID, current.ID, mainQuotaScope, oldResetAt); err != nil {
		return quotaCycle{}, false, err
	}
	return created, true, nil
}

func eventObservationTime(e event) int64 {
	if e.ObservedAt > 0 {
		return e.ObservedAt
	}
	return e.RequestedAt
}

func scheduledResetObservation(current quotaCycle, e event) bool {
	if e.Failed || current.ResetAt <= 0 || e.ResetAt <= 0 || e.WindowMinutes <= 0 {
		return false
	}
	declaredStart := e.ResetAt - e.WindowMinutes*60
	return absInt64(declaredStart-current.ResetAt) <= scheduledResetTolerance &&
		eventObservationTime(e) >= current.ResetAt-scheduledResetTolerance
}

func confirmScheduledReset(ctx context.Context, tx *sql.Tx, current quotaCycle, e event) (bool, error) {
	events, err := recentQuotaEvents(ctx, tx, current.ID, 1)
	if err != nil || len(events) == 0 {
		return false, err
	}
	previous := events[0]
	if previous.Failed || previous.ObservedAt > eventObservationTime(e) || !sameQuotaRegime(previous, e) {
		return false, nil
	}
	previousUsed := previous.UsedPercent
	previousEvent := event{RequestedAt: previous.RequestedAt, ObservedAt: previous.ObservedAt, UsedPercent: &previousUsed, ResetAt: previous.ResetAt, WindowMinutes: previous.WindowMinutes, PlanType: previous.PlanType}
	return scheduledResetObservation(current, previousEvent), nil
}

func confirmEarlyReset(ctx context.Context, tx *sql.Tx, current quotaCycle, e event) (recordedQuotaEvent, bool, error) {
	events, err := recentQuotaEvents(ctx, tx, current.ID, resetConfirmationSamples-1)
	if err != nil || len(events) < resetConfirmationSamples-1 {
		return recordedQuotaEvent{}, false, err
	}
	oldest, latest := events[len(events)-1], events[0]
	observedAt := eventObservationTime(e)
	if latest.ObservedAt > observedAt || observedAt-oldest.ObservedAt < resetConfirmationMinSeconds {
		return recordedQuotaEvent{}, false, nil
	}
	sequence := append([]recordedQuotaEvent(nil), events...)
	for left, right := 0, len(sequence)-1; left < right; left, right = left+1, right-1 {
		sequence[left], sequence[right] = sequence[right], sequence[left]
	}
	for index, recorded := range sequence {
		if recorded.Failed || !sameQuotaRegime(recorded, e) || !resetCandidate(current.PeakPercent, recorded.UsedPercent) {
			return recordedQuotaEvent{}, false, nil
		}
		if index > 0 && recorded.UsedPercent+resetPercentTolerance < sequence[index-1].UsedPercent {
			return recordedQuotaEvent{}, false, nil
		}
	}
	if *e.UsedPercent+resetPercentTolerance < latest.UsedPercent {
		return recordedQuotaEvent{}, false, nil
	}
	return oldest, true, nil
}

func recentQuotaEvents(ctx context.Context, tx *sql.Tx, cycleID int64, limit int) ([]recordedQuotaEvent, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,requested_at,CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,used_percent,reset_at,window_minutes,plan_type,failed
FROM usage_events
WHERE cycle_id=? AND quota_scope=? AND used_percent IS NOT NULL AND reset_at>0 AND window_minutes>0
ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END DESC,id DESC
LIMIT ?`, cycleID, mainQuotaScope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]recordedQuotaEvent, 0, limit)
	for rows.Next() {
		var item recordedQuotaEvent
		if err = rows.Scan(&item.ID, &item.RequestedAt, &item.ObservedAt, &item.UsedPercent, &item.ResetAt, &item.WindowMinutes, &item.PlanType, &item.Failed); err != nil {
			return nil, err
		}
		events = append(events, item)
	}
	return events, rows.Err()
}

func sameQuotaRegime(recorded recordedQuotaEvent, e event) bool {
	if recorded.ResetAt != e.ResetAt || recorded.WindowMinutes != e.WindowMinutes {
		return false
	}
	return recorded.PlanType == "" || e.PlanType == "" || recorded.PlanType == e.PlanType
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func resetCandidate(peak, used float64) bool {
	if used >= peak-resetPercentTolerance {
		return false
	}
	return used <= resetLowPercent
}

func eventCycleStart(e event, previousStart int64) int64 {
	startedAt := e.ResetAt - e.WindowMinutes*60
	if startedAt <= previousStart || startedAt <= 0 || startedAt > e.RequestedAt+60 {
		return e.RequestedAt
	}
	return startedAt
}

func openCycle(ctx context.Context, tx *sql.Tx, account string) (quotaCycle, error) {
	var cycle quotaCycle
	err := tx.QueryRowContext(ctx, `SELECT id,started_at,ended_at,reset_at,window_minutes,plan_type,close_reason,first_sample_at,last_sample_at,start_used_percent,end_used_percent,peak_used_percent FROM quota_cycles WHERE account=? AND ended_at=0 ORDER BY id DESC LIMIT 1`, account).
		Scan(&cycle.ID, &cycle.StartedAt, &cycle.EndedAt, &cycle.ResetAt, &cycle.WindowMinutes, &cycle.PlanType, &cycle.CloseReason, &cycle.FirstSampleAt, &cycle.LastSampleAt, &cycle.StartPercent, &cycle.EndPercent, &cycle.PeakPercent)
	cycle.Current = err == nil
	return cycle, err
}

func createCycle(ctx context.Context, tx *sql.Tx, account string, startedAt int64, e event) (quotaCycle, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO quota_cycles(account,started_at,reset_at,window_minutes,plan_type) VALUES(?,?,?,?,?)`, account, startedAt, e.ResetAt, e.WindowMinutes, e.PlanType)
	if err != nil {
		return quotaCycle{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return quotaCycle{}, err
	}
	return quotaCycle{ID: id, StartedAt: startedAt, ResetAt: e.ResetAt, WindowMinutes: e.WindowMinutes, PlanType: e.PlanType, Current: true}, nil
}

func closeCycle(ctx context.Context, tx *sql.Tx, cycleID, endedAt int64, reason string) error {
	_, err := tx.ExecContext(ctx, `UPDATE quota_cycles SET ended_at=?,close_reason=?,end_used_percent=peak_used_percent WHERE id=? AND ended_at=0`, endedAt, reason, cycleID)
	return err
}

func updateCycleSample(ctx context.Context, tx *sql.Tx, cycleID, sampledAt int64, used float64, resetAt, windowMinutes int64, planType string) error {
	_, err := tx.ExecContext(ctx, `UPDATE quota_cycles SET
first_sample_at=CASE WHEN first_sample_at=0 THEN ? ELSE first_sample_at END,
last_sample_at=?,
start_used_percent=CASE WHEN first_sample_at=0 THEN ? ELSE start_used_percent END,
end_used_percent=?,
peak_used_percent=MAX(peak_used_percent,?),
reset_at=?,window_minutes=?,plan_type=CASE WHEN ?='' THEN plan_type ELSE ? END
WHERE id=?`, sampledAt, sampledAt, used, used, used, resetAt, windowMinutes, planType, planType, cycleID)
	return err
}

func insertSampleFromRecordedEvent(ctx context.Context, tx *sql.Tx, cycle quotaCycle, eventID int64, e event) error {
	if e.UsedPercent == nil {
		return nil
	}
	var tokens, requests int64
	var cost float64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*) FROM usage_events WHERE cycle_id=? AND quota_scope=? AND requested_at<=?`, cycle.ID, mainQuotaScope, e.RequestedAt).Scan(&tokens, &cost, &requests); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events WHERE id=? AND cycle_id=?`, eventID, cycle.ID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,plan_type,window_tokens,window_cost_usd,requests) VALUES(?,?,?,?,?,?,?,?,?,?)`, cycle.ID, e.RequestedAt, e.Account, *e.UsedPercent, e.ResetAt, e.WindowMinutes, e.PlanType, tokens, cost, requests); err != nil {
		return err
	}
	return updateCycleSample(ctx, tx, cycle.ID, e.RequestedAt, *e.UsedPercent, e.ResetAt, e.WindowMinutes, e.PlanType)
}

func (s *store) cycles(ctx context.Context, account string, limit int) ([]quotaCycle, error) {
	if limit <= 0 || limit > 1000 {
		limit = 60
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id,c.started_at,c.ended_at,c.reset_at,c.window_minutes,c.plan_type,c.close_reason,c.first_sample_at,c.last_sample_at,c.start_used_percent,c.end_used_percent,c.peak_used_percent,
COALESCE(SUM(u.total_tokens),0),COALESCE(SUM(u.cost_usd),0),COUNT(u.id)
FROM quota_cycles c
LEFT JOIN usage_events u ON u.cycle_id=c.id AND u.quota_scope='main'
WHERE c.account=?
GROUP BY c.id
ORDER BY c.started_at DESC,c.id DESC
LIMIT ?`, account, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cycles []quotaCycle
	for rows.Next() {
		var cycle quotaCycle
		if err = rows.Scan(&cycle.ID, &cycle.StartedAt, &cycle.EndedAt, &cycle.ResetAt, &cycle.WindowMinutes, &cycle.PlanType, &cycle.CloseReason, &cycle.FirstSampleAt, &cycle.LastSampleAt, &cycle.StartPercent, &cycle.EndPercent, &cycle.PeakPercent, &cycle.ActualTokens, &cycle.ActualCostUSD, &cycle.Requests); err != nil {
			return nil, err
		}
		cycle.Current = cycle.EndedAt == 0
		cycle.ObservedComplete = cycle.FirstSampleAt > 0 && cycle.FirstSampleAt <= cycle.StartedAt+15*60 && cycle.StartPercent <= resetLowPercent
		cycles = append(cycles, cycle)
	}
	return cycles, rows.Err()
}

func (s *store) pointsForCycle(ctx context.Context, account string, cycleID int64, limit int) ([]quotaPoint, string, error) {
	if limit <= 0 || limit > 10000 {
		limit = 2000
	}
	var cycle quotaCycle
	err := s.db.QueryRowContext(ctx, `SELECT id,started_at,ended_at,reset_at,window_minutes,plan_type FROM quota_cycles WHERE id=? AND account=?`, cycleID, account).
		Scan(&cycle.ID, &cycle.StartedAt, &cycle.EndedAt, &cycle.ResetAt, &cycle.WindowMinutes, &cycle.PlanType)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sampled_at,used_percent,reset_at,window_minutes,window_tokens,window_cost_usd,requests FROM quota_samples WHERE cycle_id=? ORDER BY sampled_at ASC LIMIT ?`, cycleID, limit)
	if err != nil {
		return nil, "", err
	}
	var points []quotaPoint
	for rows.Next() {
		point := quotaPoint{CycleID: cycle.ID, CycleStart: cycle.StartedAt}
		if err = rows.Scan(&point.Time, &point.UsedPercent, &point.ResetAt, &point.WindowMinutes, &point.WindowTokens, &point.WindowCostUSD, &point.Requests); err != nil {
			rows.Close()
			return nil, "", err
		}
		points = append(points, point)
	}
	if err = rows.Close(); err != nil {
		return nil, "", err
	}
	if len(points) > 0 {
		last := points[len(points)-1]
		live := quotaPoint{CycleID: cycle.ID, CycleStart: cycle.StartedAt, UsedPercent: last.UsedPercent, ResetAt: cycle.ResetAt, WindowMinutes: cycle.WindowMinutes}
		if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(requested_at),0),COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*) FROM usage_events WHERE cycle_id=? AND quota_scope=?`, cycle.ID, mainQuotaScope).Scan(&live.Time, &live.WindowTokens, &live.WindowCostUSD, &live.Requests); err != nil {
			return nil, "", err
		}
		if live.Time > last.Time {
			points = append(points, live)
		}
	}
	return points, cycle.PlanType, nil
}

func (s *store) pointsForRange(ctx context.Context, account string, startAt, endAt int64, limit int) ([]quotaPoint, []quotaCycle, error) {
	if startAt <= 0 || endAt <= startAt {
		return nil, nil, fmt.Errorf("invalid chart range")
	}
	if limit <= 0 || limit > 10000 {
		limit = 5000
	}
	allCycles, err := s.cycles(ctx, account, 1000)
	if err != nil {
		return nil, nil, err
	}
	var selected []quotaCycle
	for index := len(allCycles) - 1; index >= 0; index-- {
		cycle := allCycles[index]
		cycleEnd := cycle.EndedAt
		if cycleEnd == 0 {
			cycleEnd = cycle.ResetAt
		}
		if cycle.StartedAt <= endAt && (cycleEnd == 0 || cycleEnd >= startAt) {
			selected = append(selected, cycle)
		}
	}
	var out []quotaPoint
	for _, cycle := range selected {
		points, _, errPoints := s.pointsForCycle(ctx, account, cycle.ID, 10000)
		if errPoints != nil {
			return nil, nil, errPoints
		}
		for _, point := range points {
			if point.Time < startAt || point.Time > endAt {
				continue
			}
			out = append(out, point)
			if len(out) >= limit {
				return out, selected, nil
			}
		}
	}
	return out, selected, nil
}

func shanghaiLocation() *time.Location {
	return time.FixedZone("Asia/Shanghai", 8*60*60)
}
