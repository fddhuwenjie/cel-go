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

package interpreter

import (
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

func TestQualifierReadSegment(t *testing.T) {
	cases := []struct {
		val  ref.Val
		want string
	}{
		{types.String("name"), ".name"},
		{types.String("a b"), `["a b"]`},
		{types.String("with-dash"), `["with-dash"]`},
		{types.Int(42), "[42]"},
		{types.Int(-1), "[-1]"},
		{types.Uint(7), "[7]"},
		{types.Bool(true), "[true]"},
		{types.Bool(false), "[false]"},
		{types.Double(1.5), "[1.5]"},
	}
	for _, tc := range cases {
		if got := qualifierReadSegment(tc.val); got != tc.want {
			t.Errorf("qualifierReadSegment(%v) = %q, want %q", tc.val, got, tc.want)
		}
	}
}

func TestAttrReadTrackerOrderingAndDuplicates(t *testing.T) {
	tr := newAttrReadTracker()

	s1 := tr.begin("a", true)
	tr.appendSegment(".b")
	tr.commit(nil, s1)

	s2 := tr.begin("a", true)
	tr.appendSegment(".b")
	tr.commit(nil, s2)

	s3 := tr.begin("items", true)
	tr.appendSegment("[2]")
	tr.appendSegment(".price")
	tr.commit(nil, s3)

	want := []string{"a.b", "a.b", "items[2].price"}
	got := tr.Reads()
	if len(got) != len(want) {
		t.Fatalf("Reads() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Reads() = %v, want %v", got, want)
		}
	}
	// 返回副本：修改不影响追踪器内部状态。
	got[0] = "mutated"
	if tr.Reads()[0] != "a.b" {
		t.Fatalf("Reads() exposed mutable internal state")
	}
}

func TestAttrReadTrackerDiscardUnrootedAndFailed(t *testing.T) {
	tr := newAttrReadTracker()

	// 无法回溯到顶层输入的路径不记录。
	unrooted := tr.begin("", false)
	tr.appendSegment(".x")
	tr.commit(nil, unrooted)

	// 解析失败的路径丢弃。
	failed := tr.begin("c", true)
	tr.appendSegment(".missing")
	tr.discard(failed)

	if reads := tr.Reads(); len(reads) != 0 {
		t.Fatalf("expected no reads, got %v", reads)
	}

	// 嵌套会话：内层成功记录、外层失败丢弃时，内层已完成的读取保留。
	outer := tr.begin("m", true)
	inner := tr.begin("k", true)
	tr.commit(nil, inner)
	tr.discard(outer)
	if reads := tr.Reads(); len(reads) != 1 || reads[0] != "k" {
		t.Fatalf("expected only nested read [k], got %v", reads)
	}
}

func TestAttrReadTrackerNilSafe(t *testing.T) {
	var tr *AttrReadTracker
	if reads := tr.Reads(); reads != nil {
		t.Fatalf("nil tracker Reads() = %v, want nil", reads)
	}
}
