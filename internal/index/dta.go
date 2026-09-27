package index

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/dhafinrizqullah/postgre-mcp-go/internal/pg"
)

// Options controls the search. The defaults are upstream's.
type Options struct {
	// MaxIndexSizeMB caps the total size of the recommended indexes. Zero means
	// no cap.
	MaxIndexSizeMB int
	// MaxRuntime is the anytime cutoff for the whole search.
	MaxRuntime time.Duration
	// MaxIndexWidth is the widest index considered, in columns.
	MaxIndexWidth int
	// MinColumnQueries drops a column used by fewer queries than this, which
	// keeps one-off columns from generating candidates.
	MinColumnQueries int
	// MaxQueries caps the workload.
	MaxQueries int
	// MinCalls and MinMeanTimeMS select the workload from pg_stat_statements.
	MinCalls      int
	MinMeanTimeMS float64
	// ParetoAlpha is how much a speedup is worth against the space it costs.
	// At 2, a 100x speedup is worth 10x the space.
	ParetoAlpha float64
	// MinImprovement is the relative cost reduction a candidate must show in a
	// round to be accepted, and the round-to-round gain needed to keep going.
	MinImprovement float64
}

// DefaultOptions returns the search parameters upstream ships with.
func DefaultOptions() Options {
	return Options{
		MaxIndexSizeMB:   0,
		MaxRuntime:       30 * time.Second,
		MaxIndexWidth:    3,
		MinColumnQueries: 1,
		MaxQueries:       10,
		MinCalls:         20,
		MinMeanTimeMS:    5,
		ParetoAlpha:      2.0,
		MinImprovement:   0.10,
	}
}

// Query is one statement to analyse, with the weight the workload gives it.
type Query struct {
	Text       string
	Calls      float64
	MeanTimeMS float64
}

// weight is calls times mean time: a query that is slow and frequent matters
// most, which is also the order pg_stat_statements's total_time is in.
func (q Query) weight() float64 {
	calls, mean := q.Calls, q.MeanTimeMS
	if calls <= 0 {
		calls = 1
	}
	if mean <= 0 {
		mean = 1
	}
	return calls * mean
}

// Candidate is a possible index: a table and an ordered list of columns.
type Candidate struct {
	Table   string
	Columns []string
	Using   string
}

// key identifies a configuration of indexes. Two configurations with the same key
// are the same configuration, whatever order they were chosen in.
func (c Candidate) key() string {
	return c.Table + "(" + strings.Join(c.Columns, ",") + ")"
}

// Definition renders the index in the form hypopg and CREATE INDEX both accept.
func (c Candidate) Definition() string {
	method := c.Using
	if method == "" {
		method = "btree"
	}
	return fmt.Sprintf("%s (%s (%s))", method, c.Table, strings.Join(c.Columns, ", "))
}

// Recommendation is a proposed index, with what the planner said about it.
type Recommendation struct {
	Table                  string   `json:"table"`
	Columns                []string `json:"columns"`
	Using                  string   `json:"using"`
	Definition             string   `json:"create_index"`
	EstimatedSizeBytes     int64    `json:"estimated_size_bytes"`
	EstimatedSize          string   `json:"estimated_size"`
	IndividualImprovement  float64  `json:"individual_improvement_multiple"`
	ProgressiveImprovement float64  `json:"progressive_improvement_multiple"`
	Note                   string   `json:"note,omitempty"`
}

// Result is the outcome of one analysis.
type Result struct {
	BaselineCost        float64          `json:"baseline_cost"`
	FinalCost           float64          `json:"final_cost"`
	ImprovementMultiple float64          `json:"improvement_multiple"`
	BaseRelationBytes   int64            `json:"base_relation_bytes"`
	IndexSizeBytes      int64            `json:"index_size_bytes"`
	AnalysedQueries     int              `json:"analysed_queries"`
	SkippedQueries      []string         `json:"skipped_queries,omitempty"`
	Recommendations     []Recommendation `json:"recommendations"`
	Traces              []string         `json:"traces,omitempty"`
	Elapsed             string           `json:"elapsed"`
}

// Tuner recommends indexes. Create one per analysis with New; it caches catalog
// lookups, planner costs, and size estimates for the duration.
type Tuner struct {
	db     *pg.DB
	opts   Options
	cat    *catalog
	traces []string

	// queries is the workload actually analysed, with bind parameters replaced by
	// sampled constants so the planner can be precise.
	queries []Query
	// statement holds the parsed form of each analysed query.
	statement []*pg_query.SelectStmt
	// skipped explains, per query, why it was not analysed.
	skipped []string
	// costCache maps a configuration to its weighted mean planner cost.
	costCache map[string]float64
	// sizeCache maps a candidate to its estimated size.
	sizeCache map[string]int64
	deadline  time.Time
}

// New creates a tuner. The caller supplies the options; use DefaultOptions for
// upstream's behaviour.
func New(db *pg.DB, opts Options) *Tuner {
	if opts.MaxIndexWidth <= 0 {
		opts.MaxIndexWidth = 3
	}
	if opts.MaxRuntime <= 0 {
		opts.MaxRuntime = 30 * time.Second
	}
	if opts.ParetoAlpha <= 0 {
		opts.ParetoAlpha = 2
	}
	if opts.MinImprovement <= 0 {
		opts.MinImprovement = 0.10
	}
	return &Tuner{
		db:        db,
		opts:      opts,
		cat:       newCatalog(db),
		costCache: map[string]float64{},
		sizeCache: map[string]int64{},
	}
}

// Precheck reports why the analysis cannot run, or nil if it can.
//
// hypopg is required: without it there is no way to ask the planner what an index
// would do, and guessing is worse than saying so. Statistics are required too,
// because a plan built on stale statistics recommends the wrong index.
func (t *Tuner) Precheck(ctx context.Context) error {
	installed, err := t.db.HasExtension(ctx, "hypopg")
	if err != nil {
		return err
	}
	if !installed {
		return fmt.Errorf("index tuning needs the hypopg extension: CREATE EXTENSION hypopg")
	}
	row, err := t.db.QueryOne(ctx, `
		SELECT count(*) FILTER (WHERE last_analyze IS NOT NULL) AS analyzed,
		       count(*) AS total
		FROM pg_stat_user_tables`)
	if err != nil {
		return err
	}
	if row == nil || intOf(row, "analyzed") == 0 {
		return fmt.Errorf("no table has been analyzed, so the planner has no statistics to work from: run ANALYZE first")
	}
	return nil
}

// Analyze runs the search over a workload and returns the recommended indexes.
func (t *Tuner) Analyze(ctx context.Context, workload []Query) (*Result, error) {
	started := time.Now()
	t.deadline = started.Add(t.opts.MaxRuntime)

	if len(workload) == 0 {
		return nil, fmt.Errorf("no queries to analyze")
	}
	if t.opts.MaxQueries > 0 && len(workload) > t.opts.MaxQueries {
		workload = workload[:t.opts.MaxQueries]
	}

	result := &Result{}
	t.analyse(ctx, workload)
	if len(t.queries) == 0 {
		// Nothing survived analysis. Say which queries and why, rather than
		// failing later on an empty workload.
		result.SkippedQueries = t.skipped
		result.Elapsed = time.Since(started).Round(time.Millisecond).String()
		return result, nil
	}

	tables := map[string]bool{}
	for _, q := range t.queries {
		for _, table := range TablesIn(parseSelect(q.Text)) {
			tables[table] = true
		}
	}
	if err := t.cat.load(ctx, keysOf(tables)); err != nil {
		return nil, err
	}

	candidates, err := t.candidates(ctx)
	if err != nil {
		return nil, err
	}
	t.trace("generated %d index candidates from %d queries", len(candidates), len(t.queries))

	baseline, err := t.cost(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("measuring the baseline cost: %w", err)
	}
	result.BaselineCost = baseline
	result.AnalysedQueries = len(t.queries)

	chosen, finalCost, err := t.greedy(ctx, candidates, baseline)
	if err != nil {
		return nil, err
	}
	result.FinalCost = finalCost
	if baseline > 0 {
		result.ImprovementMultiple = baseline / finalCost
	}

	for _, table := range candidateTables(chosen) {
		size, err := t.cat.TableSize(ctx, table)
		if err != nil {
			return nil, err
		}
		result.BaseRelationBytes += size
	}
	for _, c := range chosen {
		size, err := t.size(ctx, c)
		if err != nil {
			return nil, err
		}
		result.IndexSizeBytes += size
	}

	recs, err := t.recommend(ctx, chosen, baseline)
	if err != nil {
		return nil, err
	}
	result.Recommendations = recs
	result.SkippedQueries = t.skipped
	result.Traces = t.traces
	result.Elapsed = time.Since(started).Round(time.Millisecond).String()
	return result, nil
}

// analyse parses each query, drops the ones with nothing to index, and replaces
// bind parameters with sampled constants.
func (t *Tuner) analyse(ctx context.Context, workload []Query) {
	for _, q := range workload {
		sel := parseSelect(q.Text)
		if sel == nil {
			t.skipped = append(t.skipped, truncate(q.Text, 80)+": not a single SELECT statement")
			continue
		}
		tables := TablesIn(sel)
		if len(tables) == 0 || allSystemTables(tables) {
			t.skipped = append(t.skipped, truncate(q.Text, 80)+": only reads system catalogs")
			continue
		}
		sql, err := t.replaceParameters(ctx, q.Text)
		if err != nil {
			t.skipped = append(t.skipped, truncate(q.Text, 80)+": "+err.Error())
			continue
		}
		t.queries = append(t.queries, Query{Text: sql, Calls: q.Calls, MeanTimeMS: q.MeanTimeMS})
		t.statement = append(t.statement, sel)
	}
}

// candidates builds the index candidates: every combination of the filtered
// columns of each table, up to the width limit, minus what already exists.
func (t *Tuner) candidates(ctx context.Context) ([]Candidate, error) {
	// A column used by more queries is more likely to be worth indexing, so the
	// combinations are generated most-used first and the search sees the
	// promising ones early when the time budget cuts it short.
	usage := map[string]int{}
	perTable := map[string][]ColumnUse{}
	for _, sel := range t.statement {
		cols := ExtractColumns(sel, t.cat)
		for _, use := range cols.Filtered() {
			key := use.Table + "." + use.Column
			usage[key]++
			perTable[use.Table] = append(perTable[use.Table], use)
		}
	}

	seen := map[string]bool{}
	var candidates []Candidate
	for table, uses := range perTable {
		columns := map[string]bool{}
		for _, use := range uses {
			if usage[use.Table+"."+use.Column] < t.opts.MinColumnQueries {
				continue
			}
			if exists, known := t.cat.HasColumn(table, use.Column); known && !exists {
				continue
			}
			if t.cat.isLongText(table, use.Column) {
				t.trace("skipping %s.%s: avg_width is over %d bytes", table, use.Column, maxIndexedTextWidth)
				continue
			}
			columns[use.Column] = true
		}
		names := make([]string, 0, len(columns))
		for name := range columns {
			names = append(names, name)
		}
		sortByUsageThenName(names, usage, table)

		for width := 1; width <= t.opts.MaxIndexWidth && width <= len(names); width++ {
			for _, combo := range combinations(names, width) {
				if t.cat.indexExists(table, combo) {
					continue
				}
				c := Candidate{Table: table, Columns: combo, Using: "btree"}
				if seen[c.key()] {
					continue
				}
				seen[c.key()] = true
				candidates = append(candidates, c)
			}
		}
	}
	return candidates, nil
}

// greedy is the Anytime search: repeatedly add the candidate that most reduces
// the objective, until nothing clears the improvement threshold or time runs out.
//
// The objective is log(cost) + alpha*log(space). Minimising it is what turns a
// fast index that is too large into a rejected one.
func (t *Tuner) greedy(ctx context.Context, candidates []Candidate, baseline float64) ([]Candidate, float64, error) {
	baseBytes, err := t.baseRelationSize(ctx, candidates)
	if err != nil {
		return nil, 0, err
	}

	current := []Candidate{}
	remaining := slices.Clone(candidates)
	currentCost := baseline
	currentSpace := baseBytes
	currentObjective := objective(currentCost, currentSpace, t.opts.ParetoAlpha)

	budget := int64(t.opts.MaxIndexSizeMB) * 1024 * 1024

	for round := 1; ; round++ {
		if time.Now().After(t.deadline) {
			t.trace("stopping after round %d: time budget of %s reached", round, t.opts.MaxRuntime)
			break
		}
		var (
			best          *Candidate
			bestCost      = currentCost
			bestSpace     = currentSpace
			bestObjective = currentObjective
			bestGain      float64
		)
		for i := range remaining {
			candidate := remaining[i]
			indexSize, err := t.size(ctx, candidate)
			if err != nil {
				return nil, 0, err
			}
			space := currentSpace + indexSize
			if budget > 0 && space-baseBytes > budget {
				t.trace("skipping %s: index space would exceed the %d MB budget",
					candidate.key(), t.opts.MaxIndexSizeMB)
				continue
			}
			cost, err := t.cost(ctx, append(slices.Clone(current), candidate))
			if err != nil {
				return nil, 0, err
			}
			if currentCost <= 0 {
				continue
			}
			gain := (currentCost - cost) / currentCost
			if gain < t.opts.MinImprovement {
				continue
			}
			obj := objective(cost, space, t.opts.ParetoAlpha)
			// Strictly better on the objective, and the largest gain wins ties.
			if obj < bestObjective && gain > bestGain {
				best = &remaining[i]
				bestCost, bestSpace, bestObjective, bestGain = cost, space, obj, gain
			}
		}
		if best == nil {
			t.trace("stopping: no candidate improves the cost by at least %.0f%%", t.opts.MinImprovement*100)
			break
		}
		t.trace("round %d: adding %s, cost %.2f -> %.2f (%.1f%% better), space %s",
			round, best.key(), currentCost, bestCost, bestGain*100, humanBytes(bestSpace-baseBytes))

		current = append(current, *best)
		remaining = slices.DeleteFunc(remaining, func(c Candidate) bool { return c.key() == best.key() })
		currentCost, currentSpace, currentObjective = bestCost, bestSpace, bestObjective
	}
	return current, currentCost, nil
}

// recommend fills in what each accepted index is worth on its own and in
// combination, which is the difference between "add this" and "add this first".
func (t *Tuner) recommend(ctx context.Context, chosen []Candidate, baseline float64) ([]Recommendation, error) {
	recs := make([]Recommendation, 0, len(chosen))
	totalSize := int64(0)
	for i, candidate := range chosen {
		size, err := t.size(ctx, candidate)
		if err != nil {
			return nil, err
		}
		solo, err := t.cost(ctx, []Candidate{candidate})
		if err != nil {
			return nil, err
		}
		progressive, err := t.cost(ctx, chosen[:i+1])
		if err != nil {
			return nil, err
		}
		rec := Recommendation{
			Table:              candidate.Table,
			Columns:            candidate.Columns,
			Using:              "btree",
			Definition:         formatDefinition(candidate.Table, candidate.Columns, "btree"),
			EstimatedSizeBytes: size,
			EstimatedSize:      humanBytes(size),
		}
		if solo > 0 {
			rec.IndividualImprovement = baseline / solo
		}
		if progressive > 0 {
			rec.ProgressiveImprovement = baseline / progressive
		}
		totalSize += size
		recs = append(recs, rec)
	}
	return recs, nil
}

// cost measures a configuration by asking the planner, with hypopg, what it would
// cost to run the workload with exactly those indexes in place.
//
// The whole configuration is measured in one session: hypopg indexes live in the
// session, so creating them once and explaining every query is both correct and
// far cheaper than doing it per query.
func (t *Tuner) cost(ctx context.Context, config []Candidate) (float64, error) {
	key := configKey(config)
	if cached, ok := t.costCache[key]; ok {
		return cached, nil
	}
	if len(t.queries) == 0 {
		return 0, fmt.Errorf("no analysable queries")
	}

	var total float64
	var measured int
	err := t.db.WithConn(ctx, func(ctx context.Context, s *pg.Session) error {
		if err := pg.ResetHypopg(ctx, s); err != nil {
			return err
		}
		for _, candidate := range config {
			if err := pg.CreateHypopgIndex(ctx, s, candidate.Definition()); err != nil {
				return err
			}
		}
		for _, q := range t.queries {
			rows, err := s.Query(ctx, "EXPLAIN (FORMAT JSON) "+q.Text)
			if err != nil {
				return fmt.Errorf("explaining a workload query: %w", err)
			}
			cost, err := planCost(rows)
			if err != nil {
				return err
			}
			total += cost * q.weight()
			measured++
		}
		return pg.ResetHypopg(ctx, s)
	})
	if err != nil {
		return 0, err
	}
	if measured == 0 {
		return 0, fmt.Errorf("no query could be measured")
	}
	avg := total / float64(measured)
	t.costCache[key] = avg
	return avg, nil
}

// size prefers hypopg's estimate, because it is the server's own, and falls back
// to column statistics.
func (t *Tuner) size(ctx context.Context, candidate Candidate) (int64, error) {
	if size, ok := t.sizeCache[candidate.key()]; ok {
		return size, nil
	}
	var size int64
	err := t.db.WithConn(ctx, func(ctx context.Context, s *pg.Session) error {
		if err := pg.ResetHypopg(ctx, s); err != nil {
			return err
		}
		defer func() { _ = pg.ResetHypopg(ctx, s) }()
		if err := pg.CreateHypopgIndex(ctx, s, candidate.Definition()); err != nil {
			return err
		}
		rows, err := s.Query(ctx,
			`SELECT hypopg_relation_size(indexrelid) AS size FROM hypopg_list_indexes LIMIT 1`)
		if err != nil {
			return fmt.Errorf("sizing simulated index: %w", err)
		}
		if len(rows) > 0 {
			size = int64Of(rows[0]["size"])
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if size == 0 {
		size, err = t.cat.estimateIndexSize(ctx, candidate.Table, candidate.Columns)
		if err != nil {
			return 0, err
		}
	}
	t.sizeCache[candidate.key()] = size
	return size, nil
}

func (t *Tuner) baseRelationSize(ctx context.Context, candidates []Candidate) (int64, error) {
	var total int64
	for _, table := range candidateTables(candidates) {
		size, err := t.cat.TableSize(ctx, table)
		if err != nil {
			return 0, err
		}
		total += size
	}
	return total, nil
}

// planCost reads Total Cost from an EXPLAIN (FORMAT JSON) result. It is the
// planner's own estimate in cost units, which is comparable across
// configurations because it comes from the same statistics.
func planCost(rows []map[string]any) (float64, error) {
	if len(rows) == 0 {
		return 0, fmt.Errorf("EXPLAIN returned no rows")
	}
	// The value is a list holding one plan object, but the driver may hand it
	// back as decoded JSON or as text, so both are accepted.
	var payload any
	for key, value := range rows[0] {
		if strings.EqualFold(key, "QUERY PLAN") {
			payload = value
			break
		}
	}
	if payload == nil {
		return 0, fmt.Errorf("EXPLAIN result has no QUERY PLAN column")
	}

	var plans []struct {
		Plan struct {
			TotalCost float64 `json:"Total Cost"`
		} `json:"Plan"`
	}
	switch typed := payload.(type) {
	case string:
		if err := jsonUnmarshal([]byte(typed), &plans); err != nil {
			return 0, fmt.Errorf("decoding the plan: %w", err)
		}
	default:
		encoded, err := jsonBytes(payload)
		if err != nil {
			return 0, fmt.Errorf("re-encoding the plan: %w", err)
		}
		if err := jsonUnmarshal(encoded, &plans); err != nil {
			return 0, fmt.Errorf("decoding the plan: %w", err)
		}
	}
	if len(plans) == 0 {
		return 0, fmt.Errorf("EXPLAIN returned an empty plan")
	}
	cost := plans[0].Plan.TotalCost
	if cost <= 0 {
		return 0, fmt.Errorf("EXPLAIN reported a cost of %v, which means the query was not planned", cost)
	}
	return cost, nil
}

// objective is what the search minimises. Both terms are logarithms so the two
// kinds of cost trade off linearly in log space, and the constants cancel: what
// matters is the ratio between them, weighted by alpha.
func objective(cost float64, space int64, alpha float64) float64 {
	if cost <= 0 || space <= 0 {
		return math.Inf(1)
	}
	return math.Log(cost) + alpha*math.Log(float64(space))
}

func configKey(config []Candidate) string {
	keys := make([]string, 0, len(config))
	for _, c := range config {
		keys = append(keys, c.key())
	}
	slices.Sort(keys)
	return strings.Join(keys, "|")
}

func candidateTables(candidates []Candidate) []string {
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c.Table] = true
	}
	return keysOf(seen)
}

func keysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

func allSystemTables(tables []string) bool {
	for _, table := range tables {
		if !isSystemTable(table) {
			return false
		}
	}
	return true
}

func combinations(items []string, width int) [][]string {
	out := make([][]string, 0, 8)
	var walk func(start int, current []string)
	walk = func(start int, current []string) {
		if len(current) == width {
			out = append(out, slices.Clone(current))
			return
		}
		for i := start; i < len(items); i++ {
			walk(i+1, append(current, items[i]))
		}
	}
	walk(0, nil)
	return out
}

func sortByUsageThenName(names []string, usage map[string]int, table string) {
	slices.SortFunc(names, func(a, b string) int {
		ua, ub := usage[table+"."+a], usage[table+"."+b]
		if ua != ub {
			return ub - ua
		}
		return strings.Compare(a, b)
	})
}

func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func (t *Tuner) trace(format string, args ...any) {
	t.traces = append(t.traces, fmt.Sprintf(format, args...))
}

// parseSelect parses a statement, returning nil when it is not a single SELECT.
func parseSelect(sql string) *pg_query.SelectStmt {
	tree, err := pg_query.Parse(sql)
	if err != nil || len(tree.GetStmts()) != 1 {
		return nil
	}
	return tree.GetStmts()[0].GetStmt().GetSelectStmt()
}

func int64Of(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int32:
		return int64(t)
	case float64:
		return int64(t)
	default:
		return 0
	}
}
