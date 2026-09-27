package index

import (
	"fmt"
	"strings"
)

// Text renders the result as the report a human or a model reads. It leads with
// the statements to run, because a recommendation nobody applies is worth
// nothing, and it states the estimate's own limits.
func (r *Result) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Index analysis of %d quer", r.AnalysedQueries)
	if r.AnalysedQueries == 1 {
		b.WriteString("y")
	} else {
		b.WriteString("ies")
	}
	fmt.Fprintf(&b, " in %s\n\n", r.Elapsed)

	if len(r.Recommendations) == 0 {
		b.WriteString("No index would help these queries by the required margin. " +
			"The planner's cost is already as low as these statistics allow.\n")
		r.appendSkipped(&b)
		return b.String()
	}

	fmt.Fprintf(&b, "Planner cost %.2f -> %.2f", r.BaselineCost, r.FinalCost)
	if r.ImprovementMultiple > 0 {
		fmt.Fprintf(&b, " (%.1fx)", r.ImprovementMultiple)
	}
	fmt.Fprintf(&b, ", for %s of indexes on %s of table data.\n\n",
		humanBytes(r.IndexSizeBytes), humanBytes(r.BaseRelationBytes))

	for i, rec := range r.Recommendations {
		fmt.Fprintf(&b, "%d. %s\n", i+1, rec.Definition)
		fmt.Fprintf(&b, "   estimated size:    %s\n", rec.EstimatedSize)
		fmt.Fprintf(&b, "   faster on its own: %.1fx\n", rec.IndividualImprovement)
		if i > 0 {
			// The marginal gain is the useful number once more than one index is
			// recommended: it says whether this one is still worth its space.
			fmt.Fprintf(&b, "   faster so far:     %.1fx\n", rec.ProgressiveImprovement)
		}
		if rec.Note != "" {
			fmt.Fprintf(&b, "   note:              %s\n", rec.Note)
		}
		b.WriteString("\n")
	}

	b.WriteString("These are estimates from the planner's cost model, not measurements. " +
		"Check them with EXPLAIN after creating the first index, and keep only what " +
		"the plan actually uses.\n")
	r.appendSkipped(&b)
	return b.String()
}

func (r *Result) appendSkipped(b *strings.Builder) {
	if len(r.SkippedQueries) == 0 {
		return
	}
	fmt.Fprintf(b, "\nNot analysed (%d):\n", len(r.SkippedQueries))
	for _, reason := range r.SkippedQueries {
		fmt.Fprintf(b, "  - %s\n", reason)
	}
}
