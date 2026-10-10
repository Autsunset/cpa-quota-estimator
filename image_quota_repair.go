package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
)

const imageQuotaRepairKey = "image_quota_scopes_v1"

type imageQuotaRepairEvent struct {
	ID, CycleID, RequestedAt, ResetAt, WindowMinutes int64
}

// Reclassify proven image-pool observations without changing raw usage values.
// Scope, samples, cycle totals and the completion marker commit together.
func (s *store) repairImageQuotaScopes(ctx context.Context) error {
	var marker string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, imageQuotaRepairKey).Scan(&marker)
	if err == nil && marker == "complete" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,cycle_id,requested_at,model,alias,reset_at,window_minutes,codex_headers_json
FROM usage_events WHERE quota_scope IN (?,?) AND (window_minutes=1440 OR LOWER(codex_headers_json) LIKE '%imagegen%')`, mainQuotaScope, sparkQuotaScope)
	if err != nil {
		return err
	}
	var images []imageQuotaRepairEvent
	cycles := make(map[int64][]imageQuotaRepairEvent)
	for rows.Next() {
		var e imageQuotaRepairEvent
		var model, alias, raw string
		if err = rows.Scan(&e.ID, &e.CycleID, &e.RequestedAt, &model, &alias, &e.ResetAt, &e.WindowMinutes, &raw); err != nil {
			rows.Close()
			return err
		}
		var headers http.Header
		_ = json.Unmarshal([]byte(raw), &headers)
		if quotaScopeForObservation(model, alias, header(headers, "X-Codex-Active-Limit"), e.WindowMinutes) != imageQuotaScope {
			continue
		}
		images = append(images, e)
		if e.CycleID != 0 {
			cycles[e.CycleID] = append(cycles[e.CycleID], e)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range images {
		if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET quota_scope=?,learned_quota_pct=NULL,learned_secondary_pct=NULL WHERE id=?`, imageQuotaScope, e.ID); err != nil {
			return err
		}
	}
	// Do not remove a coincident sample corroborated by a real main-pool event.
	if len(images) > 0 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM quota_samples WHERE EXISTS (
	SELECT 1 FROM usage_events e WHERE e.quota_scope=? AND e.cycle_id=quota_samples.cycle_id
	AND e.account=quota_samples.account AND e.requested_at=quota_samples.sampled_at
	AND e.reset_at=quota_samples.reset_at AND e.window_minutes=quota_samples.window_minutes
	AND ABS(e.used_percent-quota_samples.used_percent)<0.000001
) AND NOT EXISTS (
	SELECT 1 FROM usage_events e WHERE e.quota_scope=? AND e.cycle_id=quota_samples.cycle_id
	AND e.account=quota_samples.account AND e.requested_at=quota_samples.sampled_at
	AND e.reset_at=quota_samples.reset_at AND e.window_minutes=quota_samples.window_minutes
	AND ABS(e.used_percent-quota_samples.used_percent)<0.000001
)`, imageQuotaScope, mainQuotaScope); err != nil {
			return err
		}
		for _, e := range images {
			if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=0 WHERE id=?`, e.ID); err != nil {
				return err
			}
		}
		for cycleID, removed := range cycles {
			if err = repairImageQuotaCycle(ctx, tx, cycleID, removed); err != nil {
				return err
			}
		}
		// Derived image regimes and any fit that used them must be rebuilt.
		if _, err = tx.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, segmentBackfillKey); err != nil {
			return err
		}
		for _, table := range []string{"weight_fits", "online_cycle_scales"} {
			var exists int
			if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); err != nil {
				return err
			}
			if exists > 0 {
				if _, err = tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
					return err
				}
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,'complete')
ON CONFLICT(key) DO UPDATE SET value='complete'`, imageQuotaRepairKey); err != nil {
		return err
	}
	return tx.Commit()
}

func repairImageQuotaCycle(ctx context.Context, tx *sql.Tx, cycleID int64, removed []imageQuotaRepairEvent) error {
	var resetAt, window int64
	if err := tx.QueryRowContext(ctx, `SELECT reset_at,window_minutes FROM quota_cycles WHERE id=?`, cycleID).Scan(&resetAt, &window); err != nil {
		return err
	}
	var e event
	var firstAt, firstReset, firstWindow int64
	err := tx.QueryRowContext(ctx, `SELECT requested_at,reset_at,window_minutes FROM usage_events
WHERE cycle_id=? AND quota_scope=? AND failed=0 AND used_percent IS NOT NULL AND reset_at>0 AND window_minutes>0
ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,id LIMIT 1`, cycleID, mainQuotaScope).
		Scan(&firstAt, &firstReset, &firstWindow)
	if err == sql.ErrNoRows {
		var samples int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_samples WHERE cycle_id=?`, cycleID).Scan(&samples); err != nil {
			return err
		}
		if samples == 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=0 WHERE cycle_id=?`, cycleID); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM quota_cycles WHERE id=?`, cycleID)
			return err
		}
		return refreshCycleDerivedData(ctx, tx, cycleID)
	} else if err != nil {
		return err
	}
	if err == nil {
		var plan string
		if err = tx.QueryRowContext(ctx, `SELECT reset_at,window_minutes,plan_type FROM usage_events
WHERE cycle_id=? AND quota_scope=? AND failed=0 AND used_percent IS NOT NULL AND reset_at>0 AND window_minutes>0
ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END DESC,id DESC LIMIT 1`, cycleID, mainQuotaScope).
			Scan(&e.ResetAt, &e.WindowMinutes, &plan); err != nil {
			return err
		}
		for _, image := range removed {
			if resetAt == image.ResetAt && window == image.WindowMinutes {
				if _, err = tx.ExecContext(ctx, `UPDATE quota_cycles SET reset_at=?,window_minutes=?,plan_type=? WHERE id=?`, e.ResetAt, e.WindowMinutes, plan, cycleID); err != nil {
					return err
				}
				resetAt, window = e.ResetAt, e.WindowMinutes
				break
			}
		}
		var imageFirst bool
		for _, image := range removed {
			imageFirst = imageFirst || image.RequestedAt < firstAt
		}
		if imageFirst {
			e.RequestedAt, e.ResetAt, e.WindowMinutes = firstAt, firstReset, firstWindow
			if _, err = tx.ExecContext(ctx, `UPDATE quota_cycles SET started_at=? WHERE id=?`, eventCycleStart(e, 0), cycleID); err != nil {
				return err
			}
		}
	}
	if err = refreshCycleDerivedData(ctx, tx, cycleID); err != nil {
		return err
	}
	return refreshCycleSampleStatsForRegime(ctx, tx, cycleID, resetAt, window)
}
