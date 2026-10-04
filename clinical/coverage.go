package clinical

// scopeKey 是整类覆盖的定位键：某次就诊 + 某个记录类别（诊断/医嘱）。
type scopeKey struct {
	encounterID ID
	category    string
}

// grantCoverage 是授权范围被归一化后的“覆盖含义”，是接收方查阅与内部使用者
// 创建交换共用的唯一判定来源：两处对同一条授权“覆盖什么”的理解必须一致，
// 不能各自维护一套判断。
//
// 覆盖由两个互不串用的维度组成：
//
//   - whole：整类范围。某个“就诊+类别”一旦在其中，该范围下的全部已生效记录
//     都被覆盖，包括授权建立之后才生效的记录。
//   - selected：限定范围。只覆盖明确选中的记录标识；键记录该选择声明的
//     “就诊+类别”，用于把限定覆盖严格约束在所声明的就诊与类别内。被选记录
//     更正后仍按记录标识覆盖它的当前版本；同类其他记录以及后来新增的记录
//     不会因此获得授权。
//
// 可由多条授权合并而成（接收方查阅合并该患者、该接收方的所有当前有效授权），
// 也可只装一条绑定授权（创建交换时不能借用其他授权补足）。
type grantCoverage struct {
	whole          map[scopeKey]bool
	selected       map[ID]scopeKey
	selectedScopes map[scopeKey]bool
}

func newGrantCoverage() *grantCoverage {
	return &grantCoverage{
		whole:          map[scopeKey]bool{},
		selected:       map[ID]scopeKey{},
		selectedScopes: map[scopeKey]bool{},
	}
}

// add 把一条授权的整类范围与限定范围并入当前覆盖。调用方负责只并入满足身份
// （同一患者、同一接收方）且在核对时刻有效的授权；这里只解释范围本身。
func (c *grantCoverage) add(a *Authorization) {
	for _, sc := range a.Scopes {
		c.whole[scopeKey{encounterID: sc.EncounterID, category: sc.Category}] = true
	}
	for _, sel := range a.Selections {
		k := scopeKey{encounterID: sel.EncounterID, category: sel.Category}
		// 重复选择同一记录只算一次；保留其声明的就诊+类别。
		c.selected[sel.RecordID] = k
		c.selectedScopes[k] = true
	}
}

// coversScope 报告整类范围是否覆盖某次就诊下某个类别。
func (c *grantCoverage) coversScope(encounterID ID, category string) bool {
	return c.whole[scopeKey{encounterID: encounterID, category: category}]
}

// coversRequest 报告覆盖是否触及所请求的“就诊+类别”：存在覆盖它的整类范围，
// 或至少有一条限定选择明确声明属于该就诊与类别。用于区分“有授权但当前没有
// 生效记录（返回空集合）”与“没有任何授权覆盖（拒绝）”。
func (c *grantCoverage) coversRequest(encounterID ID, category string) bool {
	k := scopeKey{encounterID: encounterID, category: category}
	return c.whole[k] || c.selectedScopes[k]
}

// coversRecord 报告某条具体记录是否被覆盖：整类范围按记录实际的就诊+类别
// 覆盖；限定范围只覆盖明确选中的记录标识，且该选择声明的就诊+类别必须与
// 记录实际所在一致（授权建立时已校验，此处再保证覆盖关系不会串到其他就诊
// 或类别）。草稿或无生效版本的记录由调用方另行排除。
func (c *grantCoverage) coversRecord(r *Record) bool {
	k := scopeKey{encounterID: r.EncounterID, category: r.Category}
	if c.whole[k] {
		return true
	}
	declared, ok := c.selected[r.ID]
	return ok && declared == k
}
