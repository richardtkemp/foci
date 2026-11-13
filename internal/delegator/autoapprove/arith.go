package autoapprove

import (
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ---------- Arithmetic contexts and array subscripts (#3064) ----------
//
// Bash evaluates a variable's VALUE as an arithmetic expression, recursively,
// wherever it does arithmetic: `x='a[$(id)]'; (( x ))` runs id, because the
// text of x is parsed as an expression and the array subscript inside it is
// expanded — command substitution included. The walker validates only the
// substitutions that are explicit in the AST, never one hidden in a value, so
// a read-only rule set used to approve that command (the #2968 review, F2).
//
// Dick's ruling 2026-11-13: an arithmetic context that names a variable (a
// bare name or a parameter expansion) is never auto-approved; pure-literal
// arithmetic may still be. Every context is covered, each measured with bash
// 5.2 to run a hidden substitution:
//
//   - (( )) and `for (( ; ; ))`;
//   - let;
//   - $(( )) and the old $[ ];
//   - array subscripts: ${a[i]}, a[i]=v, ([i]=v), and ${s:off:len};
//   - the numeric comparisons of [[ ]] (-eq -ne -lt -le -gt -ge), whose
//     operands bash evaluates as arithmetic — `[[ x -eq 1 ]]` reads x;
//   - declare/typeset/local -i, whose assignments are evaluated;
//   - the places a NAME is looked up with its subscript evaluated: [[ -v n ]],
//     [[ -R n ]], test/[ -v and -R (testCommandUnsafe), and the indirect
//     expansion ${!x}, whose value names the variable to read.
//
// A substring blacklist on values ("$(" in a value) is not a fix: the
// recursion reaches through any number of named variables, and the
// environment holds values the symbol table never sees.

// numericSpecialParams are the special parameters whose value is always a
// number bash itself produced ($#, $?, $$, $!): they cannot carry an
// expression, so naming one does not hide anything.
var numericSpecialParams = map[string]bool{"#": true, "?": true, "$": true, "!": true}

// arithLiteralRe matches one arithmetic integer literal token: a decimal,
// octal (0…), hex (0x…) or base#digits number. It starts with a digit, so it
// is never a variable name; letters after the first digit are digits of the
// base (16#ff, 36#zz), never a name bash looks up.
var arithLiteralRe = regexp.MustCompile(`^[0-9][0-9A-Za-z#@_]*$`)

// plainNameRe matches a plain shell variable name, with no subscript.
var plainNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// arithLeafLiteral reports whether one leaf word of an arithmetic expression
// is a number: an integer literal, or a numeric special parameter ($#, ${?}).
// A bare name, any other parameter expansion, a substitution or a quoted part
// is not — bash would evaluate its text as a further expression.
func arithLeafLiteral(w *syntax.Word) bool {
	if w == nil || len(w.Parts) != 1 {
		return false
	}
	switch p := w.Parts[0].(type) {
	case *syntax.Lit:
		return arithLiteralRe.MatchString(p.Value)
	case *syntax.ParamExp:
		return numericParamExp(p)
	}
	return false
}

// numericParamExp reports whether a parameter expansion is a plain reference
// to a numeric special parameter: $#, ${#}, $?, … with no operator attached.
func numericParamExp(p *syntax.ParamExp) bool {
	return p.Param != nil && numericSpecialParams[p.Param.Value] &&
		!p.Excl && !p.Length && !p.Width && !p.IsSet && p.Flags == nil && p.NestedParam == nil &&
		p.Index == nil && p.Slice == nil && p.Repl == nil && p.Names == 0 && p.Exp == nil && len(p.Modifiers) == 0
}

// arithLiteralOnly reports whether an arithmetic expression names no
// variable: every leaf is a number (arithLeafLiteral). Operators, parentheses
// and the ternary are structure and carry no text of their own. A nil
// expression (an empty `for ((;;))` clause) is literal.
func arithLiteralOnly(x syntax.ArithmExpr) bool {
	switch n := x.(type) {
	case nil:
		return true
	case *syntax.Word:
		return arithLeafLiteral(n)
	case *syntax.BinaryArithm:
		return arithLiteralOnly(n.X) && arithLiteralOnly(n.Y)
	case *syntax.UnaryArithm:
		return arithLiteralOnly(n.X)
	case *syntax.ParenArithm:
		return arithLiteralOnly(n.X)
	}
	return false // an unknown node — fail closed
}

// subscriptLiteral reports whether an array subscript is safe to evaluate:
// the whole-array subscripts @ and *, or pure-literal arithmetic.
func subscriptLiteral(x syntax.ArithmExpr) bool {
	return wholeArraySubscript(x) || arithLiteralOnly(x)
}

// testIntOperandLiteral reports whether an operand of a [[ ]] numeric
// comparison is a number: an optional sign and an integer literal, or a
// numeric special parameter.
func testIntOperandLiteral(x syntax.TestExpr) bool {
	w, ok := x.(*syntax.Word)
	if !ok {
		return false
	}
	if len(w.Parts) == 1 {
		if lit, ok := w.Parts[0].(*syntax.Lit); ok {
			return arithLiteralRe.MatchString(strings.TrimLeft(lit.Value, "+-"))
		}
	}
	return arithLeafLiteral(w)
}

// testArithOp reports whether a [[ ]] binary operator compares its operands
// as integers — bash evaluates each operand as an arithmetic expression.
func testArithOp(op syntax.BinTestOperator) bool {
	switch op {
	case syntax.TsEql, syntax.TsNeq, syntax.TsLeq, syntax.TsGeq, syntax.TsLss, syntax.TsGtr:
		return true
	}
	return false
}

// declIntegerFlag reports whether a declaration gives its variables the
// integer attribute (-i, in any bundle), after which every assignment to them
// is evaluated as arithmetic. A flag word that is not plain text cannot be
// read and counts — fail closed.
func declIntegerFlag(d *syntax.DeclClause) bool {
	for _, arg := range d.Args {
		if arg.Name != nil || !arg.Naked || arg.Value == nil {
			continue
		}
		lit := arg.Value.Lit()
		if lit == "" {
			return true // not plain text: an unreadable flag
		}
		if strings.HasPrefix(lit, "-") && strings.ContainsRune(lit, 'i') {
			return true
		}
	}
	return false
}

// arithNodeUnsafe reports whether one AST node is an arithmetic context (or a
// subscript-evaluating name lookup) that names a variable — see the section
// comment for the list. The walker asks it about every node it visits.
func arithNodeUnsafe(node syntax.Node) bool {
	switch n := node.(type) {
	case *syntax.ArithmCmd:
		return !arithLiteralOnly(n.X)
	case *syntax.ArithmExp:
		return !arithLiteralOnly(n.X)
	case *syntax.LetClause:
		for _, x := range n.Exprs {
			if !arithLiteralOnly(x) {
				return true
			}
		}
	case *syntax.CStyleLoop:
		return !arithLiteralOnly(n.Init) || !arithLiteralOnly(n.Cond) || !arithLiteralOnly(n.Post)
	case *syntax.ParamExp:
		return paramExpUnsafe(n)
	case *syntax.Assign:
		return n.Index != nil && !subscriptLiteral(n.Index)
	case *syntax.ArrayElem:
		return n.Index != nil && !subscriptLiteral(n.Index)
	case *syntax.BinaryTest:
		return testArithOp(n.Op) && !(testIntOperandLiteral(n.X) && testIntOperandLiteral(n.Y))
	case *syntax.UnaryTest:
		if n.Op == syntax.TsVarSet || n.Op == syntax.TsRefVar {
			w, ok := n.X.(*syntax.Word)
			return !ok || !plainNameRe.MatchString(w.Lit())
		}
	case *syntax.DeclClause:
		return declIntegerFlag(n)
	}
	return false
}

// paramExpUnsafe reports whether a parameter expansion evaluates arithmetic
// that names a variable: a subscript (${a[i]}), a substring offset or length
// (${s:i:j}), or an indirect reference (${!x}: the VALUE of x is the name
// read, subscript and all). The prefix listings ${!p*}/${!p@} and the key
// listing ${!a[@]} read no value and stay approved.
func paramExpUnsafe(p *syntax.ParamExp) bool {
	if p.Index != nil && !subscriptLiteral(p.Index) {
		return true
	}
	if p.Slice != nil && (!arithLiteralOnly(p.Slice.Offset) || !arithLiteralOnly(p.Slice.Length)) {
		return true
	}
	if p.Excl && p.Names == 0 && !wholeArraySubscript(p.Index) {
		return true
	}
	return false
}

// wholeArraySubscript reports whether a subscript is @ or * — the whole
// array, with no element selected.
func wholeArraySubscript(x syntax.ArithmExpr) bool {
	w, ok := x.(*syntax.Word)
	if !ok {
		return false
	}
	lit := w.Lit()
	return lit == "@" || lit == "*"
}

// testCommandUnsafe judges the test and [ commands' -v and -R operators
// (#3064): each looks a NAME up, and a subscript in that name is evaluated as
// arithmetic — `test -v 'a[$(id)]'` runs id. Unlike [[ ]], test parses its
// operators at run time, from the expanded words, so a variable can supply
// the operator or the name. The arguments are resolved with resolveArgWord
// (the same symbol table and environment as every other resolution); then
// the command refuses when:
//
//   - a -v or -R word is followed by a word that is not a plain name, or by
//     one that cannot be resolved;
//   - an unresolvable word (a substitution, an unknown variable) could itself
//     be -v or -R: two or more such words, or one beside a word holding '['
//     (the closing ] of `[` aside).
//
// Every other test — `[ -f "$x" ]`, `[ "$a" = "$b" ]` with known values,
// `test -n "$(…)"` alone — keeps its verdict.
func testCommandUnsafe(ce *syntax.CallExpr, vc *varCtx) bool {
	if len(ce.Args) == 0 {
		return false
	}
	name, ok := resolveArgWord(ce.Args[0], vc)
	if !ok || len(name) != 1 {
		return false // commandWordUnsafe refuses an unresolvable command word
	}
	base := name[0].visible
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if base != "test" && base != "[" {
		return false
	}
	type arg struct {
		text  string
		known bool
	}
	var args []arg
	for _, w := range ce.Args[1:] {
		words, ok := resolveArgWord(w, vc)
		if !ok {
			args = append(args, arg{})
			continue
		}
		for _, sw := range words {
			args = append(args, arg{text: sw.visible, known: true})
		}
	}
	if base == "[" && len(args) > 0 && args[len(args)-1].known && args[len(args)-1].text == "]" {
		args = args[:len(args)-1]
	}
	unknown, bracket := 0, false
	for i, a := range args {
		if !a.known {
			unknown++
			continue
		}
		if strings.Contains(a.text, "[") {
			bracket = true
		}
		if a.text == "-v" || a.text == "-R" {
			if i+1 < len(args) && (!args[i+1].known || !plainNameRe.MatchString(args[i+1].text)) {
				return true
			}
		}
	}
	return unknown >= 2 || unknown == 1 && bracket
}
