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
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"cel.dev/cel-go/common/containers"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// AttrReadTracker 记录一次求值过程中实际完成读取的顶层输入属性路径。
//
// 每个追踪器仅从属于单次求值：同一份 Program 被并发复用时，每次求值都会通过
// attrReadTrackingInterpretable 创建独立的追踪器，因此不同请求的读取记录不会相互混入。
//
// 路径从顶层输入变量开始，到实际完成读取的叶属性为止，例如 "request.user.name" 或
// "items[1].price"。动态键与列表下标按本次求值的真实取值展开；读取按发生顺序记录并保留
// 重复项。
type AttrReadTracker struct {
	// reads 是按读取发生顺序记录的完整路径。
	reads []string

	// sessions 是当前正在解析的属性路径构建栈，用于支持动态下标属性的嵌套解析。
	sessions []*readPathSession

	// attrPaths 记录每个根属性最近一次成功解析得到的完整路径，供推导循环迭代变量的
	// 具体元素路径等场景使用。映射本身为单次求值私有，不存在跨请求共享。
	attrPaths map[Attribute]string
}

// readPathSession 表示一条正在构建中的属性读取路径。
type readPathSession struct {
	// root 是路径的顶层变量名或循环元素的具体路径前缀。
	root string
	// rooted 标识该路径是否可以回溯到顶层输入变量；不可回溯的解析不会产生读取记录。
	rooted bool
	// segs 是根路径之后依次应用成功的限定片段，例如 ".name" 或 "[1]"。
	segs []string
}

func newAttrReadTracker() *AttrReadTracker {
	return &AttrReadTracker{
		attrPaths: make(map[Attribute]string),
	}
}

// Reads 返回本次求值实际读取的属性路径副本，按读取发生顺序排列并保留重复项。
func (t *AttrReadTracker) Reads() []string {
	if t == nil || len(t.reads) == 0 {
		return nil
	}
	out := make([]string, len(t.reads))
	copy(out, t.reads)
	return out
}

// begin 开始构建一条属性读取路径。
func (t *AttrReadTracker) begin(root string, rooted bool) *readPathSession {
	s := &readPathSession{root: root, rooted: rooted}
	t.sessions = append(t.sessions, s)
	return s
}

// appendSegment 向当前最内层的待完成路径追加一个已成功应用的限定片段。
func (t *AttrReadTracker) appendSegment(seg string) {
	if len(t.sessions) == 0 {
		return
	}
	s := t.sessions[len(t.sessions)-1]
	s.segs = append(s.segs, seg)
}

// commit 完成一条路径：从栈顶弹出会话，根路径可回溯到顶层输入时将完整路径记入读取列表，
// 并登记该根属性本次解析出的路径。
func (t *AttrReadTracker) commit(attr Attribute, s *readPathSession) {
	if len(t.sessions) == 0 || t.sessions[len(t.sessions)-1] != s {
		return
	}
	t.sessions = t.sessions[:len(t.sessions)-1]
	if !s.rooted {
		return
	}
	path := s.render()
	t.reads = append(t.reads, path)
	if attr != nil {
		t.attrPaths[attr] = path
	}
}

// discard 丢弃一条未能成功完成的路径，例如限定过程出错或解析结果为未知值。
func (t *AttrReadTracker) discard(s *readPathSession) {
	if len(t.sessions) == 0 || t.sessions[len(t.sessions)-1] != s {
		return
	}
	t.sessions = t.sessions[:len(t.sessions)-1]
}

// attributePath 查询某个可求值节点对应的属性最近一次成功解析的完整路径，循环范围表达式
// 用它把迭代变量绑定到具体的集合元素路径。
func (t *AttrReadTracker) attributePath(i InterpretableV2) (string, bool) {
	ia, ok := i.(InterpretableAttribute)
	if !ok {
		return "", false
	}
	attr := ia.Attr()
	for {
		if p, found := t.attrPaths[attr]; found {
			return p, true
		}
		// 部分求值工厂会再包一层 attributeMatcher，其内部嵌的才是发起路径构建的根属性。
		matcher, isMatcher := attr.(*attributeMatcher)
		if !isMatcher {
			return "", false
		}
		attr = matcher.NamespacedAttribute
	}
}

func (s *readPathSession) render() string {
	var b strings.Builder
	b.WriteString(s.root)
	for _, seg := range s.segs {
		b.WriteString(seg)
	}
	return b.String()
}

// attrReadTrackerFromActivation 从求值帧（必要时沿 Activation 层级向上查找）获取当前求值的
// 属性读取追踪器；未开启追踪时返回 nil，使默认求值路径不产生任何额外开销。
func attrReadTrackerFromActivation(vars Activation) *AttrReadTracker {
	if f, ok := vars.(*ExecutionFrame); ok {
		return f.AttrReadTracker()
	}
	if f := findFrame(vars); f != nil {
		return f.AttrReadTracker()
	}
	return nil
}

// qualifierReadSegment 按本次求值的真实取值渲染单个限定片段。
//
// 合法标识符形式的字符串片段渲染为点选形式（如 ".name"），其余字符串键使用加引号的下标
// 形式；整型等其它键类型统一使用下标形式（如 "[1]"、"[true]"）。
func qualifierReadSegment(val ref.Val) string {
	switch v := val.(type) {
	case types.String:
		s := string(v)
		if isIdentifierName(s) {
			return "." + s
		}
		return "[" + strconv.Quote(s) + "]"
	case types.Int:
		return "[" + strconv.FormatInt(int64(v), 10) + "]"
	case types.Uint:
		return "[" + strconv.FormatUint(uint64(v), 10) + "]"
	case types.Bool:
		if bool(v) {
			return "[true]"
		}
		return "[false]"
	case types.Double:
		return "[" + strconv.FormatFloat(float64(v), 'g', -1, 64) + "]"
	default:
		return fmt.Sprintf("[%v]", val)
	}
}

// nativeSegment 通过适配器把原生键值转换为 CEL 值后渲染限定片段。
func nativeReadSegment(adapter types.Adapter, val any) string {
	return qualifierReadSegment(adapter.NativeToValue(val))
}

func isIdentifierName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// trackConstantQualifier 在一个常量限定（字段选择、常量下标或 has 存在性检查）成功应用后，
// 把对应片段追加到当前读取路径。
func trackConstantQualifier(vars Activation, qual Qualifier) {
	tr := attrReadTrackerFromActivation(vars)
	if tr == nil {
		return
	}
	cq, ok := qual.(ConstantQualifier)
	if !ok {
		return
	}
	tr.appendSegment(qualifierReadSegment(cq.Value()))
}

// trackResolvedQualifier 在动态下标属性解析出本次真实键值并构造出限定器后，把展开后的片段
// 追加到外层读取路径。片段在真正执行限定前记录：随后的 has 存在性检查即使判定字段不存在也
// 视为成功完成的读取，而限定本身报错时外层路径会整体丢弃。
func trackResolvedQualifier(vars Activation, qual Qualifier) {
	tr := attrReadTrackerFromActivation(vars)
	if tr == nil {
		return
	}
	cq, ok := qual.(ConstantQualifier)
	if !ok {
		return
	}
	tr.appendSegment(qualifierReadSegment(cq.Value()))
}

// AttributeReadTracking 开启属性读取追踪的规划选项。
//
// 开启后规划出的可求值程序会在每次求值开始时把一个全新的 AttrReadTracker 放置到求值帧上，
// 属性解析过程据此记录读取路径；追踪器随求值帧回收，天然支持同一 Program 的并发复用。
func AttributeReadTracking() PlannerOption {
	return func(p *planner) (*planner, error) {
		p.attrReadTracking = true
		return p, nil
	}
}

// attrReadTrackingInterpretable 在每次求值前安装本次求值私有的读取追踪器。
type attrReadTrackingInterpretable struct {
	InterpretableV2
}

func (c *attrReadTrackingInterpretable) Exec(frame *ExecutionFrame) ref.Val {
	if frame.AttrReadTracker() == nil {
		frame.SetAttrReadTracker(newAttrReadTracker())
	}
	return c.InterpretableV2.Exec(frame)
}

func (c *attrReadTrackingInterpretable) Eval(ctx Activation) ref.Val {
	return EvalActivation(ctx, c.Exec)
}

// NewAttributeReadTrackingFactory 包装一个 AttributeFactory，使经由该工厂创建的根属性在
// 求值帧携带 AttrReadTracker 时自动记录自身的成功读取路径。
//
// 注意调用顺序：部分求值工厂必须包装在本工厂的外层（即 partial(tracked(base))），这样部分
// 求值的 attributeMatcher 在把解析委托给内部根属性时，路径构建逻辑才能生效；未知分支在委托
// 前直接返回，不会产生读取记录。
func NewAttributeReadTrackingFactory(
	inner AttributeFactory,
	container *containers.Container,
	adapter types.Adapter,
	provider types.Provider,
) AttributeFactory {
	return &attrReadTrackingFactory{
		AttributeFactory: inner,
		container:        container,
		adapter:          adapter,
		provider:         provider,
	}
}

type attrReadTrackingFactory struct {
	AttributeFactory
	container *containers.Container
	adapter   types.Adapter
	provider  types.Provider
}

// AbsoluteAttribute 包装内部工厂创建的命名空间根属性，使每次成功解析都构建并提交一条读取
// 路径。
func (f *attrReadTrackingFactory) AbsoluteAttribute(id int64, names ...string) NamespacedAttribute {
	// 与内部工厂保持一致，去掉用于命名空间消歧的 "." 前缀，路径只使用真实变量名。
	cleanNames := append([]string(nil), names...)
	for idx, name := range cleanNames {
		cleanNames[idx] = strings.TrimPrefix(name, ".")
	}
	return &trackedRootAttribute{
		NamespacedAttribute: f.AttributeFactory.AbsoluteAttribute(id, names...),
		names:               cleanNames,
		fac:                 f,
	}
}

// MaybeAttribute 重建 maybe 属性，保证其各候选变量及限定增补属性同样经由追踪工厂创建；
// 未类型检查表达式的命名空间解析才能完整记录读取路径。
func (f *attrReadTrackingFactory) MaybeAttribute(id int64, name string) Attribute {
	var names []string
	if strings.HasPrefix(name, ".") {
		names = append(names, name)
	} else {
		names = f.container.ResolveCandidateNames(name)
	}
	return &maybeAttribute{
		id: id,
		attrs: []NamespacedAttribute{
			f.AbsoluteAttribute(id, names...),
		},
		adapter:  f.adapter,
		provider: f.provider,
		fac:      f,
	}
}

// trackedRootAttribute 包装一个命名空间根属性（identAttribute / absoluteAttribute，或部分
// 求值工厂的 attributeMatcher），围绕 Resolve 维护读取路径的构建会话。
type trackedRootAttribute struct {
	NamespacedAttribute
	names []string
	fac   AttributeFactory
}

// AddQualifier 保持包装器自身作为属性根，避免 evalAttr 在追加限定后被内部属性替换而丢失
// 追踪能力。
func (a *trackedRootAttribute) AddQualifier(qual Qualifier) (Attribute, error) {
	if _, err := a.NamespacedAttribute.AddQualifier(qual); err != nil {
		return nil, err
	}
	return a, nil
}

// Qualify / QualifyIfPresent 在本属性作为其它对象的动态限定（如下标属性 m[k] 中的 k）时
// 被调用。内部属性自带的 Qualify 方法会把未包装的内部属性传给 attrQualify，从而绕过读取
// 追踪；这里显式以包装器自身参与限定，使作为下标的属性解析（Resolve）仍经过路径记录。
func (a *trackedRootAttribute) Qualify(vars Activation, obj any) (any, error) {
	return attrQualify(a.fac, vars, obj, a)
}

// QualifyIfPresent 同 Qualify，用于存在性限定路径。
func (a *trackedRootAttribute) QualifyIfPresent(vars Activation, obj any, presenceOnly bool) (any, bool, error) {
	return attrQualifyIfPresent(a.fac, vars, obj, a, presenceOnly)
}

// Resolve 在内部解析前后管理读取路径会话：
//   - 解析开始前确定根路径（顶层变量名，或循环/绑定局部变量对应的具体元素路径）；
//   - 解析失败或得到未知值时丢弃整条路径，不把未成功解析的路径记为已读；
//   - 解析成功时提交从根到叶属性的完整路径。
func (a *trackedRootAttribute) Resolve(vars Activation) (any, error) {
	tr := attrReadTrackerFromActivation(vars)
	if tr == nil {
		return a.NamespacedAttribute.Resolve(vars)
	}
	root, rooted := a.resolveRoot(vars)
	session := tr.begin(root, rooted)
	out, err := a.NamespacedAttribute.Resolve(vars)
	if err != nil {
		tr.discard(session)
		return nil, err
	}
	if _, isUnknown := out.(*types.Unknown); isUnknown {
		tr.discard(session)
		return out, nil
	}
	tr.commit(a, session)
	return out, nil
}

// resolveRoot 确定该根属性本次解析应使用的路径起点：
//   - 循环迭代变量、cel.bind 绑定变量等局部变量映射为其当前绑定值在输入中的具体路径；
//   - 其余按命名空间解析顺序确定实际命中的顶层输入变量名；
//   - 命中的是类型标识符或无法回溯到顶层输入的值时，返回 rooted=false，不产生记录。
//
// 对惰性绑定的提前解析不会造成重复调用：mapActivation 与求值帧都会缓存惰性变量的首次
// 解析结果。
func (a *trackedRootAttribute) resolveRoot(vars Activation) (string, bool) {
	if frame, ok := vars.(*ExecutionFrame); ok {
		for _, nm := range a.names {
			if frame.IsLocalVariable(nm) {
				if prefix, rooted, handled := frame.attrReadPrefix(nm); handled {
					return prefix, rooted
				}
				return "", false
			}
		}
	}
	for _, nm := range a.names {
		if _, found := vars.ResolveName(nm); found {
			return nm, true
		}
	}
	return "", false
}
