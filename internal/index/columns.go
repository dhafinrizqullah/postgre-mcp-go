// Package index recommends indexes, using Postgres's own cost model via the
// hypopg extension. It is the Go port of crystaldba/postgres-mcp's
// index/ package, which adapts Microsoft's Anytime algorithm.
//
// The search is greedy: find the single index that helps most, then the best
// index to add to that, and stop when the time budget runs out or a round brings
// less than the minimum improvement. Every candidate is judged by the planner
// itself, so an estimate is never the server's guess about a guess.
package index

import (
	"fmt"
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// ColumnUse is one column, and why it is worth an index.
type ColumnUse struct {
	Table  string
	Column string
	// Filtered means the column appears in WHERE, JOIN, or HAVING, which is
	// where an index pays off. A column that is only projected is not worth one.
	Filtered bool
}

// Columns is the set of column uses found in one statement, keyed by
// table.column. A statement is analysed, not executed, so this is the only place
// its columns are discovered.
type Columns struct {
	uses map[string]ColumnUse
	// order preserves first-seen order so output is stable and reviewable.
	order []string
}

// Add records a use of a column. The first use wins for Filtered, because a
// column that is both filtered and projected is a filtering column.
func (c *Columns) Add(table, column string, filtered bool) {
	if table == "" || column == "" {
		return
	}
	key := table + "." + column
	if existing, ok := c.uses[key]; ok {
		existing.Filtered = existing.Filtered || filtered
		c.uses[key] = existing
		return
	}
	c.uses[key] = ColumnUse{Table: table, Column: column, Filtered: filtered}
	c.order = append(c.order, key)
}

// Len reports how many distinct columns were found.
func (c *Columns) Len() int { return len(c.order) }

// All returns the column uses in first-seen order.
func (c *Columns) All() []ColumnUse {
	out := make([]ColumnUse, 0, len(c.order))
	for _, key := range c.order {
		out = append(out, c.uses[key])
	}
	return out
}

// Filtered returns only the uses that justify an index.
func (c *Columns) Filtered() []ColumnUse {
	out := make([]ColumnUse, 0, len(c.order))
	for _, key := range c.order {
		if u := c.uses[key]; u.Filtered {
			out = append(out, u)
		}
	}
	return out
}

// Tables returns the distinct table names referenced, sorted.
func (c *Columns) Tables() []string {
	seen := map[string]bool{}
	for _, u := range c.uses {
		seen[u.Table] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ColumnResolver answers "which of these tables has this column". It is an
// interface only because the caller has a database and the extractor should not
// need one; a nil resolver is valid and falls back to attributing an ambiguous
// column to every table in scope.
type ColumnResolver interface {
	// Owner returns the table that owns column among candidates, or "" if it
	// cannot be determined.
	Owner(table, column string, candidates []string) string
}

// ExtractColumns finds the columns a SELECT reads, resolves aliases, and records
// which of them are filtered rather than merely projected.
//
// stmt must be a SelectStmt. The walk is scope-aware: a subquery in FROM gets its
// own alias scope, so `o.user_id` inside it is never attributed to the outer
// query's `orders`.
func ExtractColumns(stmt *pg_query.SelectStmt, resolver ColumnResolver) *Columns {
	cols := &Columns{uses: map[string]ColumnUse{}}
	scope := &queryScope{columns: cols, resolver: resolver}
	scope.walkSelect(stmt)
	return cols
}

// queryScope is the alias environment of one SELECT level.
type queryScope struct {
	columns  *Columns
	resolver ColumnResolver
	// tables are the base relations in scope, aliases maps an alias to its table.
	tables  map[string]bool
	aliases map[string]string
	// targetAliases maps a column alias in the target list to the expression it
	// stands for, so `ORDER BY total` indexes the real column.
	targetAliases map[string]*pg_query.Node
	// ctes holds CTE names so a reference to one is not mistaken for a table.
	ctes map[string]bool
	// derived holds aliases of subqueries in FROM. They have no index of their
	// own, so a column qualified by one is dropped rather than guessed at.
	// ponytail: a filter on a subquery's output column is missed. Resolving it
	// means projecting through the subquery's target list, which is worth doing
	// if this shows up in real workloads.
	derived map[string]bool
	// onCompare, when set, is called for every comparison in this scope, with the
	// scope itself so a nested scope resolves its own columns. The parameter
	// collector uses it; column extraction leaves it nil.
	onCompare func(scope *queryScope, left, right *pg_query.Node)
}

func (s *queryScope) walkSelect(stmt *pg_query.SelectStmt) {
	if stmt == nil {
		return
	}
	// UNION arms: each is a complete query with its own scope.
	if stmt.GetLarg() != nil {
		child := s.child()
		child.walkSelect(stmt.GetLarg())
	}
	if stmt.GetRarg() != nil {
		child := s.child()
		child.walkSelect(stmt.GetRarg())
	}
	if len(stmt.GetWithClause().GetCtes()) > 0 {
		s.ctes = map[string]bool{}
		for _, cte := range stmt.GetWithClause().GetCtes() {
			c := cte.GetCommonTableExpr()
			if c == nil {
				continue
			}
			s.ctes[c.GetCtename()] = true
			// A CTE's own body sees the CTEs before it, not itself.
			child := s.child()
			child.ctes = s.ctes
			if sel := selectOf(c.GetCtequery()); sel != nil {
				child.walkSelect(sel)
			}
		}
	}

	// Build this level's alias environment from the FROM clause.
	s.tables = map[string]bool{}
	s.aliases = map[string]string{}
	s.derived = map[string]bool{}
	for _, from := range stmt.GetFromClause() {
		s.addFrom(from)
	}

	s.targetAliases = map[string]*pg_query.Node{}
	for _, target := range stmt.GetTargetList() {
		if t := target.GetResTarget(); t != nil && t.GetName() != "" {
			s.targetAliases[t.GetName()] = t.GetVal()
		}
	}

	for _, target := range stmt.GetTargetList() {
		if t := target.GetResTarget(); t != nil {
			s.expr(t.GetVal(), false)
		}
	}
	// GROUP BY either names a column, a target-list expression, or a position.
	// `GROUP BY 1` parses as an integer constant, not as a reference to the first
	// target, so the position has to be resolved here.
	for _, group := range stmt.GetGroupClause() {
		if position := targetPosition(group); position > 0 && position <= len(stmt.GetTargetList()) {
			s.expr(stmt.GetTargetList()[position-1].GetResTarget().GetVal(), true)
			continue
		}
		s.expr(group, true)
	}
	s.expr(stmt.GetWhereClause(), true)
	s.expr(stmt.GetHavingClause(), true)
	// A window's PARTITION BY and ORDER BY are index candidates in their own
	// right, and every window in the clause is walked once here.
	for _, window := range stmt.GetWindowClause() {
		s.windowPartition(window.GetWindowDef())
	}
	for _, sort := range stmt.GetSortClause() {
		sb := sort.GetSortBy()
		if sb == nil {
			continue
		}
		// `ORDER BY total` may name a target-list alias; follow it to the real
		// column so the index lands on the expression's column.
		if ref := s.aliasTarget(sb.GetNode()); ref != nil {
			s.expr(ref, true)
			continue
		}
		s.expr(sb.GetNode(), true)
	}
	// A FROM subquery's body is a query in its own right, already walked above
	// for CTEs; walk plain subselects here.
	for _, from := range stmt.GetFromClause() {
		if sub := from.GetRangeSubselect(); sub != nil {
			if sel := selectOf(sub.GetSubquery()); sel != nil {
				s.child().walkSelect(sel)
			}
		}
	}
}

// child returns a scope for a nested query, inheriting the CTE names.
func (s *queryScope) child() *queryScope {
	child := &queryScope{
		columns:   s.columns,
		resolver:  s.resolver,
		ctes:      s.ctes,
		derived:   map[string]bool{},
		onCompare: s.onCompare,
	}
	return child
}

func (s *queryScope) addFrom(node *pg_query.Node) {
	switch n := unwrap(node).(type) {
	case *pg_query.RangeVar:
		s.tables[n.GetRelname()] = true
		if alias := n.GetAlias().GetAliasname(); alias != "" {
			s.aliases[alias] = n.GetRelname()
		}
	case *pg_query.JoinExpr:
		s.addFrom(n.GetLarg())
		s.addFrom(n.GetRarg())
		// The join condition filters rows, so its columns are index candidates.
		s.expr(n.GetQuals(), true)
		// USING (col) is a filter too, on both sides of the join.
		for _, using := range n.GetUsingClause() {
			if str := using.GetString_(); str != nil {
				s.addAllTables(str.GetSval(), true)
			}
		}
	case *pg_query.RangeSubselect:
		if alias := n.GetAlias().GetAliasname(); alias != "" {
			s.tables[alias] = true
			s.derived[alias] = true
		}
	}
}

// aliasTarget resolves a node that names a target-list alias to the expression
// that alias stands for.
func (s *queryScope) aliasTarget(node *pg_query.Node) *pg_query.Node {
	ref, ok := unwrap(node).(*pg_query.ColumnRef)
	if !ok {
		return nil
	}
	if name, ok := singleFieldName(ref); ok {
		return s.targetAliases[name]
	}
	return nil
}

func (s *queryScope) expr(node *pg_query.Node, filtered bool) {
	if node == nil {
		return
	}
	switch n := unwrap(node).(type) {
	case *pg_query.ColumnRef:
		s.columnRef(n, filtered)
	case *pg_query.A_Expr:
		// A hook rather than more special cases, so the parameter collector
		// sees comparisons in the scope they were written in.
		if s.onCompare != nil {
			s.onCompare(s, n.GetLexpr(), n.GetRexpr())
		}
		s.expr(n.GetLexpr(), filtered)
		s.expr(n.GetRexpr(), filtered)
	case *pg_query.BoolExpr:
		for _, arg := range n.GetArgs() {
			s.expr(arg, filtered)
		}
	case *pg_query.FuncCall:
		for _, arg := range n.GetArgs() {
			s.expr(arg, filtered)
		}
		// count(*) has a target list too.
		for _, agg := range n.GetAggOrder() {
			s.expr(agg, filtered)
		}
		// An aggregate's FILTER clause restricts rows, so it filters.
		if filter := n.GetAggFilter(); filter != nil {
			s.expr(filter, true)
		}
		// In the raw parse tree a windowed call carries its window definition
		// inline. Only the analyzed WindowFunc form uses a numeric winref, and
		// that form never reaches here.
		s.windowPartition(n.GetOver())
	case *pg_query.NullTest:
		s.expr(n.GetArg(), filtered)
	case *pg_query.BooleanTest:
		s.expr(n.GetArg(), filtered)
	case *pg_query.CaseExpr:
		s.expr(n.GetArg(), filtered)
		for _, when := range n.GetArgs() {
			s.expr(when, filtered)
		}
		s.expr(n.GetDefresult(), filtered)
	case *pg_query.CaseWhen:
		s.expr(n.GetExpr(), filtered)
		s.expr(n.GetResult(), filtered)
	case *pg_query.CoalesceExpr:
		for _, arg := range n.GetArgs() {
			s.expr(arg, filtered)
		}
	case *pg_query.SubLink:
		// A subquery in an expression is a query in its own scope.
		if sel := selectOf(n.GetSubselect()); sel != nil {
			s.child().walkSelect(sel)
		}
		s.expr(n.GetTestexpr(), filtered)
	case *pg_query.WindowFunc:
		for _, arg := range n.GetArgs() {
			s.expr(arg, filtered)
		}
		// The window's partition and order keys are reached through the
		// enclosing SELECT's windowClause list, not from here: in the raw parse
		// tree a WindowFunc only carries a numeric winref.
		if filter := n.GetAggfilter(); filter != nil {
			s.expr(filter, true)
		}
	case *pg_query.A_ArrayExpr:
		for _, elem := range n.GetElements() {
			s.expr(elem, filtered)
		}
	case *pg_query.RowExpr:
		for _, elem := range n.GetArgs() {
			s.expr(elem, filtered)
		}
	case *pg_query.GroupingFunc:
		for _, arg := range n.GetArgs() {
			s.expr(arg, filtered)
		}
	case *pg_query.MinMaxExpr:
		for _, arg := range n.GetArgs() {
			s.expr(arg, filtered)
		}
	case *pg_query.NamedArgExpr:
		s.expr(n.GetArg(), filtered)
	case *pg_query.SelectStmt:
		s.child().walkSelect(n)
	}
}

// windowPartition walks a window definition, whose partition and order keys are
// the same kind of index candidates as a WHERE clause.
func (s *queryScope) windowPartition(over *pg_query.WindowDef) {
	if over == nil {
		return
	}
	for _, part := range over.GetPartitionClause() {
		s.expr(part, true)
	}
	for _, order := range over.GetOrderClause() {
		if sb := order.GetSortBy(); sb != nil {
			s.expr(sb.GetNode(), true)
		}
	}
}

func (s *queryScope) columnRef(ref *pg_query.ColumnRef, filtered bool) {
	fields := fieldNames(ref)
	switch len(fields) {
	case 0:
		return
	case 1:
		// `SELECT *` and a reference to a target-list alias carry no column.
		if fields[0] == "*" || s.targetAliases[fields[0]] != nil {
			return
		}
		if len(s.tables) == 1 {
			s.addAllTables(fields[0], filtered)
			return
		}
		if owner := s.ownerOf(fields[0]); owner != "" {
			s.columns.Add(owner, fields[0], filtered)
			return
		}
		s.addAllTables(fields[0], filtered)
	case 2:
		if fields[1] == "*" {
			return
		}
		table := s.resolve(fields[0])
		// A derived table or a CTE has no index of its own.
		if s.derived[table] || s.ctes[table] {
			return
		}
		s.columns.Add(table, fields[1], filtered)
	default:
		// schema.table.column: the middle field is the table.
		if fields[len(fields)-1] == "*" {
			return
		}
		s.columns.Add(fields[len(fields)-2], fields[len(fields)-1], filtered)
	}
}

// resolve maps a table name or alias to the underlying relation. A CTE name or
// an unknown identifier is returned unchanged: a CTE has no index of its own, and
// dropping the reference would lose a real table.
func (s *queryScope) resolve(name string) string {
	if s.ctes[name] {
		return name
	}
	if table, ok := s.aliases[name]; ok {
		return table
	}
	return name
}

// ownerOf decides which table in scope an unqualified column belongs to.
//
// Upstream attributes it to whichever table it happens to visit first, which
// produces recommendations for the wrong table. A catalog lookup is cheap and
// correct; without a resolver the column is credited to every table in scope, so
// the candidate set is a superset rather than a guess.
func (s *queryScope) ownerOf(column string) string {
	if s.resolver == nil {
		return ""
	}
	candidates := make([]string, 0, len(s.tables))
	for table := range s.tables {
		candidates = append(candidates, table)
	}
	sort.Strings(candidates)
	if owner := s.resolver.Owner("", column, candidates); owner != "" {
		return owner
	}
	return ""
}

func (s *queryScope) addAllTables(column string, filtered bool) {
	for table := range s.tables {
		if !s.derived[table] {
			s.columns.Add(table, column, filtered)
		}
	}
}

// targetPosition reports the 1-based target-list position a clause refers to, or
// 0 if it is not a positional reference. `GROUP BY 1` arrives as an integer
// constant; an explicit `SortGroupClause` reference carries the same number.
func targetPosition(node *pg_query.Node) int {
	switch n := unwrap(node).(type) {
	case *pg_query.A_Const:
		if n.GetIval() != nil {
			return int(n.GetIval().GetIval())
		}
	case *pg_query.SortGroupClause:
		return int(n.GetTleSortGroupRef())
	}
	return 0
}

// fieldNames flattens ColumnRef.fields into plain names, using "*" for a star.
func fieldNames(ref *pg_query.ColumnRef) []string {
	out := make([]string, 0, len(ref.GetFields()))
	for _, f := range ref.GetFields() {
		if str := f.GetString_(); str != nil {
			out = append(out, str.GetSval())
			continue
		}
		out = append(out, "*")
	}
	return out
}

// singleFieldName reports the name of an unqualified column reference.
func singleFieldName(ref *pg_query.ColumnRef) (string, bool) {
	fields := fieldNames(ref)
	if len(fields) != 1 || fields[0] == "*" {
		return "", false
	}
	return fields[0], true
}

// unwrap returns the concrete message behind the protobuf oneof holder.
func unwrap(node *pg_query.Node) any {
	if node == nil {
		return nil
	}
	if m := node.ProtoReflect(); m.Descriptor().FullName() == "pg_query.Node" {
		fields := m.Descriptor().Fields()
		for i := range fields.Len() {
			if fd := fields.Get(i); m.Has(fd) && fd.Message() != nil {
				return m.Get(fd).Message().Interface()
			}
		}
	}
	return node.ProtoReflect().Interface()
}

// selectOf returns the SELECT behind a node, whether it is a SelectStmt directly
// or wrapped in a set-operation node.
func selectOf(node *pg_query.Node) *pg_query.SelectStmt {
	if node == nil {
		return nil
	}
	if sel, ok := unwrap(node).(*pg_query.SelectStmt); ok {
		return sel
	}
	return nil
}

// TablesIn returns the base relations a statement reads, which is how the
// workload pre-check skips system catalogs.
func TablesIn(stmt *pg_query.SelectStmt) []string {
	seen := map[string]bool{}
	var walk func(*pg_query.SelectStmt)
	walk = func(s *pg_query.SelectStmt) {
		if s == nil {
			return
		}
		for _, from := range s.GetFromClause() {
			collectTables(from, seen)
		}
		for _, cte := range s.GetWithClause().GetCtes() {
			if c := cte.GetCommonTableExpr(); c != nil {
				if sel := selectOf(c.GetCtequery()); sel != nil {
					walk(sel)
				}
			}
		}
	}
	walk(stmt)
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func collectTables(node *pg_query.Node, into map[string]bool) {
	switch n := unwrap(node).(type) {
	case *pg_query.RangeVar:
		into[n.GetRelname()] = true
	case *pg_query.JoinExpr:
		collectTables(n.GetLarg(), into)
		collectTables(n.GetRarg(), into)
	case *pg_query.RangeSubselect:
		if sel := selectOf(n.GetSubquery()); sel != nil {
			// A subquery is a table only by alias; its inner tables are counted
			// by the outer walk instead.
			_ = sel
		}
	}
}

// isSystemTable reports whether every table is a catalog table, which means there
// is nothing to index.
func isSystemTable(name string) bool {
	return strings.HasPrefix(name, "pg_") ||
		strings.HasPrefix(name, "aurora_") ||
		strings.HasPrefix(name, "information_schema")
}

// formatDefinition renders an index the way CREATE INDEX would, which is what
// the recommendation report shows.
func formatDefinition(table string, columns []string, using string) string {
	method := using
	if method == "" {
		method = "btree"
	}
	return fmt.Sprintf("CREATE INDEX ON %s USING %s (%s)", table, method, strings.Join(columns, ", "))
}
