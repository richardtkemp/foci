package autoapprove

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// declBuiltins are the bash declaration builtins. Reached through `builtin`
// or `command` they parse as an ordinary CallExpr, not a DeclClause, but they
// still set and export variables in the running shell.
var declBuiltins = map[string]bool{
	"export":   true,
	"declare":  true,
	"typeset":  true,
	"local":    true,
	"readonly": true,
}

// exportedChangeBeforeCommand reports whether a statement list changes an
// EXPORTED variable before a command runs (#2326, Dick's ruling 2026-10-13).
// A changed exported variable reaches the later program's environment exactly
// as a prefix assignment would (GOFLAGS=-toolexec=…, RIPGREP_CONFIG_PATH,
// HOME pointing at a .gitconfig with core.pager), so the request must ask.
//
// A change is any of:
//   - an exporting declaration: `export` with an operand, or a
//     declare/typeset/local/readonly whose flags hold x (or cannot be read),
//     also when reached through `builtin`/`command` (declChangesEnv,
//     callDeclaresVariable);
//   - any assignment to a name the backend environment already holds: bash
//     keeps an inherited variable exported, so the next command sees the new
//     value. That covers a plain assignment, a declaration operand, a for
//     iterator, an arithmetic assignment (((HOME=5)), let, $((…))) and a
//     ${VAR:=…} / ${VAR=…} default.
//
// A command is any CallExpr that runs something (it has words). It counts as
// LATER when it ends after the change begins — so a command whose own argument
// expansion makes the change counts — or when both sit in one loop, whose body
// runs again after the change.
//
// A change with no later command keeps its verdict (`export FOO=1` alone), and
// so does a plain assignment to a name the environment does not hold
// (`X=1; ls`): bash does not export it. env is the backend environment
// snapshot the match was given; nil holds no names.
func exportedChangeBeforeCommand(stmts []*syntax.Stmt, env map[string]string) bool {
	type span struct{ start, end uint }
	var loops []span
	var changes []uint
	var commandEnds []uint
	inEnv := func(name string) bool {
		_, ok := env[name]
		return ok
	}
	for _, stmt := range stmts {
		syntax.Walk(stmt, func(node syntax.Node) bool {
			switch n := node.(type) {
			case *syntax.ForClause:
				loops = append(loops, span{n.Pos().Offset(), n.End().Offset()})
				if it, ok := n.Loop.(*syntax.WordIter); ok && it.Name != nil && inEnv(it.Name.Value) {
					changes = append(changes, n.Pos().Offset())
				}
			case *syntax.WhileClause: // while and until
				loops = append(loops, span{n.Pos().Offset(), n.End().Offset()})
			case *syntax.CallExpr:
				if len(n.Args) > 0 {
					commandEnds = append(commandEnds, n.End().Offset())
					if callDeclaresVariable(n) {
						changes = append(changes, n.Pos().Offset())
					}
					break
				}
				for _, a := range n.Assigns {
					if a.Name != nil && inEnv(a.Name.Value) {
						changes = append(changes, n.Pos().Offset())
					}
				}
			case *syntax.DeclClause:
				if declChangesEnv(n, inEnv) {
					changes = append(changes, n.Pos().Offset())
				}
			case *syntax.BinaryArithm:
				if arithAssignOp(n.Op) && arithNameChanges(n.X, inEnv) {
					changes = append(changes, n.Pos().Offset())
				}
			case *syntax.UnaryArithm:
				if (n.Op == syntax.Inc || n.Op == syntax.Dec) && arithNameChanges(n.X, inEnv) {
					changes = append(changes, n.Pos().Offset())
				}
			case *syntax.ParamExp:
				if n.Exp != nil && (n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) &&
					n.Param != nil && inEnv(n.Param.Value) {
					changes = append(changes, n.Pos().Offset())
				}
			}
			return true
		})
	}
	for _, at := range changes {
		// A change inside a loop takes effect for every command the loop
		// runs, including those written before it in the body.
		effective := at
		for _, l := range loops {
			if l.start <= at && at < l.end && l.start < effective {
				effective = l.start
			}
		}
		for _, end := range commandEnds {
			if end > effective {
				return true
			}
		}
	}
	return false
}

// declChangesEnv reports whether a declaration clause exports a variable or
// assigns one the environment already holds. Flag words are read literally:
// a flag word that is not plain text (`declare $F X=1`, a quoted operand)
// cannot be read and counts as exporting — fail closed.
func declChangesEnv(d *syntax.DeclClause, inEnv func(string) bool) bool {
	variant := ""
	if d.Variant != nil {
		variant = d.Variant.Value
	}
	exporting := false
	hasOperand := false
	for _, arg := range d.Args {
		if arg.Name != nil {
			hasOperand = true
			if inEnv(arg.Name.Value) {
				return true
			}
			continue
		}
		if !arg.Naked || arg.Value == nil {
			continue
		}
		lit := arg.Value.Lit()
		switch {
		case lit == "":
			// Not plain text: an unreadable flag or a quoted operand.
			exporting, hasOperand = true, true
		case strings.HasPrefix(lit, "-") || strings.HasPrefix(lit, "+"):
			if strings.ContainsRune(lit, 'x') {
				exporting = true
			}
		default:
			hasOperand = true
		}
	}
	if !hasOperand {
		return false // `export`, `export -p`, `declare -x`: they list, not set
	}
	return variant == "export" || exporting
}

// callDeclaresVariable reports whether a command is a declaration builtin
// reached through `builtin` or `command` (`builtin export X=1`): it parses as
// an ordinary command, but sets and may export a variable in the running
// shell. Any operands count — fail closed rather than re-parse its flags.
func callDeclaresVariable(ce *syntax.CallExpr) bool {
	if len(ce.Args) < 2 {
		return false
	}
	switch ce.Args[0].Lit() {
	case "builtin", "command":
	default:
		return false
	}
	for _, w := range ce.Args[1:] {
		lit := w.Lit()
		if strings.HasPrefix(lit, "-") {
			continue // command -p export …
		}
		return declBuiltins[lit]
	}
	return false
}

// arithAssignOp reports whether an arithmetic binary operator assigns to its
// left operand (=, +=, …, **=).
func arithAssignOp(op syntax.BinAritOperator) bool {
	switch op {
	case syntax.Assgn, syntax.AddAssgn, syntax.SubAssgn, syntax.MulAssgn,
		syntax.QuoAssgn, syntax.RemAssgn, syntax.AndAssgn, syntax.OrAssgn,
		syntax.XorAssgn, syntax.ShlAssgn, syntax.ShrAssgn, syntax.AndBoolAssgn,
		syntax.OrBoolAssgn, syntax.XorBoolAssgn, syntax.PowAssgn:
		return true
	}
	return false
}

// arithNameChanges reports whether the target of an arithmetic assignment or
// increment is a variable the environment holds. A target that is not a
// plain name (`${N}=5`, `a[$i]=1`) cannot be read and counts — fail closed.
func arithNameChanges(x syntax.ArithmExpr, inEnv func(string) bool) bool {
	w, ok := x.(*syntax.Word)
	if !ok {
		return true
	}
	name := w.Lit()
	if name == "" {
		return true
	}
	if i := strings.IndexByte(name, '['); i >= 0 {
		name = name[:i]
	}
	return inEnv(name)
}
