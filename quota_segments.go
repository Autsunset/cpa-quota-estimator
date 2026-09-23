package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	segmentBackfillKey = "quota_segments_v1"
	segmentGapSeconds  = int64(2 * time.Hour / time.Second)
)

type segmentKey struct {
	Account string
	Window  string
	CycleID int64
	ResetAt int64 // used only for scopes without persisted quota_cycles IDs
}

type segmentEvent struct {
	ID            int64
	RequestedAt   int64
	ObservedAt    int64
	Model         string
	ServiceTier   string
	InputTokens   int64
	CacheRead     int64
	CacheWrite    int64
	OutputTokens  int64
	Failed        bool
	UsedPercent   float64
	HasUsed       bool
	ResetAt       int64
	WindowMinutes int64
}

func (e segmentEvent) pointTime() int64 {
	if e.ObservedAt > 0 {
		return e.ObservedAt
	}
	return e.RequestedAt
}

type segmentFeature struct {
	Model  string `json:"model"`
	Type   string `json:"type"`
	Fast   bool   `json:"fast,omitempty"`
	Long   bool   `json:"long,omitempty"`
	Tokens int64  `json:"tokens"`
}

type quotaSegment struct {
	ID             int64            `json:"id"`
	Account        string           `json:"account"`
	Window         string           `json:"window"`
	CycleID        int64            `json:"cycle_id"`
	Lag            int              `json:"lag"`
	StartEventID   int64            `json:"start_event_id"`
	EndEventID     int64            `json:"end_event_id"`
	StartAt        int64            `json:"start_at"`
	EndAt          int64            `json:"end_at"`
	DP             float64          `json:"dp"`
	BoundaryWeight float64          `json:"boundary_weight"`
	Features       []segmentFeature `json:"features"`
	Flags          []string         `json:"flags"`
}

func (s quotaSegment) eligible() bool { return len(s.Flags) == 0 }

func segmentKeysForEvent(e event, cycleID int64) []segmentKey {
	scope := eventQuotaScope(e)
	keys := make([]segmentKey, 0, 2)
	if e.UsedPercent != nil && e.ResetAt > 0 && e.WindowMinutes > 0 {
		key := segmentKey{Account: e.Account, Window: scope, CycleID: e.ResetAt, ResetAt: e.ResetAt}
		if scope == mainQuotaScope && cycleID > 0 {
			key.CycleID, key.ResetAt = cycleID, 0
		}
		keys = append(keys, key)
	}
	if e.SecondaryUsedPercent != nil && e.SecondaryResetAt > 0 && isFiveHourWindow(e.WindowMinutes) && isWeeklyWindow(e.SecondaryWindowMinutes) {
		window := weeklyQuotaScope
		if scope == sparkQuotaScope {
			window = sparkWeeklyQuotaScope
		}
		keys = append(keys, segmentKey{Account: e.Account, Window: window, CycleID: e.SecondaryResetAt, ResetAt: e.SecondaryResetAt})
	}
	return keys
}

func (s *store) migrateSegments() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS quota_segments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account TEXT NOT NULL, window TEXT NOT NULL, cycle_id INTEGER NOT NULL, lag INTEGER NOT NULL,
		start_event_id INTEGER NOT NULL, end_event_id INTEGER NOT NULL,
		start_at INTEGER NOT NULL, end_at INTEGER NOT NULL, dp REAL NOT NULL,
		boundary_weight REAL NOT NULL, features_json TEXT NOT NULL, flags_json TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE(account,window,cycle_id,lag,start_event_id,end_event_id)
	);
	CREATE INDEX IF NOT EXISTS idx_segments_account_window_time ON quota_segments(account,window,end_at);
	CREATE TABLE IF NOT EXISTS quota_segment_progress (
		account TEXT NOT NULL, window TEXT NOT NULL, cycle_id INTEGER NOT NULL,
		peak_integer INTEGER NOT NULL, last_observed_at INTEGER NOT NULL,
		PRIMARY KEY(account,window,cycle_id)
	);
	CREATE TABLE IF NOT EXISTS weight_fits (
		id INTEGER PRIMARY KEY AUTOINCREMENT, fitted_at INTEGER NOT NULL,
		lag INTEGER NOT NULL, segment_count INTEGER NOT NULL,
		fit_json TEXT NOT NULL, backtest_json TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_weight_fits_time ON weight_fits(fitted_at DESC);`); err != nil {
		return err
	}
	var value string
	err := s.db.QueryRow(`SELECT value FROM metadata WHERE key=?`, segmentBackfillKey).Scan(&value)
	if err == nil && value == "complete" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	return s.rebuildAllQuotaSegments(context.Background())
}

func (s *store) rebuildAllQuotaSegments(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT account,quota_scope,cycle_id,reset_at,window_minutes,
		used_percent,secondary_used_percent,secondary_reset_at,secondary_window_minutes
		FROM usage_events WHERE used_percent IS NOT NULL OR secondary_used_percent IS NOT NULL`)
	if err != nil {
		return err
	}
	keys := make(map[segmentKey]struct{})
	for rows.Next() {
		var e event
		var cycleID int64
		var used, secondary sql.NullFloat64
		if err = rows.Scan(&e.Account, &e.QuotaScope, &cycleID, &e.ResetAt, &e.WindowMinutes,
			&used, &secondary, &e.SecondaryResetAt, &e.SecondaryWindowMinutes); err != nil {
			rows.Close()
			return err
		}
		if used.Valid {
			v := used.Float64
			e.UsedPercent = &v
		}
		if secondary.Valid {
			v := secondary.Float64
			e.SecondaryUsedPercent = &v
		}
		for _, key := range segmentKeysForEvent(e, cycleID) {
			keys[key] = struct{}{}
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM quota_segments; DELETE FROM quota_segment_progress`); err != nil {
		return err
	}
	known, err := knownSegmentModels(ctx, tx)
	if err != nil {
		return err
	}
	for key := range keys {
		if err = rebuildQuotaSegmentsForKey(ctx, tx, key, known, 272000); err != nil {
			return fmt.Errorf("rebuild %s/%s/%d: %w", key.Account, key.Window, key.CycleID, err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, segmentBackfillKey, "complete"); err != nil {
		return err
	}
	return tx.Commit()
}

func knownSegmentModels(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT model FROM model_prices WHERE input>0 OR output>0 OR cache_read>0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := make(map[string]bool)
	for rows.Next() {
		var model string
		if err = rows.Scan(&model); err != nil {
			return nil, err
		}
		known[normalizeModel(model)] = true
	}
	return known, rows.Err()
}

func segmentSource(key segmentKey) (string, []any) {
	base := `SELECT id,requested_at,observed_at,model,service_tier,input_tokens,cache_read_tokens,
		cache_write_tokens,output_tokens,failed,used_percent,reset_at,window_minutes,
		secondary_used_percent,secondary_reset_at,secondary_window_minutes
		FROM usage_events WHERE account=? AND quota_scope=?`
	args := []any{key.Account, key.Window}
	switch key.Window {
	case mainQuotaScope:
		if key.ResetAt == 0 {
			base += ` AND cycle_id=?`
			args = append(args, key.CycleID)
		} else {
			base += ` AND cycle_id=0 AND reset_at=?`
			args = append(args, key.ResetAt)
		}
	case sparkQuotaScope:
		base += ` AND reset_at=?`
		args = append(args, key.ResetAt)
	case weeklyQuotaScope, sparkWeeklyQuotaScope:
		actualScope := mainQuotaScope
		if key.Window == sparkWeeklyQuotaScope {
			actualScope = sparkQuotaScope
		}
		args[1] = actualScope
		// Requests without a Secondary header still consume the same account
		// window. Keep them as features; only matching headers close crossings.
		base += ` AND requested_at>=? AND requested_at<?`
		args = append(args, key.ResetAt-weeklyWindowMinutes*60, key.ResetAt)
	}
	base += ` ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,id`
	return base, args
}

func loadSegmentEvents(ctx context.Context, tx *sql.Tx, key segmentKey) ([]segmentEvent, error) {
	query, args := segmentSource(key)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]segmentEvent, 0, 256)
	for rows.Next() {
		var e segmentEvent
		var used, secondary sql.NullFloat64
		var secondaryReset, secondaryWindow int64
		if err = rows.Scan(&e.ID, &e.RequestedAt, &e.ObservedAt, &e.Model, &e.ServiceTier,
			&e.InputTokens, &e.CacheRead, &e.CacheWrite, &e.OutputTokens, &e.Failed,
			&used, &e.ResetAt, &e.WindowMinutes, &secondary, &secondaryReset, &secondaryWindow); err != nil {
			return nil, err
		}
		if key.Window == weeklyQuotaScope || key.Window == sparkWeeklyQuotaScope {
			e.ResetAt, e.WindowMinutes, used = secondaryReset, secondaryWindow, secondary
			if secondaryReset != key.ResetAt {
				used.Valid = false
			}
		}
		e.HasUsed, e.UsedPercent = used.Valid, used.Float64
		events = append(events, e)
	}
	return events, rows.Err()
}

func rebuildQuotaSegmentsForKey(ctx context.Context, tx *sql.Tx, key segmentKey, known map[string]bool, longThreshold int64) error {
	events, err := loadSegmentEvents(ctx, tx, key)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM quota_segments WHERE account=? AND window=? AND cycle_id=?`, key.Account, key.Window, key.CycleID); err != nil {
		return err
	}
	observations := make([]quotaRegimeObservation, 0, len(events))
	for _, e := range events {
		if e.HasUsed && !e.Failed {
			observations = append(observations, quotaRegimeObservation{RequestedAt: e.pointTime(), UsedPercent: e.UsedPercent, ResetAt: e.ResetAt, WindowMinutes: e.WindowMinutes})
		}
	}
	anomalies := detectQuotaRegimeAnomalies(key.CycleID, observations)
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO quota_segments
		(account,window,cycle_id,lag,start_event_id,end_event_id,start_at,end_at,dp,boundary_weight,features_json,flags_json,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for lag := 0; lag <= 2; lag++ {
		for _, segment := range buildQuotaSegments(key, events, lag, known, longThreshold, anomalies) {
			features, errJSON := json.Marshal(segment.Features)
			if errJSON != nil {
				return errJSON
			}
			flags, errJSON := json.Marshal(segment.Flags)
			if errJSON != nil {
				return errJSON
			}
			if _, err = stmt.ExecContext(ctx, segment.Account, segment.Window, segment.CycleID, segment.Lag,
				segment.StartEventID, segment.EndEventID, segment.StartAt, segment.EndAt,
				segment.DP, segment.BoundaryWeight, string(features), string(flags), time.Now().Unix()); err != nil {
				return err
			}
		}
	}
	peak, last := int64(-1), int64(0)
	for _, e := range events {
		if e.pointTime() > last {
			last = e.pointTime()
		}
		if e.HasUsed && !e.Failed && e.UsedPercent < 100 {
			if p := int64(math.Floor(e.UsedPercent + 1e-7)); p > peak {
				peak = p
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO quota_segment_progress(account,window,cycle_id,peak_integer,last_observed_at)
		VALUES(?,?,?,?,?) ON CONFLICT(account,window,cycle_id) DO UPDATE SET
		peak_integer=excluded.peak_integer,last_observed_at=excluded.last_observed_at`,
		key.Account, key.Window, key.CycleID, peak, last)
	return err
}

func buildQuotaSegments(key segmentKey, events []segmentEvent, lag int, known map[string]bool, longThreshold int64, anomalies []quotaRegimeAnomaly) []quotaSegment {
	segments := make([]quotaSegment, 0, 100)
	anchor := -1
	anchorPercent := int64(-1)
	peak := int64(-1)
	dropSinceAnchor := false
	for index, e := range events {
		if !e.HasUsed || e.Failed || e.UsedPercent >= 100 || e.UsedPercent < 0 {
			continue
		}
		integer := int64(math.Floor(e.UsedPercent + 1e-7))
		if integer <= peak {
			if anchor >= 0 && e.UsedPercent+1 < float64(peak) {
				dropSinceAnchor = true
			}
			continue
		}
		if anchor >= 0 {
			segment := quotaSegment{
				Account: key.Account, Window: key.Window, CycleID: key.CycleID, Lag: lag,
				StartEventID: events[anchor].ID, EndEventID: e.ID,
				StartAt: events[anchor].pointTime(), EndAt: e.pointTime(),
				DP: float64(integer - anchorPercent), BoundaryWeight: 1,
				Features: []segmentFeature{}, Flags: []string{},
			}
			if dropSinceAnchor {
				segment.Flags = append(segment.Flags, "quota_drop")
			}
			for i := anchor + 1; i <= index; i++ {
				if events[i].pointTime()-events[i-1].pointTime() > segmentGapSeconds {
					segment.Flags = addSegmentFlag(segment.Flags, "long_gap")
					break
				}
			}
			start, end := anchor+1-lag, index-lag
			if start < 0 || end < start {
				segment.Flags = append(segment.Flags, "lag_incomplete")
			} else {
				features := make(map[string]*segmentFeature)
				var totalTokens int64
				baseReset := events[anchor].ResetAt
				for i := start; i <= end; i++ {
					request := events[i]
					if request.Failed {
						segment.Flags = addSegmentFlag(segment.Flags, "failed_request")
					}
					if request.ResetAt > 0 && request.ResetAt != baseReset {
						segment.Flags = addSegmentFlag(segment.Flags, "regime_change")
					}
					model := normalizeModel(request.Model)
					if !known[model] {
						if _, official := officialCodexCreditPrice(model); !official {
							segment.Flags = addSegmentFlag(segment.Flags, "unknown_model")
						}
					}
					fast := isFastTier(request.ServiceTier)
					long := request.InputTokens > longThreshold
					uncached := request.InputTokens - request.CacheRead - request.CacheWrite
					if uncached < 0 {
						uncached = 0
					}
					for _, item := range []struct {
						typ    string
						tokens int64
					}{{"input", uncached + request.CacheWrite}, {"cache", request.CacheRead}, {"output", request.OutputTokens}} {
						if item.tokens <= 0 {
							continue
						}
						key := fmt.Sprintf("%s|%s|%t|%t", model, item.typ, fast, long)
						feature := features[key]
						if feature == nil {
							feature = &segmentFeature{Model: model, Type: item.typ, Fast: fast, Long: long}
							features[key] = feature
						}
						feature.Tokens += item.tokens
						totalTokens += item.tokens
					}
				}
				for _, feature := range features {
					segment.Features = append(segment.Features, *feature)
				}
				sort.Slice(segment.Features, func(i, j int) bool {
					a, b := segment.Features[i], segment.Features[j]
					if a.Model != b.Model {
						return a.Model < b.Model
					}
					if a.Type != b.Type {
						return a.Type < b.Type
					}
					if a.Fast != b.Fast {
						return !a.Fast
					}
					return !a.Long && b.Long
				})
				if totalTokens == 0 {
					segment.Flags = addSegmentFlag(segment.Flags, "zero_tokens")
				} else {
					boundary := events[end]
					boundaryTokens := boundary.InputTokens + boundary.OutputTokens
					segment.BoundaryWeight = 1 / (1 + 2*float64(boundaryTokens)/float64(totalTokens))
				}
			}
			for _, anomaly := range anomalies {
				if segment.StartAt < anomaly.EndedAt && segment.EndAt >= anomaly.StartedAt {
					segment.Flags = addSegmentFlag(segment.Flags, "quota_anomaly")
				}
			}
			segments = append(segments, segment)
		}
		anchor, anchorPercent, peak = index, integer, integer
		dropSinceAnchor = false
	}
	return segments
}

func addSegmentFlag(flags []string, flag string) []string {
	for _, existing := range flags {
		if existing == flag {
			return flags
		}
	}
	return append(flags, flag)
}

func (s *store) refreshSegmentsForEvent(ctx context.Context, tx *sql.Tx, e event, cycleID int64) error {
	var known map[string]bool
	for _, key := range segmentKeysForEvent(e, cycleID) {
		used := e.UsedPercent
		if key.Window == weeklyQuotaScope || key.Window == sparkWeeklyQuotaScope {
			used = e.SecondaryUsedPercent
		}
		if used == nil || *used >= 100 || *used < 0 {
			continue
		}
		var peak, last int64
		err := tx.QueryRowContext(ctx, `SELECT peak_integer,last_observed_at FROM quota_segment_progress
			WHERE account=? AND window=? AND cycle_id=?`, key.Account, key.Window, key.CycleID).Scan(&peak, &last)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		integer := int64(math.Floor(*used + 1e-7))
		if err == sql.ErrNoRows || integer > peak || eventObservationTime(e) < last {
			if known == nil {
				known, err = knownSegmentModels(ctx, tx)
				if err != nil {
					return err
				}
			}
			if err = rebuildQuotaSegmentsForKey(ctx, tx, key, known, 272000); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *store) quotaSegments(ctx context.Context, lag int) ([]quotaSegment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,account,window,cycle_id,lag,start_event_id,end_event_id,
		start_at,end_at,dp,boundary_weight,features_json,flags_json FROM quota_segments WHERE lag=? ORDER BY end_at,id`, lag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	segments := make([]quotaSegment, 0, 1000)
	for rows.Next() {
		var segment quotaSegment
		var features, flags string
		if err = rows.Scan(&segment.ID, &segment.Account, &segment.Window, &segment.CycleID, &segment.Lag,
			&segment.StartEventID, &segment.EndEventID, &segment.StartAt, &segment.EndAt, &segment.DP,
			&segment.BoundaryWeight, &features, &flags); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(features), &segment.Features); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(flags), &segment.Flags); err != nil {
			return nil, err
		}
		segments = append(segments, segment)
	}
	return segments, rows.Err()
}
