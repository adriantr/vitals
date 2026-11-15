// Package hclx reads Terraform `locals` without Terraform: static parts evaluate, while resource
// attributes and remote state stay as an Unknown carrying their source text.
package hclx

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Unknown is a value Terraform would only know at plan time; Src is its expression as written.
type Unknown struct{ Src string }

func (u Unknown) String() string { return u.Src }

// Stack holds every `locals` entry of one Terraform root module (a directory of .tf files).
type Stack struct {
	vars   map[string]any
	locals map[string]hclsyntax.Expression
	srcs   map[string][]byte
	done   map[string]any
	busy   map[string]bool
	// Warnings name data an expression dropped for an unknown value, so gaps are loud.
	Warnings []string
}

// Load parses every .tf file in dir. vars supplies var.* values, typically env and project.
func Load(dir string, vars map[string]any) (*Stack, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.tf"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .tf files in %s", dir)
	}
	s := &Stack{vars: vars, locals: map[string]hclsyntax.Expression{}, srcs: map[string][]byte{}, done: map[string]any{}, busy: map[string]bool{}}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		file, diags := hclsyntax.ParseConfig(raw, path, hcl.InitialPos)
		if diags.HasErrors() {
			return nil, fmt.Errorf("parse %s: %s", path, diags.Error())
		}
		s.srcs[path] = raw
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}
		for _, block := range body.Blocks {
			if block.Type != "locals" {
				continue
			}
			for name, attr := range block.Body.Attributes {
				s.locals[name] = attr.Expr
			}
		}
	}
	return s, nil
}

// Local evaluates one local by name.
func (s *Stack) Local(name string) (any, error) {
	if v, ok := s.done[name]; ok {
		return v, nil
	}
	expr, ok := s.locals[name]
	if !ok {
		return nil, fmt.Errorf("local.%s is not defined", name)
	}
	if s.busy[name] {
		return Unknown{Src: "local." + name}, nil
	}
	s.busy[name] = true
	v := s.eval(expr, nil)
	delete(s.busy, name)
	s.done[name] = v
	return v, nil
}

func (s *Stack) warn(expr hclsyntax.Expression, message string) {
	r := expr.Range()
	text := fmt.Sprintf("%s:%d: %s", filepath.Base(r.Filename), r.Start.Line, message)
	if !slices.Contains(s.Warnings, text) {
		s.Warnings = append(s.Warnings, text)
	}
}

// Location returns file:line of a local's definition, for provenance.
func (s *Stack) Location(name string) string {
	expr, ok := s.locals[name]
	if !ok {
		return ""
	}
	r := expr.Range()
	return fmt.Sprintf("%s:%d", r.Filename, r.Start.Line)
}

func (s *Stack) src(expr hclsyntax.Expression) string {
	r := expr.Range()
	raw := s.srcs[r.Filename]
	if raw == nil || r.End.Byte > len(raw) {
		return "?"
	}
	return strings.Join(strings.Fields(string(raw[r.Start.Byte:r.End.Byte])), " ")
}

type scope map[string]any

func (s *Stack) eval(expr hclsyntax.Expression, sc scope) any {
	unknown := Unknown{Src: s.src(expr)}
	switch e := expr.(type) {
	case *hclsyntax.LiteralValueExpr:
		return fromCty(e.Val)
	case *hclsyntax.TemplateWrapExpr:
		return s.eval(e.Wrapped, sc)
	case *hclsyntax.ParenthesesExpr:
		return s.eval(e.Expression, sc)
	case *hclsyntax.TemplateExpr:
		var b strings.Builder
		for _, part := range e.Parts {
			switch v := s.eval(part, sc).(type) {
			case string:
				b.WriteString(v)
			case Unknown:
				b.WriteString("${" + v.Src + "}")
			default:
				fmt.Fprint(&b, v)
			}
		}
		return b.String()
	case *hclsyntax.ObjectConsExpr:
		out := map[string]any{}
		for _, item := range e.Items {
			key, ok := s.key(item.KeyExpr, sc).(string)
			if !ok {
				return unknown
			}
			out[key] = s.eval(item.ValueExpr, sc)
		}
		return out
	case *hclsyntax.TupleConsExpr:
		out := make([]any, 0, len(e.Exprs))
		for _, item := range e.Exprs {
			out = append(out, s.eval(item, sc))
		}
		return out
	case *hclsyntax.ScopeTraversalExpr:
		return s.traverse(e.Traversal, sc, unknown)
	case *hclsyntax.RelativeTraversalExpr:
		return applyTraversal(s.eval(e.Source, sc), e.Traversal, unknown)
	case *hclsyntax.IndexExpr:
		coll := s.eval(e.Collection, sc)
		key := s.eval(e.Key, sc)
		return index(coll, key, unknown)
	case *hclsyntax.ConditionalExpr:
		cond, ok := s.eval(e.Condition, sc).(bool)
		if !ok {
			return unknown
		}
		if cond {
			return s.eval(e.TrueResult, sc)
		}
		return s.eval(e.FalseResult, sc)
	case *hclsyntax.BinaryOpExpr:
		return s.binary(e, sc, unknown)
	case *hclsyntax.UnaryOpExpr:
		if b, ok := s.eval(e.Val, sc).(bool); ok && e.Op == hclsyntax.OpLogicalNot {
			return !b
		}
		return unknown
	case *hclsyntax.FunctionCallExpr:
		return s.call(e, sc, unknown)
	case *hclsyntax.ForExpr:
		return s.forExpr(e, sc, unknown)
	default:
		return unknown
	}
}

// key resolves an object key: a bare word is a literal name, anything else is evaluated.
func (s *Stack) key(expr hclsyntax.Expression, sc scope) any {
	if k, ok := expr.(*hclsyntax.ObjectConsKeyExpr); ok {
		if name := hcl.ExprAsKeyword(k.Wrapped); name != "" && !k.ForceNonLiteral {
			return name
		}
		return s.eval(k.Wrapped, sc)
	}
	return s.eval(expr, sc)
}

func (s *Stack) traverse(t hcl.Traversal, sc scope, unknown Unknown) any {
	root := t.RootName()
	if v, ok := sc[root]; ok {
		return applyTraversal(v, t[1:], unknown)
	}
	if len(t) < 2 {
		return unknown
	}
	attr, ok := t[1].(hcl.TraverseAttr)
	if !ok {
		return unknown
	}
	switch root {
	case "var":
		if v, ok := s.vars[attr.Name]; ok {
			return applyTraversal(v, t[2:], unknown)
		}
	case "local":
		if v, err := s.Local(attr.Name); err == nil {
			return applyTraversal(v, t[2:], unknown)
		}
	}
	return unknown
}

func applyTraversal(v any, t hcl.Traversal, unknown Unknown) any {
	for _, step := range t {
		if _, ok := v.(Unknown); ok {
			return unknown
		}
		switch st := step.(type) {
		case hcl.TraverseAttr:
			v = index(v, st.Name, unknown)
		case hcl.TraverseIndex:
			v = index(v, fromCty(st.Key), unknown)
		default:
			return unknown
		}
	}
	return v
}

func index(coll, key any, unknown Unknown) any {
	switch c := coll.(type) {
	case map[string]any:
		if k, ok := key.(string); ok {
			if v, ok := c[k]; ok {
				return v
			}
		}
	case []any:
		if f, ok := key.(float64); ok && int(f) >= 0 && int(f) < len(c) {
			return c[int(f)]
		}
	}
	return unknown
}

func (s *Stack) binary(e *hclsyntax.BinaryOpExpr, sc scope, unknown Unknown) any {
	l, r := s.eval(e.LHS, sc), s.eval(e.RHS, sc)
	if _, ok := l.(Unknown); ok {
		return unknown
	}
	if _, ok := r.(Unknown); ok {
		return unknown
	}
	switch e.Op {
	case hclsyntax.OpEqual:
		return fmt.Sprint(l) == fmt.Sprint(r)
	case hclsyntax.OpNotEqual:
		return fmt.Sprint(l) != fmt.Sprint(r)
	case hclsyntax.OpLogicalAnd:
		lb, lok := l.(bool)
		rb, rok := r.(bool)
		if lok && rok {
			return lb && rb
		}
	case hclsyntax.OpLogicalOr:
		lb, lok := l.(bool)
		rb, rok := r.(bool)
		if lok && rok {
			return lb || rb
		}
	}
	return unknown
}

func (s *Stack) call(e *hclsyntax.FunctionCallExpr, sc scope, unknown Unknown) any {
	args := make([]any, 0, len(e.Args))
	for _, a := range e.Args {
		args = append(args, s.eval(a, sc))
	}
	switch e.Name {
	case "concat":
		var out []any
		for _, a := range args {
			if list, ok := a.([]any); ok {
				out = append(out, list...)
			} else {
				out = append(out, a)
			}
		}
		return out
	case "flatten":
		return flatten(args)
	case "merge":
		out := map[string]any{}
		for i, a := range args {
			m, ok := a.(map[string]any)
			if !ok {
				s.warn(e.Args[i], "merge argument is unknown; its keys are missing")
				continue
			}
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	case "toset", "distinct":
		if list, ok := first(args).([]any); ok {
			return dedupe(list)
		}
	case "tolist":
		if list, ok := first(args).([]any); ok {
			return list
		}
	case "compact":
		if list, ok := first(args).([]any); ok {
			var out []any
			for _, item := range list {
				if item != nil && item != "" {
					out = append(out, item)
				}
			}
			return out
		}
	case "keys":
		if m, ok := first(args).(map[string]any); ok {
			keys := make([]any, 0, len(m))
			for _, k := range slices.Sorted(maps.Keys(m)) {
				keys = append(keys, k)
			}
			return keys
		}
	case "lower", "upper":
		if str, ok := first(args).(string); ok {
			if e.Name == "lower" {
				return strings.ToLower(str)
			}
			return strings.ToUpper(str)
		}
	case "format":
		if f, ok := first(args).(string); ok {
			return fmt.Sprintf(f, args[1:]...)
		}
	case "lookup":
		if len(args) >= 2 {
			v := index(args[0], args[1], unknown)
			if _, missing := v.(Unknown); missing && len(args) == 3 {
				return args[2]
			}
			return v
		}
	case "try", "coalesce":
		for _, a := range args {
			if _, bad := a.(Unknown); !bad && a != nil {
				return a
			}
		}
	}
	return unknown
}

func (s *Stack) forExpr(e *hclsyntax.ForExpr, sc scope, unknown Unknown) any {
	coll := s.eval(e.CollExpr, sc)
	type pair struct {
		k any
		v any
	}
	var items []pair
	switch c := coll.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(c)) {
			items = append(items, pair{k, c[k]})
		}
	case []any:
		for i, v := range c {
			items = append(items, pair{float64(i), v})
		}
	default:
		return unknown
	}
	listOut := []any{}
	mapOut := map[string]any{}
	for _, it := range items {
		inner := scope{}
		for k, v := range sc {
			inner[k] = v
		}
		if e.KeyVar != "" {
			inner[e.KeyVar] = it.k
		}
		inner[e.ValVar] = it.v
		if e.CondExpr != nil {
			cond, known := s.eval(e.CondExpr, inner).(bool)
			if !known {
				s.warn(e.CondExpr, "for-expression condition is unknown; the item is dropped")
			}
			if !cond {
				continue
			}
		}
		val := s.eval(e.ValExpr, inner)
		if e.KeyExpr == nil {
			listOut = append(listOut, val)
			continue
		}
		key, ok := s.eval(e.KeyExpr, inner).(string)
		if !ok {
			return unknown
		}
		if e.Group {
			existing, _ := mapOut[key].([]any)
			mapOut[key] = append(existing, val)
			continue
		}
		mapOut[key] = val
	}
	if e.KeyExpr == nil {
		return listOut
	}
	return mapOut
}

func fromCty(v cty.Value) any {
	if !v.IsKnown() || v.IsNull() {
		return nil
	}
	switch {
	case v.Type() == cty.String:
		return v.AsString()
	case v.Type() == cty.Number:
		f, _ := v.AsBigFloat().Float64()
		return f
	case v.Type() == cty.Bool:
		return v.True()
	default:
		return v.GoString()
	}
}

func flatten(items []any) []any {
	var out []any
	for _, item := range items {
		if list, ok := item.([]any); ok {
			out = append(out, flatten(list)...)
		} else {
			out = append(out, item)
		}
	}
	return out
}

func dedupe(list []any) []any {
	seen := map[string]bool{}
	var out []any
	for _, item := range list {
		key := fmt.Sprint(item)
		if seen[key] || item == nil {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

func first(args []any) any {
	if len(args) == 0 {
		return nil
	}
	return args[0]
}

// Strings flattens a value into its string leaves; unknowns render as their source text.
func Strings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case Unknown:
		return []string{t.Src}
	case []any:
		var out []string
		for _, item := range t {
			out = append(out, Strings(item)...)
		}
		return out
	default:
		return nil
	}
}

// Map returns v as a map, or nil.
func Map(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// Keys returns a map's keys sorted.

// Field reads a string field of a map, or "".
func Field(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case Unknown:
		return v.Src
	default:
		return ""
	}
}

// Contains reports whether v's string leaves include s.
func Contains(v any, s string) bool { return slices.Contains(Strings(v), s) }
