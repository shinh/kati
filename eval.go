package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"time"
)

const (
	FILE_EXISTS       = 0
	FILE_NOT_EXISTS   = 1
	FILE_INCONSISTENT = 2 // Modified during kati is running.
)

type ReadMakefile struct {
	Filename string
	Hash     [sha1.Size]byte
	State    int32
}

type EvalResult struct {
	vars     Vars
	rules    []*Rule
	ruleVars map[string]Vars
	readMks  []*ReadMakefile
	exports  map[string]bool
}

type Evaluator struct {
	paramVars    []tmpval // $1 => paramVars[1]
	outVars      Vars
	outRules     []*Rule
	outRuleVars  map[string]Vars
	vars         Vars
	lastRule     *Rule
	currentScope Vars
	avoidIO      bool
	hasIO        bool
	readMks      map[string]*ReadMakefile
	exports      map[string]bool

	filename string
	lineno   int
}

func newEvaluator(vars map[string]Var) *Evaluator {
	return &Evaluator{
		outVars:     make(Vars),
		vars:        vars,
		outRuleVars: make(map[string]Vars),
		readMks:     make(map[string]*ReadMakefile),
		exports:     make(map[string]bool),
	}
}

func (ev *Evaluator) args(buf *buffer, args ...Value) [][]byte {
	var pos []int
	for _, arg := range args {
		arg.Eval(buf, ev)
		pos = append(pos, buf.Len())
	}
	v := buf.Bytes()
	buf.args = buf.args[:0]
	s := 0
	for _, p := range pos {
		buf.args = append(buf.args, v[s:p])
		s = p
	}
	return buf.args
}

func (ev *Evaluator) evalAssign(ast *AssignAST) {
	ev.lastRule = nil
	lhs, rhs := ev.evalAssignAST(ast)
	Log("ASSIGN: %s=%q (flavor:%q)", lhs, rhs, rhs.Flavor())
	if len(lhs) == 0 {
		Error(ast.filename, ast.lineno, "*** empty variable name.")
	}
	ev.outVars.Assign(lhs, rhs)
}

func (ev *Evaluator) evalAssignAST(ast *AssignAST) (string, Var) {
	ev.filename = ast.filename
	ev.lineno = ast.lineno

	v, _, err := parseExpr([]byte(ast.lhs), nil)
	if err != nil {
		panic(fmt.Errorf("parse %s:%d %v", ev.filename, ev.lineno, err))
	}
	var lhs string
	switch v := v.(type) {
	case literal:
		lhs = string(v)
	case tmpval:
		lhs = string(v)
	default:
		buf := newBuf()
		v.Eval(buf, ev)
		lhs = string(trimSpaceBytes(buf.Bytes()))
		freeBuf(buf)
	}
	rhs := ast.evalRHS(ev, lhs)
	return lhs, rhs
}

func (ev *Evaluator) setTargetSpecificVar(assign *AssignAST, output string) {
	vars, present := ev.outRuleVars[output]
	if !present {
		vars = make(Vars)
		ev.outRuleVars[output] = vars
	}
	ev.currentScope = vars
	lhs, rhs := ev.evalAssignAST(assign)
	Log("rule outputs:%q assign:%q=%q (flavor:%q)", output, lhs, rhs, rhs.Flavor())
	vars.Assign(lhs, TargetSpecificVar{v: rhs, op: assign.op})
	ev.currentScope = nil
}

func (ev *Evaluator) evalMaybeRule(ast *MaybeRuleAST) {
	ev.lastRule = nil
	ev.filename = ast.filename
	ev.lineno = ast.lineno

	expr := ast.expr
	if ast.semicolonIndex >= 0 {
		expr = expr[0:ast.semicolonIndex]
	}
	if ast.equalIndex >= 0 {
		expr = expr[0:ast.equalIndex]
	}
	lexpr, _, err := parseExpr([]byte(expr), nil)
	if err != nil {
		panic(fmt.Errorf("parse %s:%d %v", ev.filename, ev.lineno, err))
	}
	buf := newBuf()
	lexpr.Eval(buf, ev)
	line := buf.Bytes()
	if ast.equalIndex >= 0 {
		line = append(line, []byte(ast.expr[ast.equalIndex:])...)
	}
	Log("rule? %q=>%q", ast.expr, line)

	// See semicolon.mk.
	if len(bytes.TrimRight(line, " \t\n;")) == 0 {
		freeBuf(buf)
		return
	}

	rule := &Rule{
		filename: ast.filename,
		lineno:   ast.lineno,
	}
	assign, err := rule.parse(line)
	if err != nil {
		Error(ast.filename, ast.lineno, "%v", err.Error())
	}
	freeBuf(buf)
	Log("rule %q => outputs:%q, inputs:%q", line, rule.outputs, rule.inputs)

	// TODO: Pretty print.
	//Log("RULE: %s=%s (%d commands)", lhs, rhs, len(cmds))

	if assign != nil {
		if ast.semicolonIndex >= 0 {
			// TODO(ukai): reuse lexpr above?
			lexpr, _, err := parseExpr([]byte(ast.expr), nil)
			if err != nil {
				panic(fmt.Errorf("parse %s:%d %v", ev.filename, ev.lineno, err))
			}
			buf = newBuf()
			lexpr.Eval(buf, ev)
			assign, err = rule.parse(buf.Bytes())
			if err != nil {
				Error(ast.filename, ast.lineno, "%v", err.Error())
			}
			freeBuf(buf)
		}
		for _, output := range rule.outputs {
			ev.setTargetSpecificVar(assign, output)
		}
		for _, output := range rule.outputPatterns {
			ev.setTargetSpecificVar(assign, output.String())
		}
		return
	}

	if ast.semicolonIndex > 0 {
		rule.cmds = append(rule.cmds, ast.expr[ast.semicolonIndex+1:])
	}
	Log("rule outputs:%q cmds:%q", rule.outputs, rule.cmds)
	ev.lastRule = rule
	ev.outRules = append(ev.outRules, rule)
}

func (ev *Evaluator) evalCommand(ast *CommandAST) {
	ev.filename = ast.filename
	ev.lineno = ast.lineno
	if ev.lastRule == nil {
		// This could still be an assignment statement. See
		// assign_after_tab.mk.
		if strings.IndexByte(ast.cmd, '=') >= 0 {
			line := trimLeftSpace(ast.cmd)
			mk, err := ParseMakefileString(line, ast.filename, ast.lineno)
			if err != nil {
				panic(err)
			}
			if len(mk.stmts) == 1 && mk.stmts[0].(*AssignAST) != nil {
				ev.eval(mk.stmts[0])
			}
			return
		}
		// Or, a comment is OK.
		if strings.TrimSpace(ast.cmd)[0] == '#' {
			return
		}
		Error(ast.filename, ast.lineno, "*** commands commence before first target.")
	}
	ev.lastRule.cmds = append(ev.lastRule.cmds, ast.cmd)
	if ev.lastRule.cmdLineno == 0 {
		ev.lastRule.cmdLineno = ast.lineno
	}
}

func (ev *Evaluator) LookupVar(name string) Var {
	if ev.currentScope != nil {
		v := ev.currentScope.Lookup(name)
		if v.IsDefined() {
			return v
		}
	}
	v := ev.outVars.Lookup(name)
	if v.IsDefined() {
		return v
	}
	return ev.vars.Lookup(name)
}

func (ev *Evaluator) LookupVarInCurrentScope(name string) Var {
	if ev.currentScope != nil {
		v := ev.currentScope.Lookup(name)
		return v
	}
	v := ev.outVars.Lookup(name)
	if v.IsDefined() {
		return v
	}
	return ev.vars.Lookup(name)
}

// Only for a few special uses such as getting SHELL and handling
// export/unexport.
func (ev *Evaluator) EvaluateVar(name string) string {
	var buf bytes.Buffer
	ev.LookupVar(name).Eval(&buf, ev)
	return buf.String()
}

func (ev *Evaluator) evalIncludeFile(fname string, c []byte) error {
	t := time.Now()
	defer func() {
		addStats("include", literal(fname), t)
	}()
	mk, err, ok := LookupMakefileCache(fname)
	if !ok {
		Log("Reading makefile %q", fname)
		mk, err = ParseMakefile(c, fname)
	}
	if err != nil {
		return err
	}
	makefileList := ev.outVars.Lookup("MAKEFILE_LIST")
	makefileList = makefileList.Append(ev, mk.filename)
	ev.outVars.Assign("MAKEFILE_LIST", makefileList)

	for _, stmt := range mk.stmts {
		ev.eval(stmt)
	}
	return nil
}

func (ev *Evaluator) updateReadMakefile(fn string, c []byte, st int32) {
	if !useCache {
		return
	}

	h := sha1.Sum(c)
	rm, present := ev.readMks[fn]
	if present {
		switch rm.State {
		case FILE_EXISTS:
			if st != FILE_EXISTS {
				Warn(ev.filename, ev.lineno, "%s was removed after the previous read", fn)
			} else if !bytes.Equal(h[:], rm.Hash[:]) {
				Warn(ev.filename, ev.lineno, "%s was modified after the previous read", fn)
				ev.readMks[fn].State = FILE_INCONSISTENT
			}
			return
		case FILE_NOT_EXISTS:
			if st != FILE_NOT_EXISTS {
				Warn(ev.filename, ev.lineno, "%s was created after the previous read", fn)
				ev.readMks[fn].State = FILE_INCONSISTENT
			}
		case FILE_INCONSISTENT:
			return
		}
	} else {
		ev.readMks[fn] = &ReadMakefile{
			Filename: fn,
			Hash:     h,
			State:    st,
		}
	}
}

func (ev *Evaluator) evalInclude(ast *IncludeAST) {
	ev.lastRule = nil
	ev.filename = ast.filename
	ev.lineno = ast.lineno

	Log("%s:%d include %q", ev.filename, ev.lineno, ast.expr)
	v, _, err := parseExpr([]byte(ast.expr), nil)
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	v.Eval(&buf, ev)
	pats := splitSpaces(buf.String())
	buf.Reset()

	var files []string
	for _, pat := range pats {
		if strings.Contains(pat, "*") || strings.Contains(pat, "?") {
			matched, err := filepath.Glob(pat)
			if err != nil {
				panic(err)
			}
			files = append(files, matched...)
		} else {
			files = append(files, pat)
		}
	}

	for _, fn := range files {
		if ignoreOptionalInclude != "" && ast.op == "-include" && strings.HasPrefix(fn, ignoreOptionalInclude) {
			continue
		}
		c, err := ioutil.ReadFile(fn)
		if err != nil {
			if ast.op == "include" {
				Error(ev.filename, ev.lineno, fmt.Sprintf("%v\nNOTE: kati does not support generating missing makefiles", err))
			} else {
				ev.updateReadMakefile(fn, nil, FILE_NOT_EXISTS)
				continue
			}
		}
		ev.updateReadMakefile(fn, c, FILE_EXISTS)
		err = ev.evalIncludeFile(fn, c)
		if err != nil {
			panic(err)
		}
	}
}

func (ev *Evaluator) evalIf(ast *IfAST) {
	var isTrue bool
	switch ast.op {
	case "ifdef", "ifndef":
		expr, _, err := parseExpr([]byte(ast.lhs), nil)
		if err != nil {
			panic(fmt.Errorf("ifdef parse %s:%d %v", ast.filename, ast.lineno, err))
		}
		buf := newBuf()
		expr.Eval(buf, ev)
		v := ev.LookupVar(buf.String())
		buf.Reset()
		v.Eval(buf, ev)
		value := buf.String()
		val := buf.Len()
		freeBuf(buf)
		isTrue = (val > 0) == (ast.op == "ifdef")
		Log("%s lhs=%q value=%q => %t", ast.op, ast.lhs, value, isTrue)
	case "ifeq", "ifneq":
		lexpr, _, err := parseExpr([]byte(ast.lhs), nil)
		if err != nil {
			panic(fmt.Errorf("ifeq lhs parse %s:%d %v", ast.filename, ast.lineno, err))
		}
		rexpr, _, err := parseExpr([]byte(ast.rhs), nil)
		if err != nil {
			panic(fmt.Errorf("ifeq rhs parse %s:%d %v", ast.filename, ast.lineno, err))
		}
		buf := newBuf()
		params := ev.args(buf, lexpr, rexpr)
		lhs := string(params[0])
		rhs := string(params[1])
		freeBuf(buf)
		isTrue = (lhs == rhs) == (ast.op == "ifeq")
		Log("%s lhs=%q %q rhs=%q %q => %t", ast.op, ast.lhs, lhs, ast.rhs, rhs, isTrue)
	default:
		panic(fmt.Sprintf("unknown if statement: %q", ast.op))
	}

	var stmts []AST
	if isTrue {
		stmts = ast.trueStmts
	} else {
		stmts = ast.falseStmts
	}
	for _, stmt := range stmts {
		ev.eval(stmt)
	}
}

func (ev *Evaluator) evalExport(ast *ExportAST) {
	ev.lastRule = nil
	ev.filename = ast.filename
	ev.lineno = ast.lineno

	v, _, err := parseExpr(ast.expr, nil)
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	v.Eval(&buf, ev)
	for _, n := range splitSpacesBytes(buf.Bytes()) {
		ev.exports[string(n)] = ast.export
	}
}

func (ev *Evaluator) eval(ast AST) {
	ast.eval(ev)
}

func createReadMakefileArray(mp map[string]*ReadMakefile) []*ReadMakefile {
	var r []*ReadMakefile
	for _, v := range mp {
		r = append(r, v)
	}
	return r
}

func Eval(mk Makefile, vars Vars) (er *EvalResult, err error) {
	ev := newEvaluator(vars)
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in eval %s: %v", mk.filename, r)
		}
	}()

	makefile_list := vars.Lookup("MAKEFILE_LIST")
	makefile_list = makefile_list.Append(ev, mk.filename)
	ev.outVars.Assign("MAKEFILE_LIST", makefile_list)

	for _, stmt := range mk.stmts {
		ev.eval(stmt)
	}

	return &EvalResult{
		vars:     ev.outVars,
		rules:    ev.outRules,
		ruleVars: ev.outRuleVars,
		readMks:  createReadMakefileArray(ev.readMks),
		exports:  ev.exports,
	}, nil
}
