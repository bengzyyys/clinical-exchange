package clinical

// coverage 是“一条或多条授权覆盖什么”的唯一解释入口。接收方查阅
// （[Store.Read]，合并该患者+该接收方的多条当前有效授权）与内部使用者
// 创建交换（[Store.CreateExchange]，只认绑定的那一条有效授权）都通过它
// 判断覆盖，使同一条授权的整类范围与选定记录范围在两个功能中的含义完全
// 一致，不再各自维护一份判定逻辑。
//
// 覆盖语义：
//   - 整类范围按“就诊 + 类别”覆盖：覆盖指定就诊、指定类别下的全部已生效
//     记录，包括授权建立之后才生效的记录；覆盖关系严格限定在声明的就诊与
//     类别内，不会串到其他患者、就诊或类别。
//   - 限定范围按记录标识覆盖：只有明确选中的记录被覆盖。被选记录被更正后
//     仍覆盖它的当前版本（记录标识不变）；同一就诊同一类别的其他记录、
//     后来新增并生效的记录都不会因此获得授权。
type coverage struct {
	// whole 为被整类范围覆盖的“就诊 + 类别”集合。
	whole map[Scope]struct{}
	// selected 为限定范围明确选中的记录标识 -> 建立授权时已校验的
	// 该记录所属就诊+类别（记录的就诊与类别此后不可变）。
	selected map[ID]Scope
}

func newCoverage() coverage {
	return coverage{
		whole:    map[Scope]struct{}{},
		selected: map[ID]Scope{},
	}
}

// coverageOf 提取单条授权的覆盖含义。授权的有效性（时间窗、撤回）由调用方
// 在调用前按各自的核对时刻判断，这里只解释范围本身。
func coverageOf(a *Authorization) coverage {
	c := newCoverage()
	for _, sc := range a.Scopes {
		c.whole[sc] = struct{}{}
	}
	for _, sel := range a.Selections {
		c.selected[sel.RecordID] = Scope{EncounterID: sel.EncounterID, Category: sel.Category}
	}
	return c
}

// merge 把另一份覆盖并入自身：接收方查阅时合并多条当前有效授权，
// 重叠的整类范围与选定记录只算一次。
func (c coverage) merge(other coverage) {
	for sc := range other.whole {
		c.whole[sc] = struct{}{}
	}
	for rid, sc := range other.selected {
		c.selected[rid] = sc
	}
}

// coversCategory 报告整类范围是否覆盖某次就诊下的某个类别。
func (c coverage) coversCategory(encounterID ID, category string) bool {
	_, ok := c.whole[Scope{EncounterID: encounterID, Category: category}]
	return ok
}

// coversRecord 报告某条具体记录是否被覆盖：记录的实际就诊+类别落在整类
// 范围内，或记录标识被限定范围明确选中。整类与限定重叠时结论不变。
func (c coverage) coversRecord(r *Record) bool {
	if c.coversCategory(r.EncounterID, r.Category) {
		return true
	}
	_, ok := c.selected[r.ID]
	return ok
}

// hasSelectedIn 报告限定范围是否在指定就诊+类别中明确选中过记录。
// 用于区分两种看似相近、结论却不同的情形：
//   - 整类范围覆盖某就诊+类别、但其中暂时没有生效记录：查阅成功返回空集合；
//   - 没有任何整类范围或选定记录覆盖请求范围：必须返回 ErrAccessDenied，
//     不能把拒绝变成成功的空结果，也不能借其他就诊/类别的选择冒充覆盖。
func (c coverage) hasSelectedIn(encounterID ID, category string) bool {
	for _, sc := range c.selected {
		if sc.EncounterID == encounterID && sc.Category == category {
			return true
		}
	}
	return false
}
