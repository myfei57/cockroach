// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package optbuilder

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

// analyzeQualify analyzes the QUALIFY clause and returns it as a typed
// boolean expression. fromScope contains the name bindings visible from the
// FROM/GROUP BY clauses, and projectionsScope contains the SELECT output
// columns (including their aliases).
//
// Like ORDER BY, QUALIFY can reference the unique output aliases of the
// SELECT list. Unlike ORDER BY, a name that matches both an input column and
// a SELECT output alias (or that matches multiple output aliases) is an
// ambiguity error.
//
// Window and aggregate functions are allowed in QUALIFY: type checking the
// expression against fromScope registers them so that they are built as part
// of this SELECT, in the same way functions in the SELECT list are.
func (b *Builder) analyzeQualify(
	qualify *tree.Where, fromScope, projectionsScope *scope,
) tree.TypedExpr {
	if qualify == nil {
		return nil
	}

	// We need to save and restore the previous value of the field in semaCtx
	// in case we are recursively called within a subquery context.
	defer b.semaCtx.Properties.Restore(b.semaCtx.Properties)
	b.semaCtx.Properties.Require(
		exprKindQualify.String(),
		tree.RejectGenerators|tree.RejectProcedures,
	)
	fromScope.context = exprKindQualify

	expr, _ := tree.WalkExpr(
		&qualifyAliasRewriter{b: b, fromScope: fromScope, projectionsScope: projectionsScope},
		qualify.Expr,
	)
	return fromScope.resolveAndRequireType(expr, types.Bool)
}

// buildQualify builds a set of memo groups that represent the given QUALIFY
// clause on top of outScope. It must be called after aggregate and window
// functions have been built (so that aggregate and window result columns are
// available in outScope.expr), but before the final SELECT projection and
// DISTINCT are constructed (so that filtered rows never reach them and
// hidden filter columns do not leak into the output).
//
// The return value corresponds to the top-level memo group ID for this
// QUALIFY clause.
func (b *Builder) buildQualify(qualify tree.TypedExpr, outScope, fromScope *scope) {
	if qualify == nil {
		return
	}

	filter := b.buildScalar(qualify, fromScope, nil /* outScope */, nil /* outCol */, nil /* colRefs */)

	// A FiltersItem keeps a row only when the filter evaluates to true; rows
	// for which it evaluates to false or NULL are discarded.
	outScope.expr = b.factory.ConstructSelect(
		outScope.expr,
		memo.FiltersExpr{b.factory.ConstructFiltersItem(filter)},
	)
}

// qualifyAliasRewriter rewrites bare column references in QUALIFY that
// uniquely match a SELECT output alias into the underlying projection
// expression. See analyzeQualify for the ambiguity rules.
type qualifyAliasRewriter struct {
	b                *Builder
	fromScope        *scope
	projectionsScope *scope
}

var _ tree.Visitor = &qualifyAliasRewriter{}

// VisitPre implements the tree.Visitor interface.
func (r *qualifyAliasRewriter) VisitPre(expr tree.Expr) (recurse bool, newExpr tree.Expr) {
	name, ok := expr.(*tree.UnresolvedName)
	if !ok || name.Star || name.NumParts != 1 {
		return true, expr
	}
	target := tree.Name(name.Parts[0])

	inputMatch := false
	for i := range r.fromScope.cols {
		if r.fromScope.cols[i].name.MatchesReferenceName(target) {
			inputMatch = true
			break
		}
	}

	matchIdx := -1
	for i := range r.projectionsScope.cols {
		col := &r.projectionsScope.cols[i]
		if !col.name.MatchesReferenceName(target) {
			continue
		}
		if matchIdx != -1 {
			panic(pgerror.Newf(pgcode.AmbiguousAlias,
				"QUALIFY %q is ambiguous", target))
		}
		matchIdx = i
	}

	if matchIdx == -1 {
		// Not a SELECT output alias; leave the name untouched so that it
		// resolves as a regular input column.
		return true, expr
	}
	if inputMatch {
		panic(errors.WithDetail(
			pgerror.Newf(pgcode.AmbiguousColumn,
				"column reference %q is ambiguous", target),
			"It could refer to either a SELECT output alias or an input column.",
		))
	}

	// Replace the alias with the projection expression. There is no need to
	// recurse into the substituted expression: it has already been analyzed
	// against fromScope when the SELECT list was analyzed.
	return false, r.projectionsScope.cols[matchIdx].getExpr()
}

// VisitPost implements the tree.Visitor interface.
func (r *qualifyAliasRewriter) VisitPost(expr tree.Expr) tree.Expr { return expr }
