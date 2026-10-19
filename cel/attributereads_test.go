// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cel_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	proto3pb "cel.dev/cel-go/test/proto3pb"
)

func TestAttributeReads(t *testing.T) {
	// 输入覆盖嵌套对象、列表、Map 以及动态键，供各用例按表达式自行引用。
	baseVars := map[string]any{
		"request": map[string]any{
			"user": map[string]any{
				"name": "huevos",
				"role": "admin",
			},
		},
		"items": []any{
			map[string]any{"price": int64(1)},
			map[string]any{"price": int64(200)},
			map[string]any{"price": int64(3)},
		},
		"m": map[string]any{
			"x": int64(42),
		},
		"k":       "x",
		"i":       int64(1),
		"flag":    true,
		"a":       map[string]any{"b": "av"},
		"c":       map[string]any{"d": "cv"},
		"guard":   true,
		"numbers": []int64{1, 2, 3},
	}

	tests := []struct {
		name      string
		expr      string
		vars      map[string]any
		parseOnly bool
		want      []string
		// wantSet 用例的 Map 遍历顺序不确定，按集合比较。
		wantSet bool
	}{
		{
			name: "nested object leaf path",
			expr: `request.user.name == "huevos"`,
			want: []string{"request.user.name"},
		},
		{
			name: "multiple leaves in order",
			expr: `request.user.name == "huevos" && request.user.role == "admin"`,
			want: []string{"request.user.name", "request.user.role"},
		},
		{
			name: "repeated reads are preserved",
			expr: `request.user.name == request.user.name`,
			want: []string{"request.user.name", "request.user.name"},
		},
		{
			name: "static list index and trailing field",
			expr: `items[1].price == 200`,
			want: []string{"items[1].price"},
		},
		{
			name: "bare top-level variable",
			expr: `flag`,
			want: []string{"flag"},
		},
		{
			name: "dynamic map key expands to actual value",
			expr: `m[k] == 42`,
			want: []string{"k", "m.x"},
		},
		{
			name: "dynamic list index expands to actual value",
			expr: `items[i].price > 0`,
			want: []string{"i", "items[1].price"},
		},
		{
			name: "literal string index on map",
			expr: `m["x"] == 42`,
			want: []string{"m.x"},
		},
		{
			name: "dynamic key with non-identifier rendering",
			expr: `m[k] == 42`,
			vars: mergeVars(baseVars, map[string]any{
				"k": "a b",
				"m": map[string]any{"a b": int64(42)},
			}),
			want: []string{"k", `m["a b"]`},
		},
		{
			name: "successful has field presence check",
			expr: `has(request.user.name)`,
			want: []string{"request.user.name"},
		},
		{
			name: "has on absent field still counts the check",
			expr: `has(request.user.missing)`,
			want: []string{"request.user.missing"},
		},
		{
			name: "ternary only reads taken branch",
			expr: `flag ? request.user.name : request.user.role`,
			want: []string{"flag", "request.user.name"},
		},
		{
			name: "ternary false branch",
			expr: `flag ? request.user.name : request.user.role`,
			vars: mergeVars(baseVars, map[string]any{"flag": false}),
			want: []string{"flag", "request.user.role"},
		},
		{
			name: "logical and short circuit",
			expr: `false && request.user.name == "x"`,
			want: nil,
		},
		{
			name: "logical and reads until first false",
			expr: `guard && !flag && request.user.name == "x"`,
			want: []string{"guard", "flag"},
		},
		{
			name: "exists records range and only evaluated elements",
			expr: `items.exists(x, x.price > 100)`,
			// 第 0 个不命中、第 1 个命中后循环结束，第 2 个元素不读取。
			want: []string{"items", "items[0].price", "items[1].price"},
		},
		{
			name: "all stops on first failing element",
			expr: `items.all(x, x.price > 100)`,
			want: []string{"items", "items[0].price"},
		},
		{
			name: "filter reads predicates and appended elements",
			expr: `size(items.filter(x, x.price > 100)) == 1`,
			// 每个元素的判定都会读取 price；命中元素还会被整体放入结果列表，因此额外
			// 记录一次元素本身的读取（items[1]）。
			want: []string{"items", "items[0].price", "items[1].price", "items[1]", "items[2].price"},
		},
		{
			name: "map transform reads every element",
			expr: `size(items.map(x, x.price)) == 3`,
			want: []string{"items", "items[0].price", "items[1].price", "items[2].price"},
		},
		{
			name: "nested comprehension path",
			expr: `numbers.exists(x, x == 2) && items.exists(y, y.price == 3)`,
			want: []string{
				"numbers", "numbers[0]", "numbers[1]",
				"items", "items[0].price", "items[1].price", "items[2].price",
			},
		},
		{
			name:      "parse-only nested attribute",
			expr:      `request.user.name == "huevos"`,
			want:      []string{"request.user.name"},
			parseOnly: true,
		},
		{
			name:      "parse-only dynamic selection",
			expr:      `m[k] == 42`,
			want:      []string{"k", "m.x"},
			parseOnly: true,
		},
		{
			name:    "map comprehension with key-based dynamic lookup",
			expr:    `m.exists(k, m[k] == 42)`,
			wantSet: true,
			want:    []string{"m", "m.x"},
		},
	}

	for _, tst := range tests {
		t.Run(tst.name, func(t *testing.T) {
			input := tst.vars
			if input == nil {
				input = baseVars
			}
			prg := newReadTrackingProgram(t, tst.expr, tst.parseOnly)
			out, det, err := prg.Eval(input)
			if err != nil {
				t.Fatalf("Eval() failed: %v", err)
			}
			if det == nil {
				t.Fatalf("Eval() returned nil details with OptTrackAttributeReads enabled")
			}
			got := det.AttributeReads()
			assertReads(t, got, tst.want, tst.wantSet)
			// 返回切片应当是拷贝，修改不影响后续调用。
			if len(got) > 0 {
				got[0] = "mutated"
				if det.AttributeReads()[0] == "mutated" {
					t.Fatalf("AttributeReads() returned a mutable internal slice")
				}
			}
			t.Logf("expr=%q result=%v reads=%v", tst.expr, out.Value(), det.AttributeReads())
		})
	}
}

func TestAttributeReads_FailedEvaluation(t *testing.T) {
	// c 中缺少键 no_such_key，右操作数解析失败；左操作数已完成的读取保留，未解析的路径不记录。
	env := newReadsEnv(t)
	ast := mustCompile(t, env, `request.user.name == "huevos" && c.no_such_key == "x"`)
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, det, err := prg.Eval(map[string]any{
		"request": map[string]any{"user": map[string]any{"name": "huevos"}},
		"c":       map[string]any{},
	})
	if err == nil {
		t.Fatalf("Eval() succeeded unexpectedly: %v", out)
	}
	if det == nil {
		t.Fatalf("failed Eval() returned nil details")
	}
	assertReads(t, det.AttributeReads(), []string{"request.user.name"}, false)
}

func TestAttributeReads_DynamicIndexFailure(t *testing.T) {
	// items[5] 越界：下标 i 先被读取并保留，items[5].price 这条未成功解析的路径不记录。
	env := newReadsEnv(t)
	ast := mustCompile(t, env, `items[i].price`)
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	input := map[string]any{
		"items": []any{
			map[string]any{"price": int64(1)},
		},
		"i": int64(5),
	}
	_, det, err := prg.Eval(input)
	if err == nil {
		t.Fatalf("Eval() succeeded unexpectedly for out-of-bounds index")
	}
	assertReads(t, det.AttributeReads(), []string{"i"}, false)
}

func TestAttributeReads_MissingVariable(t *testing.T) {
	env, err := cel.NewEnv()
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	ast := mustParse(t, env, `missing.foo`)
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	_, det, err := prg.Eval(map[string]any{})
	if err == nil {
		t.Fatalf("Eval() succeeded unexpectedly for missing variable")
	}
	if reads := det.AttributeReads(); len(reads) != 0 {
		t.Fatalf("expected no reads on missing variable, got %v", reads)
	}
}

func TestAttributeReads_ConcurrentProgramReuse(t *testing.T) {
	env := newReadsEnv(t)
	ast := mustCompile(t, env, `items.exists(x, x.price > 100)`)
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}

	const goroutines = 32
	const iterations = 50
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				// 每次求值构造独立输入：不同长度与命中位置，验证结果互不混入。
				limit := (seed+n)%5 + 1
				items := make([]any, limit)
				hitAt := (seed + n) % limit
				for idx := range items {
					price := int64(1)
					if idx == hitAt {
						price = int64(200)
					}
					items[idx] = map[string]any{"price": price}
				}
				_, det, err := prg.Eval(map[string]any{"items": items})
				if err != nil {
					errCh <- err
					return
				}
				reads := det.AttributeReads()
				// range + 命中元素及其之前每个元素的 price 读取。
				want := []string{"items"}
				for idx := 0; idx <= hitAt; idx++ {
					want = append(want, fmt.Sprintf("items[%d].price", idx))
				}
				if !equalStrings(reads, want) {
					errCh <- fmt.Errorf("goroutine %d iteration %d: got %v, want %v", seed, n, reads, want)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestAttributeReads_WithStateTracking(t *testing.T) {
	env := newReadsEnv(t)
	ast := mustCompile(t, env, `request.user.name == "huevos" && items[1].price == 200`)
	prg, err := env.Program(ast,
		cel.EvalOptions(cel.OptTrackAttributeReads|cel.OptTrackState))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	_, det, err := prg.Eval(map[string]any{
		"request": map[string]any{"user": map[string]any{"name": "huevos"}},
		"items": []any{
			nil,
			map[string]any{"price": int64(200)},
		},
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	assertReads(t, det.AttributeReads(),
		[]string{"request.user.name", "items[1].price"}, false)
	if det.State() == nil {
		t.Fatalf("State() unavailable while OptTrackState enabled")
	}
	if ids := det.State().IDs(); len(ids) == 0 {
		t.Fatalf("State() recorded no expression values")
	}
}

func TestAttributeReads_DisabledKeepsLegacyBehavior(t *testing.T) {
	env := newReadsEnv(t)
	ast := mustCompile(t, env, `request.user.name`)
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, det, err := prg.Eval(map[string]any{
		"request": map[string]any{"user": map[string]any{"name": "huevos"}},
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	if got := out.Value(); got != "huevos" {
		t.Fatalf("Eval() = %v, want huevos", got)
	}
	if det != nil {
		t.Fatalf("Eval() returned details without tracking options: %v", det)
	}

	// ContextEval 的短路与错误行为同样保持原样。
	out2, det2, err2 := prg.ContextEval(context.Background(), map[string]any{
		"request": map[string]any{},
	})
	if err2 == nil {
		t.Fatalf("ContextEval() expected missing-key error, got %v", out2)
	}
	if det2 != nil {
		t.Fatalf("ContextEval() returned details without tracking options: %v", det2)
	}
}

func TestAttributeReads_ContextEval(t *testing.T) {
	env := newReadsEnv(t)
	ast := mustCompile(t, env, `items[i].price`)
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	_, det, err := prg.ContextEval(context.Background(), map[string]any{
		"items": []any{
			nil,
			map[string]any{"price": int64(200)},
		},
		"i": int64(1),
	})
	if err != nil {
		t.Fatalf("ContextEval() failed: %v", err)
	}
	assertReads(t, det.AttributeReads(), []string{"i", "items[1].price"}, false)
}

func TestAttributeReads_QualifiedRangeAndNestedLoop(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("data", cel.DynType))
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	ast := mustCompile(t, env,
		`data.groups.exists(g, g.members.exists(m, m.active))`)
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	input := map[string]any{
		"data": map[string]any{
			"groups": []any{
				map[string]any{"members": []any{
					map[string]any{"active": false},
					map[string]any{"active": true},
				}},
			},
		},
	}
	if _, det, err := prg.Eval(input); err != nil {
		t.Fatalf("Eval() failed: %v", err)
	} else {
		assertReads(t, det.AttributeReads(), []string{
			"data.groups",
			"data.groups[0].members",
			"data.groups[0].members[0].active",
			"data.groups[0].members[1].active",
		}, false)
	}
}

func TestAttributeReads_TypedMapKeys(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("u", cel.MapType(cel.UintType, cel.StringType)),
		cel.Variable("b", cel.MapType(cel.BoolType, cel.StringType)),
		cel.Variable("uk", cel.UintType),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	ast := mustCompile(t, env, `u[uk] == "v" && b[true] == "yes"`)
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	_, det, err := prg.Eval(map[string]any{
		"u":  map[uint64]any{uint64(3): "v"},
		"b":  map[bool]any{true: "yes"},
		"uk": uint64(3),
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	assertReads(t, det.AttributeReads(),
		[]string{"uk", "u[3]", "b[true]"}, false)
}

func TestAttributeReads_ProtoField(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Types(&proto3pb.TestAllTypes{}),
		cel.Variable("msg", cel.ObjectType("google.expr.proto3.test.TestAllTypes")),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	ast := mustCompile(t, env, `msg.single_string == "huevos"`)
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	_, det, err := prg.Eval(map[string]any{
		"msg": &proto3pb.TestAllTypes{SingleString: "huevos"},
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	assertReads(t, det.AttributeReads(), []string{"msg.single_string"}, false)
}

func TestAttributeReads_PartialEvalUnknownNotRecorded(t *testing.T) {
	env, err := cel.NewEnv(
		cel.Variable("groups", cel.ListType(cel.IntType)),
		cel.Variable("id", cel.IntType),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	ast := mustCompile(t, env, `groups.exists(t, t == id)`)
	prg, err := env.Program(ast,
		cel.EvalOptions(cel.OptTrackAttributeReads, cel.OptTrackState, cel.OptPartialEval))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	pvars, err := cel.PartialVars(
		map[string]any{"groups": []int64{1, 2, 3}},
		cel.AttributePattern("id"),
	)
	if err != nil {
		t.Fatalf("PartialVars() failed: %v", err)
	}
	out, det, err := prg.Eval(pvars)
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	if !types.IsUnknown(out) {
		t.Fatalf("expected unknown result, got %v", out)
	}
	// 已知的范围与元素读取被记录；未知变量 id 不会产生路径。
	assertReads(t, det.AttributeReads(),
		[]string{"groups", "groups[0]", "groups[1]", "groups[2]"}, false)
}

func TestAttributeReads_WithOptimize(t *testing.T) {
	env := newReadsEnv(t)
	// 常量表达式在规划期折叠，不应产生任何输入读取。
	ast := mustCompile(t, env, `request.user.name == "x" && 1 < 2`)
	prg, err := env.Program(ast,
		cel.EvalOptions(cel.OptTrackAttributeReads, cel.OptOptimize))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	_, det, err := prg.Eval(map[string]any{
		"request": map[string]any{"user": map[string]any{"name": "y"}},
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	assertReads(t, det.AttributeReads(), []string{"request.user.name"}, false)
}

func TestAttributeReads_WithCostTracking(t *testing.T) {
	env := newReadsEnv(t)
	ast := mustCompile(t, env, `items[1].price`)
	prg, err := env.Program(ast,
		cel.CostTracking(nil),
		cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	_, det, err := prg.Eval(map[string]any{
		"items": []any{nil, map[string]any{"price": int64(200)}},
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	assertReads(t, det.AttributeReads(), []string{"items[1].price"}, false)
	if det.ActualCost() == nil {
		t.Fatalf("ActualCost() unavailable while cost tracking enabled")
	}
}

func TestAttributeReads_ExhaustiveEvaluatesUnreachableBranches(t *testing.T) {
	env := newReadsEnv(t)
	ast := mustCompile(t, env, `false && request.user.name == "x"`)
	prg, err := env.Program(ast,
		cel.EvalOptions(cel.OptTrackAttributeReads, cel.OptExhaustiveEval))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	out, det, err := prg.Eval(map[string]any{
		"request": map[string]any{"user": map[string]any{"name": "huevos"}},
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	if out.Value() != false {
		t.Fatalf("Eval() = %v, want false", out.Value())
	}
	// 穷举模式关闭短路，&& 右侧即使在常规求值下不可达也会被读取。
	assertReads(t, det.AttributeReads(), []string{"request.user.name"}, false)
}

func newReadsEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(
		cel.Variable("request", cel.DynType),
		cel.Variable("items", cel.DynType),
		cel.Variable("m", cel.DynType),
		cel.Variable("k", cel.DynType),
		cel.Variable("i", cel.DynType),
		cel.Variable("flag", cel.DynType),
		cel.Variable("guard", cel.DynType),
		cel.Variable("a", cel.DynType),
		cel.Variable("c", cel.DynType),
		cel.Variable("numbers", cel.DynType),
	)
	if err != nil {
		t.Fatalf("NewEnv() failed: %v", err)
	}
	return env
}

func mustCompile(t *testing.T, env *cel.Env, expr string) *cel.Ast {
	t.Helper()
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile(%q) failed: %v", expr, iss.Err())
	}
	return ast
}

func mustParse(t *testing.T, env *cel.Env, expr string) *cel.Ast {
	t.Helper()
	ast, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Parse(%q) failed: %v", expr, iss.Err())
	}
	return ast
}

func newReadTrackingProgram(t *testing.T, expr string, parseOnly bool) cel.Program {
	t.Helper()
	env := newReadsEnv(t)
	var ast *cel.Ast
	if parseOnly {
		ast = mustParse(t, env, expr)
	} else {
		ast = mustCompile(t, env, expr)
	}
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	return prg
}

func mergeVars(base map[string]any, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertReads(t *testing.T, got, want []string, asSet bool) {
	t.Helper()
	if asSet {
		gs := append([]string(nil), got...)
		ws := append([]string(nil), want...)
		sort.Strings(gs)
		sort.Strings(ws)
		if !equalStrings(gs, ws) {
			t.Fatalf("attribute reads mismatch (set):\n got=%v\nwant=%v", gs, ws)
		}
		return
	}
	if !equalStrings(got, want) {
		t.Fatalf("attribute reads mismatch (ordered):\n got=%v\nwant=%v", got, want)
	}
}
