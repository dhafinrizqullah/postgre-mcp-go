package pg

import (
	"context"
	"fmt"
	"strings"
)

// Severity ranks a health finding. The tool's output is read by an LLM, so the
// ranking is the message.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Finding is one problem, or one observation, from a health check.
type Finding struct {
	Severity Severity       `json:"severity"`
	Message  string         `json:"message"`
	Details  map[string]any `json:"details,omitempty"`
}

// HealthCheck is the result of one check.
type HealthCheck struct {
	Name     string    `json:"name"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// healthChecks maps the tool's argument to its check, in report order.
var healthChecks = []struct {
	name string
	run  func(context.Context, *DB) (HealthCheck, error)
}{
	{"index", checkIndexes},
	{"connection", checkConnections},
	{"vacuum", checkVacuum},
	{"sequence", checkSequences},
	{"replication", checkReplication},
	{"buffer", checkBuffers},
	{"constraint", checkConstraints},
}

// Health runs the named checks, or all of them when names is empty or "all".
func (db *DB) Health(ctx context.Context, names []string) ([]HealthCheck, error) {
	wanted, err := parseHealthTypes(names)
	if err != nil {
		return nil, err
	}
	out := make([]HealthCheck, 0, len(wanted))
	for _, check := range healthChecks {
		if !wanted[check.name] {
			continue
		}
		result, err := check.run(ctx, db)
		if err != nil {
			// One failing check must not hide the other six.
			result = HealthCheck{
				Name:     check.name,
				Summary:  "check failed",
				Findings: []Finding{{Severity: SeverityCritical, Message: err.Error()}},
			}
		}
		out = append(out, result)
	}
	return out, nil
}

func parseHealthTypes(names []string) (map[string]bool, error) {
	all := make(map[string]bool, len(healthChecks))
	for _, check := range healthChecks {
		all[check.name] = true
	}
	if len(names) == 0 {
		return all, nil
	}
	wanted := map[string]bool{}
	for _, raw := range names {
		for _, name := range strings.Split(raw, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if name == "all" {
				return all, nil
			}
			if !all[name] {
				return nil, fmt.Errorf("unknown health check %q: use %s, or all",
					name, strings.Join(healthCheckNames(), ", "))
			}
			wanted[name] = true
		}
	}
	if len(wanted) == 0 {
		return all, nil
	}
	return wanted, nil
}

func healthCheckNames() []string {
	names := make([]string, 0, len(healthChecks))
	for _, check := range healthChecks {
		names = append(names, check.name)
	}
	return names
}

// checkIndexes reports indexes that cost write throughput and return nothing:
// never scanned, duplicated by another index, or full of dead entries.
func checkIndexes(ctx context.Context, db *DB) (HealthCheck, error) {
	unused, err := db.Query(ctx, `
		SELECT
			ui.schemaname, ui.relname AS tablename, ui.indexrelname AS indexname,
			pg_relation_size(ui.indexrelid) AS index_size,
			ui.idx_scan
		FROM pg_stat_user_indexes ui
		JOIN pg_index i ON i.indexrelid = ui.indexrelid
		WHERE NOT i.indisunique
		  AND ui.idx_scan = 0
		  AND pg_relation_size(ui.indexrelid) > 5 * 1024 * 1024
		ORDER BY pg_relation_size(ui.indexrelid) DESC
		LIMIT 20`)
	if err != nil {
		return HealthCheck{}, err
	}

	// Two indexes on the same columns with the same method are interchangeable.
	// Comparing the key signature catches cases the index definitions do not,
	// such as differing INCLUDE lists or nulls ordering.
	duplicates, err := db.Query(ctx, `
		SELECT
			n.nspname AS schemaname,
			t.relname AS tablename,
			array_agg(ic.relname ORDER BY ic.relname) AS indexnames,
			count(*) AS copies
		FROM pg_index i
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		JOIN pg_am am ON am.oid = ic.relam
		WHERE NOT i.indisprimary
		GROUP BY n.nspname, t.relname, i.indkey, am.amname
		HAVING count(*) > 1
		ORDER BY copies DESC, n.nspname, t.relname
		LIMIT 20`)
	if err != nil {
		return HealthCheck{}, err
	}

	findings := make([]Finding, 0, len(unused)+len(duplicates))

	// Sparse leaf pages are wasted space, and autovacuum never reclaims them.
	// pgstattuple is the only accurate source and it is not always installed, so
	// this sub-check degrades instead of guessing, and a failure here must not
	// hide the unused and duplicate findings.
	// avg_leaf_density and leaf_fragmentation are the two columns that survived
	// pgstatindex's signature change in PostgreSQL 18, so one query works on
	// every supported version.
	var bloated []map[string]any
	if hasPgstattuple, err := db.HasExtension(ctx, "pgstattuple"); err == nil && hasPgstattuple {
		bloated, err = db.Query(ctx, `
			SELECT
				n.nspname AS schemaname,
				t.relname AS tablename,
				ic.relname AS indexname,
				pg_relation_size(ic.oid) AS index_size,
				s.avg_leaf_density, s.leaf_fragmentation
			FROM pg_index i
			JOIN pg_class ic ON ic.oid = i.indexrelid
			JOIN pg_class t ON t.oid = i.indrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			JOIN LATERAL pgstatindex(ic.oid) s ON true
			WHERE pg_relation_size(ic.oid) > 5 * 1024 * 1024
			  AND (s.avg_leaf_density < 50 OR s.leaf_fragmentation > 50)
			ORDER BY s.avg_leaf_density ASC
			LIMIT 20`)
		if err != nil {
			bloated = nil
			findings = append(findings, Finding{
				Severity: SeverityInfo,
				Message:  "index bloat not checked: " + err.Error(),
			})
		}
	} else {
		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Message:  "index bloat not checked: install pgstattuple to measure leaf density",
		})
	}

	for _, row := range unused {
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Message: fmt.Sprintf("index %s on %s.%s has never been scanned and costs %s per write",
				str(row, "indexname"), str(row, "schemaname"), str(row, "tablename"),
				humanBytes(intOf(row, "index_size"))),
			Details: row,
		})
	}
	for _, row := range duplicates {
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Message: fmt.Sprintf("%d interchangeable indexes on %s.%s: %v",
				intOf(row, "copies"), str(row, "schemaname"), str(row, "tablename"), row["indexnames"]),
			Details: row,
		})
	}
	for _, row := range bloated {
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Message: fmt.Sprintf("index %s on %s.%s is bloated: %.0f%% leaf density, %.0f%% fragmentation in %s",
				str(row, "indexname"), str(row, "schemaname"), str(row, "tablename"),
				floatOf(row, "avg_leaf_density"), floatOf(row, "leaf_fragmentation"),
				humanBytes(intOf(row, "index_size"))),
			Details: row,
		})
	}
	return HealthCheck{
		Name:     "index",
		Summary:  summarize(findings, "no unused, duplicate, or bloated indexes found"),
		Findings: findings,
	}, nil
}

// checkConnections reports utilisation against max_connections, which is the
// setting that actually takes a database down.
func checkConnections(ctx context.Context, db *DB) (HealthCheck, error) {
	row, err := db.QueryOne(ctx, `
		SELECT
			(SELECT setting::int FROM pg_settings WHERE name = 'max_connections') AS max_connections,
			(SELECT count(*) FROM pg_stat_activity) AS total,
			(SELECT count(*) FROM pg_stat_activity WHERE state = 'active') AS active,
			(SELECT count(*) FROM pg_stat_activity WHERE state = 'idle') AS idle,
			(SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock') AS waiting_on_lock,
			(SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction') AS idle_in_transaction`)
	if err != nil {
		return HealthCheck{}, err
	}
	if row == nil {
		return HealthCheck{}, fmt.Errorf("cannot read connection statistics")
	}

	maxConns := intOf(row, "max_connections")
	total := intOf(row, "total")
	used := float64(total) / float64(maxConns)

	findings := []Finding{}
	switch {
	case used >= 0.9:
		findings = append(findings, Finding{
			Severity: SeverityCritical,
			Message:  fmt.Sprintf("connection slots nearly exhausted: %d of %d in use (%.0f%%)", total, maxConns, used*100),
			Details:  row,
		})
	case used >= 0.7:
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("connection usage is high: %d of %d in use (%.0f%%)", total, maxConns, used*100),
			Details:  row,
		})
	}
	if waiting := intOf(row, "waiting_on_lock"); waiting > 0 {
		findings = append(findings, Finding{
			Severity: SeverityCritical,
			Message:  fmt.Sprintf("%d sessions are waiting on a lock", waiting),
			Details:  row,
		})
	}
	if stuck := intOf(row, "idle_in_transaction"); stuck > 0 {
		findings = append(findings, Finding{
			Severity: SeverityWarning,
			Message: fmt.Sprintf("%d sessions are idle in transaction and are holding back the vacuum horizon",
				stuck),
			Details: row,
		})
	}
	return HealthCheck{
		Name:     "connection",
		Summary:  summarize(findings, fmt.Sprintf("connections healthy: %d of %d in use (%.0f%%)", total, maxConns, used*100)),
		Findings: findings,
	}, nil
}

// checkVacuum reports tables approaching transaction id wraparound, which
// eventually makes Postgres refuse all writes.
func checkVacuum(ctx context.Context, db *DB) (HealthCheck, error) {
	rows, err := db.Query(ctx, `
		SELECT
			n.nspname AS schemaname,
			c.relname AS tablename,
			age(c.relfrozenxid) AS xid_age,
			pg_size_pretty(pg_total_relation_size(c.oid)) AS total_size
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'm')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY age(c.relfrozenxid) DESC
		LIMIT 10`)
	if err != nil {
		return HealthCheck{}, err
	}

	limit := int64(200_000_000) // autovacuum_freeze_max_age default
	row, err := db.QueryOne(ctx, `
		SELECT setting::bigint AS freeze_max_age FROM pg_settings WHERE name = 'autovacuum_freeze_max_age'`)
	if err != nil {
		return HealthCheck{}, err
	}
	if row != nil {
		if configured := intOf(row, "freeze_max_age"); configured > 0 {
			limit = configured
		}
	}

	findings := make([]Finding, 0, len(rows))
	for _, r := range rows {
		age := intOf(r, "xid_age")
		switch {
		case age >= limit:
			findings = append(findings, Finding{
				Severity: SeverityCritical,
				Message: fmt.Sprintf("%s.%s must be vacuumed: transaction id age %s is past autovacuum_freeze_max_age %s",
					str(r, "schemaname"), str(r, "tablename"), humanCount(age), humanCount(limit)),
				Details: r,
			})
		case age >= limit/2:
			findings = append(findings, Finding{
				Severity: SeverityWarning,
				Message: fmt.Sprintf("%s.%s is halfway to wraparound: transaction id age %s",
					str(r, "schemaname"), str(r, "tablename"), humanCount(age)),
				Details: r,
			})
		}
	}
	return HealthCheck{
		Name:     "vacuum",
		Summary:  summarize(findings, "no table is close to transaction id wraparound"),
		Findings: findings,
	}, nil
}

// checkSequences reports sequences that will run out. A sequence that wraps is a
// failed INSERT, not a slowdown, so this is the check worth alerting on.
func checkSequences(ctx context.Context, db *DB) (HealthCheck, error) {
	rows, err := db.Query(ctx, `
		SELECT
			s.schemaname, s.sequencename,
			s.last_value, s.max_value, s.increment_by, s.cycle
		FROM pg_sequences s
		WHERE s.last_value IS NOT NULL
		ORDER BY (s.max_value - s.last_value) ASC
		LIMIT 10`)
	if err != nil {
		return HealthCheck{}, err
	}

	findings := make([]Finding, 0, len(rows))
	for _, r := range rows {
		last, max := intOf(r, "last_value"), intOf(r, "max_value")
		if max <= 0 || last >= max {
			continue
		}
		remaining := max - last
		// A sequence within 10% of its ceiling, or with under 1000 values left.
		if remaining <= 1000 || float64(remaining) < 0.1*float64(max) {
			cycles := ""
			if !boolean(r, "cycle") {
				cycles = " (does not cycle)"
			}
			severity := SeverityWarning
			if cycles != "" {
				severity = SeverityCritical
			}
			findings = append(findings, Finding{
				Severity: severity,
				Message: fmt.Sprintf("sequence %s.%s has %s values left before max_value %s%s",
					str(r, "schemaname"), str(r, "sequencename"), humanCount(remaining), humanCount(max), cycles),
				Details: r,
			})
		}
	}
	return HealthCheck{
		Name:     "sequence",
		Summary:  summarize(findings, "no sequence is near its maximum value"),
		Findings: findings,
	}, nil
}

// checkReplication reports replica lag and slot health. On a primary with no
// replicas, that is a valid configuration, not a problem.
func checkReplication(ctx context.Context, db *DB) (HealthCheck, error) {
	role, err := db.QueryOne(ctx, "SELECT current_setting('wal_level') AS wal_level, pg_is_in_recovery() AS in_recovery")
	if err != nil {
		return HealthCheck{}, err
	}
	if role == nil {
		return HealthCheck{}, fmt.Errorf("cannot read replication settings")
	}
	if boolean(role, "in_recovery") {
		return HealthCheck{
			Name:     "replication",
			Summary:  "this server is a replica; lag is reported by its primary",
			Findings: []Finding{},
		}, nil
	}

	rows, err := db.Query(ctx, `
		SELECT
			client_addr, application_name, state, sync_state,
			pg_wal_lsn_diff(pg_current_wal_lsn(), sent_lsn)  AS pending_bytes,
			pg_wal_lsn_diff(pg_current_wal_lsn(), write_lsn) AS pending_to_flush,
			pg_wal_lsn_diff(sent_lsn, write_lsn)             AS flush_lag,
			pg_wal_lsn_diff(sent_lsn, replay_lsn)            AS replay_lag,
			extract(epoch FROM write_lag)                    AS write_lag_seconds,
			extract(epoch FROM flush_lag)                    AS flush_lag_seconds,
			extract(epoch FROM replay_lag)                   AS replay_lag_seconds
		FROM pg_stat_replication
		ORDER BY replay_lag DESC NULLS LAST`)
	if err != nil {
		return HealthCheck{}, err
	}
	slots, err := db.Query(ctx, `
		SELECT slot_name, slot_type, active, wal_status,
		       pg_size_pretty(
		           pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained
		FROM pg_replication_slots
		ORDER BY pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) DESC NULLS LAST`)
	if err != nil {
		return HealthCheck{}, err
	}

	findings := []Finding{}
	for _, r := range rows {
		lag := floatOf(r, "replay_lag_seconds")
		state := str(r, "state")
		switch {
		case state != "streaming":
			findings = append(findings, Finding{
				Severity: SeverityCritical,
				Message:  fmt.Sprintf("replica %s is %s, not streaming", str(r, "application_name"), state),
				Details:  r,
			})
		case lag > 300:
			findings = append(findings, Finding{
				Severity: SeverityCritical,
				Message: fmt.Sprintf("replica %s is %s behind",
					str(r, "application_name"), humanSeconds(lag)),
				Details: r,
			})
		case lag > 30:
			findings = append(findings, Finding{
				Severity: SeverityWarning,
				Message: fmt.Sprintf("replica %s is %s behind",
					str(r, "application_name"), humanSeconds(lag)),
				Details: r,
			})
		}
	}
	for _, r := range slots {
		if !boolean(r, "active") {
			findings = append(findings, Finding{
				Severity: SeverityCritical,
				Message: fmt.Sprintf("replication slot %s is inactive and is retaining %s of WAL",
					str(r, "slot_name"), str(r, "retained")),
				Details: r,
			})
			continue
		}
		if status := str(r, "wal_status"); status == "lost" || status == "extended" {
			findings = append(findings, Finding{
				Severity: SeverityWarning,
				Message: fmt.Sprintf("replication slot %s has wal_status %s and retains %s",
					str(r, "slot_name"), status, str(r, "retained")),
				Details: r,
			})
		}
	}
	return HealthCheck{
		Name:     "replication",
		Summary:  summarize(findings, fmt.Sprintf("replication healthy: %d replicas, %d slots", len(rows), len(slots))),
		Findings: findings,
	}, nil
}

// checkBuffers reports the share of reads served from the shared buffer cache
// instead of the operating system. A low rate means the working set does not fit
// in memory.
func checkBuffers(ctx context.Context, db *DB) (HealthCheck, error) {
	row, err := db.QueryOne(ctx, `
		SELECT
			sum(blks_hit) AS hit,
			sum(blks_read) AS read
		FROM pg_statio_user_tables`)
	if err != nil {
		return HealthCheck{}, err
	}
	if row == nil {
		return HealthCheck{Name: "buffer", Summary: "no table statistics available yet", Findings: []Finding{}}, nil
	}
	hit, read := intOf(row, "hit"), intOf(row, "read")
	if hit+read == 0 {
		return HealthCheck{
			Name:     "buffer",
			Summary:  "no buffer statistics collected yet; they appear after the first checkpoint",
			Findings: []Finding{},
		}, nil
	}
	rate := float64(hit) / float64(hit+read)

	worst, err := db.Query(ctx, `
		SELECT relname, schemaname, blks_hit, blks_read
		FROM pg_statio_user_tables
		WHERE blks_hit + blks_read > 1000
		ORDER BY (blks_read::float / NULLIF(blks_hit + blks_read, 0)) DESC
		LIMIT 5`)
	if err != nil {
		return HealthCheck{}, err
	}

	findings := []Finding{}
	if rate < 0.99 {
		severity := SeverityWarning
		if rate < 0.95 {
			severity = SeverityCritical
		}
		findings = append(findings, Finding{
			Severity: severity,
			Message: fmt.Sprintf("buffer cache hit rate is %.2f%%, below the 99%% target (%d hits, %d reads)",
				rate*100, hit, read),
			Details: row,
		})
	}
	for _, r := range worst {
		total := intOf(r, "blks_hit") + intOf(r, "blks_read")
		if float64(intOf(r, "blks_read"))/float64(total) < 0.01 {
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityInfo,
			Message: fmt.Sprintf("%s.%s reads %.1f%% of its blocks from disk",
				str(r, "schemaname"), str(r, "relname"),
				float64(intOf(r, "blks_read"))/float64(total)*100),
			Details: r,
		})
	}
	return HealthCheck{
		Name:     "buffer",
		Summary:  summarize(findings, fmt.Sprintf("buffer cache hit rate %.2f%%", rate*100)),
		Findings: findings,
	}, nil
}

// checkConstraints reports constraints Postgres knows are invalid, usually the
// result of a failed data load. Rows violating them are already in the table.
func checkConstraints(ctx context.Context, db *DB) (HealthCheck, error) {
	rows, err := db.Query(ctx, `
		SELECT
			n.nspname AS schemaname,
			c.relname AS tablename,
			con.conname AS constraint_name,
			pg_get_constraintdef(con.oid) AS definition
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT con.convalidated
		ORDER BY n.nspname, c.relname, con.conname
		LIMIT 50`)
	if err != nil {
		return HealthCheck{}, err
	}

	findings := make([]Finding, 0, len(rows))
	for _, r := range rows {
		findings = append(findings, Finding{
			Severity: SeverityCritical,
			Message: fmt.Sprintf("constraint %s on %s.%s is NOT VALID (%s); rows violating it are already stored",
				str(r, "constraint_name"), str(r, "schemaname"), str(r, "tablename"), str(r, "definition")),
			Details: r,
		})
	}
	return HealthCheck{
		Name:     "constraint",
		Summary:  summarize(findings, "all constraints are valid"),
		Findings: findings,
	}, nil
}

func summarize(findings []Finding, clean string) string {
	if len(findings) == 0 {
		return clean
	}
	critical, warning := 0, 0
	for _, f := range findings {
		switch f.Severity {
		case SeverityCritical:
			critical++
		case SeverityWarning:
			warning++
		}
	}
	return fmt.Sprintf("%d critical, %d warning, %d info",
		critical, warning, len(findings)-critical-warning)
}
