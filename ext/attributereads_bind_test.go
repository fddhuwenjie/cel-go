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

package ext

import (
	"reflect"
	"testing"

	"cel.dev/cel-go/cel"
)

// TestAttributeReadsWithBind 验证 cel.bind 绑定到顶层属性时，绑定变量上的字段读取能够回溯
// 到真实的顶层输入路径。
func TestAttributeReadsWithBind(t *testing.T) {
	env, err := cel.NewEnv(
		Bindings(),
		cel.Variable("user", cel.DynType),
	)
	if err != nil {
		t.Fatalf("cel.NewEnv() failed: %v", err)
	}
	ast, iss := env.Compile(`cel.bind(u, user, u.profile.name == "huevos")`)
	if iss != nil && iss.Err() != nil {
		t.Fatalf("Compile() failed: %v", iss.Err())
	}
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptTrackAttributeReads))
	if err != nil {
		t.Fatalf("Program() failed: %v", err)
	}
	_, det, err := prg.Eval(map[string]any{
		"user": map[string]any{
			"profile": map[string]any{"name": "huevos"},
		},
	})
	if err != nil {
		t.Fatalf("Eval() failed: %v", err)
	}
	got := det.AttributeReads()
	// 绑定初始化时读取一次顶层变量 user，绑定变量上的字段读取则展开为具体输入路径。
	want := []string{"user", "user.profile.name"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AttributeReads() = %v, want %v", got, want)
	}
}
