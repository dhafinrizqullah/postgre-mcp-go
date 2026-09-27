package index

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/dustin/go-humanize"
)

// replaceParameters substitutes bind parameters with realistic constants.
//
// A query from pg_stat_statements is normalised: `WHERE user_id = $1`. Asking the
// planner about that gives it a generic estimate, so every table looks like it
// holds the same number of distinct values and no index ever looks worth having.
// Sampling a real value from the column's statistics restores the planner's
// ability to tell an index scan from a sequential scan.
//
// The literal is produced by Postgres itself via quote_literal, so a value
// containing a quote cannot break out of the statement. When a parameter cannot
// be tied to a column, the query is reported as unanalysable rather than measured
// against a made-up value.
func (t *Tuner) replaceParameters(ctx context.Context, sql string) (string, error) {
	sel := parseSelect(sql)
	if sel == nil {
		return "", fmt.Errorf("not a SELECT")
	}
	sites, unresolvable := ExtractParams(sel)
	if len(sites) == 0 {
		if unresolvable > 0 {
			return "", fmt.Errorf("%d parameter(s) are not compared to a column, so their values cannot be sampled", unresolvable)
		}
		return sql, nil
	}

	// Highest parameter number first, so $10 is not clipped by replacing $1.
	replacements := make([]paramReplacement, 0, len(sites))
	for _, site := range sites {
		if site.table == "" || site.column == "" {
			return "", fmt.Errorf("parameter $%d is not compared to a column, so its value cannot be sampled", site.number)
		}
		literal, err := t.sampleLiteral(ctx, site.table, site.column)
		if err != nil {
			return "", err
		}
		if literal == "" {
			return "", fmt.Errorf("no statistics for %s.%s, so parameter $%d cannot be sampled",
				site.table, site.column, site.number)
		}
		replacements = append(replacements, paramReplacement{spelling: "$" + strconv.Itoa(site.number), literal: literal})
	}
	return spliceLiterals(sql, replacements), nil
}

// dollarTag returns the opening delimiter of a dollar-quoted string at the start
// of s, or ok=false when s does not begin one. A bare `$$` counts, and a `$1` is
// a parameter rather than a delimiter because a tag cannot start with a digit.
func dollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	if s[1] == '$' {
		return "$$", true
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '$':
			return s[:i+1], true
		case c >= '0' && c <= '9':
			return "", false // $1 is a parameter
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			continue
		default:
			return "", false
		}
	}
	return "", false
}

type paramReplacement struct {
	spelling string
	literal  string
}

// spliceLiterals rewrites a statement by walking it and replacing whole
// parameters. Scanning by hand is required: a textual replace would also hit `$1`
// inside a string literal or a comment.
func spliceLiterals(sql string, replacements []paramReplacement) string {
	// Longest spelling first, so $10 wins over $1.
	ordered := append([]paramReplacement(nil), replacements...)
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if len(ordered[j].spelling) > len(ordered[i].spelling) {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}

	var out strings.Builder
	out.Grow(len(sql))
	for i := 0; i < len(sql); {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				out.WriteString(sql[i:])
				i = len(sql)
				continue
			}
			out.WriteString(sql[i : i+end+1])
			i += end + 1
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				out.WriteString(sql[i:])
				i = len(sql)
				continue
			}
			out.WriteString(sql[i : i+2+end+2])
			i += 2 + end + 2
		case sql[i] == '\'':
			// Copy a string literal, doubling any embedded quote, so a $1 inside
			// it is left alone.
			j := i + 1
			for j < len(sql) {
				if sql[j] == '\'' {
					if j+1 < len(sql) && sql[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			out.WriteString(sql[i:j])
			i = j
		case sql[i] == '$':
			// A dollar-quoted string ($tag$ ... $tag$) may hold anything at all,
			// including something that looks like a parameter. Copy it whole.
			if tag, ok := dollarTag(sql[i:]); ok {
				end := strings.Index(sql[i+len(tag):], tag)
				if end < 0 {
					out.WriteString(sql[i:])
					i = len(sql)
					continue
				}
				stop := i + len(tag) + end + len(tag)
				out.WriteString(sql[i:stop])
				i = stop
				continue
			}
			matched := false
			for _, replacement := range ordered {
				if strings.HasPrefix(sql[i:], replacement.spelling) {
					out.WriteString(replacement.literal)
					i += len(replacement.spelling)
					matched = true
					break
				}
			}
			if !matched {
				out.WriteByte(sql[i])
				i++
			}
		default:
			out.WriteByte(sql[i])
			i++
		}
	}
	return out.String()
}

// sampleLiteral returns a representative literal for a column, taken from the most
// common value the planner already knows about.
//
// The value comes back as text plus the column's base type, because
// most_common_vals is an anyarray that cannot be cast to text[] and its elements
// have to be rendered as literals of the right kind. Numeric and boolean types
// are emitted bare, everything else is quoted with the quote doubled, so a value
// containing a quote cannot break out of the statement.
func (t *Tuner) sampleLiteral(ctx context.Context, table, column string) (string, error) {
	row, err := t.db.QueryOne(ctx, `
		SELECT
			ty.typname AS type_name,
			array_to_json(s.most_common_vals) ->> 0 AS value
		FROM pg_stats s
		JOIN pg_class c ON c.relname = s.tablename
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = s.schemaname
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = s.attname
		JOIN pg_type ty ON ty.oid = a.atttypid
		WHERE s.tablename = $1
		  AND s.attname = $2
		  AND s.most_common_vals IS NOT NULL
		  AND array_length(s.most_common_vals, 1) > 0
		LIMIT 1`, table, column)
	if err != nil {
		return "", fmt.Errorf("sampling %s.%s: %w", table, column, err)
	}
	if row == nil {
		return "", nil
	}
	value, ok := row["value"].(string)
	if !ok || value == "" {
		// The first most common value is NULL, which tells the planner nothing
		// useful about selectivity.
		return "", nil
	}
	return literalFor(rowType(row), value), nil
}

func rowType(row map[string]any) string { return str(row, "type_name") }

// numericTypes are emitted bare; anything else is a quoted string.
var numericTypes = map[string]bool{
	"int2": true, "int4": true, "int8": true,
	"float4": true, "float8": true,
	"numeric": true, "money": true, "oid": true,
}

// literalFor renders a sampled value as a SQL literal of its column's type.
func literalFor(typeName, value string) string {
	if numericTypes[typeName] {
		return value
	}
	switch typeName {
	case "bool":
		if value == "t" || value == "true" {
			return "TRUE"
		}
		if value == "f" || value == "false" {
			return "FALSE"
		}
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// paramSite is a bind parameter found next to a column reference.
type paramSite struct {
	number int
	table  string
	column string
}

// ExtractParams is the parameter counterpart of ExtractColumns: it reports every
// bind parameter together with the column it is compared to, resolved in the
// scope where the comparison was written. The column extractor's traversal does
// the walking, so a parameter inside a subquery is attributed to the subquery's
// table rather than the outer one.
//
// The second result is how many parameters had no column to point at, which the
// caller needs in order to explain why a query was skipped.
func ExtractParams(stmt *pg_query.SelectStmt) ([]paramSite, int) {
	collector := &paramCollector{seen: map[int]bool{}}
	scope := &queryScope{
		columns: &Columns{uses: map[string]ColumnUse{}},
		ctes:    map[string]bool{},
		derived: map[string]bool{},
	}
	// The scope is passed in, so a comparison inside a subquery resolves against
	// that subquery's aliases rather than the outer ones.
	scope.onCompare = func(scope *queryScope, left, right *pg_query.Node) {
		collector.compare(scope, left, right)
	}
	scope.walkSelect(stmt)
	return collector.sites, collector.total - len(collector.sites)
}

type paramCollector struct {
	sites []paramSite
	seen  map[int]bool
	// total counts every parameter found, including ones no column was found for,
	// so the caller can explain the difference.
	total int
}

func (p *paramCollector) add(site paramSite) {
	if site.number == 0 || p.seen[site.number] {
		return
	}
	p.seen[site.number] = true
	p.sites = append(p.sites, site)
}

// compare records a comparison between a column and a parameter, in either order,
// and returns the number of parameters it saw.
func (p *paramCollector) compare(scope *queryScope, left, right *pg_query.Node) int {
	count := 0
	if _, ok := numberOf(left); ok {
		count++
	}
	if _, ok := numberOf(right); ok {
		count++
	}
	if count != 1 {
		// Two parameters compared to each other have no column to sample from.
		return count
	}
	if number, ok := numberOf(left); ok {
		if column, ok := columnOf(scope, right); ok {
			p.add(paramSite{number: number, table: column.table, column: column.column})
		}
	}
	if number, ok := numberOf(right); ok {
		if column, ok := columnOf(scope, left); ok {
			p.add(paramSite{number: number, table: column.table, column: column.column})
		}
	}
	return count
}

// numberOf reports the 1-based number of a bind parameter node.
func numberOf(node *pg_query.Node) (int, bool) {
	if ref, ok := unwrap(node).(*pg_query.ParamRef); ok {
		return int(ref.GetNumber()), true
	}
	return 0, false
}

// columnOf resolves a column reference to the table it belongs to, using the
// scope's aliases. A parameter compared to an expression rather than a column has
// no answer, which is why this returns ok=false.
func columnOf(scope *queryScope, node *pg_query.Node) (qualifiedColumn, bool) {
	ref, ok := unwrap(node).(*pg_query.ColumnRef)
	if !ok {
		return qualifiedColumn{}, false
	}
	fields := fieldNames(ref)
	switch len(fields) {
	case 1:
		if fields[0] == "*" || len(scope.tables) != 1 {
			return qualifiedColumn{}, false
		}
		for table := range scope.tables {
			return qualifiedColumn{table: table, column: fields[0]}, true
		}
	case 2:
		if fields[1] == "*" {
			return qualifiedColumn{}, false
		}
		return qualifiedColumn{table: scope.resolve(fields[0]), column: fields[1]}, true
	default:
		if fields[len(fields)-1] == "*" {
			return qualifiedColumn{}, false
		}
		return qualifiedColumn{
			table:  fields[len(fields)-2],
			column: fields[len(fields)-1],
		}, true
	}
	return qualifiedColumn{}, false
}

// qualifiedColumn is a column reference resolved to a table.
type qualifiedColumn struct {
	table  string
	column string
}

// jsonBytes and jsonUnmarshal keep the plan decoding in one place.
func jsonBytes(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// humanBytes formats a size for the report.
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	return humanize.IBytes(uint64(n))
}
