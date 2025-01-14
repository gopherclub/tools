// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package loopclosure defines an Analyzer that checks for references to
// enclosing loop variables from within nested functions.
package loopclosure

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
	"golang.org/x/tools/internal/analysisinternal"
)

const Doc = `check references to loop variables from within nested functions

This analyzer checks for references to loop variables from within a function
literal inside the loop body. It checks for patterns where access to a loop
variable is known to escape the current loop iteration:
 1. a call to go or defer at the end of the loop body
 2. a call to golang.org/x/sync/errgroup.Group.Go at the end of the loop body

The analyzer only considers references in the last statement of the loop body
as it is not deep enough to understand the effects of subsequent statements
which might render the reference benign.

For example:

	for i, v := range s {
		go func() {
			println(i, v) // not what you might expect
		}()
	}

See: https://golang.org/doc/go_faq.html#closures_and_goroutines`

// TODO(rfindley): enable support for checking parallel subtests, pending
// investigation, adding:
// 3. a call testing.T.Run where the subtest body invokes t.Parallel()

var Analyzer = &analysis.Analyzer{
	Name:     "loopclosure",
	Doc:      Doc,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (interface{}, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	nodeFilter := []ast.Node{
		(*ast.RangeStmt)(nil),
		(*ast.ForStmt)(nil),
	}
	inspect.Preorder(nodeFilter, func(n ast.Node) {
		// Find the variables updated by the loop statement.
		var vars []*ast.Ident
		addVar := func(expr ast.Expr) {
			if id, ok := expr.(*ast.Ident); ok {
				vars = append(vars, id)
			}
		}
		var body *ast.BlockStmt
		switch n := n.(type) {
		case *ast.RangeStmt:
			body = n.Body
			addVar(n.Key)
			addVar(n.Value)
		case *ast.ForStmt:
			body = n.Body
			switch post := n.Post.(type) {
			case *ast.AssignStmt:
				// e.g. for p = head; p != nil; p = p.next
				for _, lhs := range post.Lhs {
					addVar(lhs)
				}
			case *ast.IncDecStmt:
				// e.g. for i := 0; i < n; i++
				addVar(post.X)
			}
		}
		if vars == nil {
			return
		}

		// Inspect statements to find function literals that may be run outside of
		// the current loop iteration.
		//
		// For go, defer, and errgroup.Group.Go, we ignore all but the last
		// statement, because it's hard to prove go isn't followed by wait, or
		// defer by return.
		//
		// We consider every t.Run statement in the loop body, because there is
		// no such commonly used mechanism for synchronizing parallel subtests.
		// It is of course theoretically possible to synchronize parallel subtests,
		// though such a pattern is likely to be exceedingly rare as it would be
		// fighting against the test runner.
		lastStmt := len(body.List) - 1
		for i, s := range body.List {
			var fun ast.Expr // if non-nil, a function that escapes the loop iteration
			switch s := s.(type) {
			case *ast.GoStmt:
				if i == lastStmt {
					fun = s.Call.Fun
				}

			case *ast.DeferStmt:
				if i == lastStmt {
					fun = s.Call.Fun
				}

			case *ast.ExprStmt: // check for errgroup.Group.Go and testing.T.Run (with T.Parallel)
				if call, ok := s.X.(*ast.CallExpr); ok {
					if i == lastStmt {
						fun = goInvoke(pass.TypesInfo, call)
					}
					if fun == nil && analysisinternal.LoopclosureParallelSubtests {
						fun = parallelSubtest(pass.TypesInfo, call)
					}
				}
			}

			lit, ok := fun.(*ast.FuncLit)
			if !ok {
				continue
			}

			ast.Inspect(lit.Body, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || id.Obj == nil {
					return true
				}
				if pass.TypesInfo.Types[id].Type == nil {
					// Not referring to a variable (e.g. struct field name)
					return true
				}
				for _, v := range vars {
					if v.Obj == id.Obj {
						pass.ReportRangef(id, "loop variable %s captured by func literal",
							id.Name)
					}
				}
				return true
			})
		}
	})
	return nil, nil
}

// goInvoke returns a function expression that would be called asynchronously
// (but not awaited) in another goroutine as a consequence of the call.
// For example, given the g.Go call below, it returns the function literal expression.
//
//	import "sync/errgroup"
//	var g errgroup.Group
//	g.Go(func() error { ... })
//
// Currently only "golang.org/x/sync/errgroup.Group()" is considered.
func goInvoke(info *types.Info, call *ast.CallExpr) ast.Expr {
	if !isMethodCall(info, call, "golang.org/x/sync/errgroup", "Group", "Go") {
		return nil
	}
	return call.Args[0]
}

// parallelSubtest returns a function expression that would be called
// asynchronously via the go test runner, as t.Run has been invoked with a
// function literal that calls t.Parallel.
//
//		import "testing"
//
//		func TestFoo(t *testing.T) {
//			tests := []int{0, 1, 2}
//			for i, t := range tests {
//			 	t.Run("subtest", func(t *testing.T) {
//	 				t.Parallel()
//			 		println(i, t)
//			 	})
//		 	}
//		}
func parallelSubtest(info *types.Info, call *ast.CallExpr) ast.Expr {
	if !isMethodCall(info, call, "testing", "T", "Run") {
		return nil
	}

	lit, ok := call.Args[1].(*ast.FuncLit)
	if !ok {
		return nil
	}

	for _, stmt := range lit.Body.List {
		exprStmt, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		if isMethodCall(info, exprStmt.X, "testing", "T", "Parallel") {
			return lit
		}
	}

	return nil
}

// isMethodCall reports whether expr is a method call of
// <pkgPath>.<typeName>.<method>.
func isMethodCall(info *types.Info, expr ast.Expr, pkgPath, typeName, method string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	// Check that we are calling a method <method>
	f := typeutil.StaticCallee(info, call)
	if f == nil || f.Name() != method {
		return false
	}
	recv := f.Type().(*types.Signature).Recv()
	if recv == nil {
		return false
	}

	// Check that the receiver is a <pkgPath>.<typeName> or
	// *<pkgPath>.<typeName>.
	rtype := recv.Type()
	if ptr, ok := recv.Type().(*types.Pointer); ok {
		rtype = ptr.Elem()
	}
	named, ok := rtype.(*types.Named)
	if !ok {
		return false
	}
	if named.Obj().Name() != typeName {
		return false
	}
	pkg := f.Pkg()
	if pkg == nil {
		return false
	}
	if pkg.Path() != pkgPath {
		return false
	}

	return true
}
