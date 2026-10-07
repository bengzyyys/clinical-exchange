package clinical

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

// ---- GrantSelective 入参与正式授权互不串改的回归夹具 ----
//
// 同一名未停用合成患者名下三次已登记就诊，资料刻意把三种覆盖状态分开，
// 避免“整类范围恰好覆盖全部候选记录”掩盖限定选择被改动：
//
//   - eW（整类就诊）：诊断 wholeDx1、wholeDx2 均由整类范围覆盖；
//   - eL（混合就诊）：诊断 selA、selB 只由限定范围覆盖，同类的另一条
//     已生效诊断 notSel 从未获任何授权（用于本地把限定选择换过去）；
//     医嘱 eLOrder 由另一条整类范围覆盖，证明同就诊不同类别互不牵连；
//   - eX（本地改投就诊）：同患者的第三次就诊，诊断 xDiag 从未获授权，
//     仅作为调用方在本地把整类范围“改到另一就诊”的目标。
type grantInputIsolationFixture struct {
	s   *Store
	clk *fakeClock

	pid ID
	eW  ID
	eL  ID
	eX  ID

	wholeDx1 ID
	wholeDx2 ID
	selA     ID
	selB     ID
	notSel   ID
	eLOrder  ID
	xDiag    ID

	start time.Time
	end   time.Time
}

func setupGrantInputIsolationFixture(t *testing.T) grantInputIsolationFixture {
	t.Helper()
	s, clk := newTestStore(t)

	p, err := s.RegisterPatient(doc, "授权入参隔离患者")
	if err != nil {
		t.Fatalf("register patient: %v", err)
	}
	addEncounter := func(when time.Duration) ID {
		t.Helper()
		e, err := s.AddEncounter(doc, p.ID, clk.t.Add(when))
		if err != nil {
			t.Fatalf("add encounter: %v", err)
		}
		return e.ID
	}
	eff := func(eid ID, category, content string) ID {
		t.Helper()
		r, err := s.CreateDraft(doc, p.ID, eid, category, content)
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if _, err := s.ActivateRecord(doc, r.ID); err != nil {
			t.Fatalf("activate: %v", err)
		}
		return r.ID
	}

	eW := addEncounter(0)
	eL := addEncounter(time.Hour)
	eX := addEncounter(2 * time.Hour)

	wholeDx1 := eff(eW, Diagnosis, "整类诊断一")
	wholeDx2 := eff(eW, Diagnosis, "整类诊断二")
	selA := eff(eL, Diagnosis, "限定诊断甲")
	selB := eff(eL, Diagnosis, "限定诊断乙")
	notSel := eff(eL, Diagnosis, "未选中诊断")
	eLOrder := eff(eL, Order, "混合就诊医嘱")
	xDiag := eff(eX, Diagnosis, "另一就诊诊断")

	return grantInputIsolationFixture{
		s: s, clk: clk,
		pid: p.ID, eW: eW, eL: eL, eX: eX,
		wholeDx1: wholeDx1, wholeDx2: wholeDx2,
		selA: selA, selB: selB, notSel: notSel,
		eLOrder: eLOrder, xDiag: xDiag,
		start: clk.t.Add(-time.Hour), end: clk.t.Add(24 * time.Hour),
	}
}

// scopeInOrder 与 GrantSelective 保存时的整类范围排序规则一致：
// 先按就诊标识、再按类别标识。测试用它预先算出返回顺序，再刻意逆序提交。
func scopeInOrder(a, b Scope) bool {
	if a.EncounterID != b.EncounterID {
		return a.EncounterID < b.EncounterID
	}
	return a.Category < b.Category
}

// ---- 成功路径：整理授权不得改写调用方输入；本地复用不得回串正式授权 ----
//
// 两份入参都故意保留重复条目，并按“保存排序结果的逆序”排列，使输入与返回
// 在顺序、重复次数上必然不同（不依赖随机标识的偶然排列）。

func TestGrantSelectiveKeepsCallerListsAndLocalReuseStaysLocal(t *testing.T) {
	f := setupGrantInputIsolationFixture(t)

	// 唯一整类条目按保存规则排序后逆序，并在末尾夹带一个重复项。
	uniqScopes := []Scope{
		{EncounterID: f.eW, Category: Diagnosis},
		{EncounterID: f.eL, Category: Order},
	}
	sort.Slice(uniqScopes, func(i, j int) bool { return scopeInOrder(uniqScopes[i], uniqScopes[j]) })
	scopesIn := []Scope{uniqScopes[1], uniqScopes[0], uniqScopes[0]}

	// 唯一限定条目按记录标识升序（保存顺序）排序后逆序，末尾同样夹带重复项。
	uniqSelections := []RecordSelection{
		sel(f.eL, Diagnosis, f.selA),
		sel(f.eL, Diagnosis, f.selB),
	}
	sort.Slice(uniqSelections, func(i, j int) bool {
		return uniqSelections[i].RecordID < uniqSelections[j].RecordID
	})
	selectionsIn := []RecordSelection{uniqSelections[1], uniqSelections[0], uniqSelections[1]}

	// 先留下调用方原始列表的独立快照：整理后必须与它逐项一致。
	wantScopesIn := append([]Scope(nil), scopesIn...)
	wantSelectionsIn := append([]RecordSelection(nil), selectionsIn...)

	auditBefore := len(mustAudit(t, f.s, f.pid))
	totalBefore := totalAuthorizations(f.s)

	a, err := f.s.GrantSelective(doc, f.pid, rcv.ID, scopesIn, selectionsIn, f.start, f.end)
	if err != nil {
		t.Fatalf("grant selective: %v", err)
	}

	// 返回的授权按现有规则去重：两份范围都只剩两个唯一条目，无重复覆盖。
	if len(a.Scopes) != 2 {
		t.Fatalf("returned scopes = %d, want 2 unique: %+v", len(a.Scopes), a.Scopes)
	}
	if len(a.Selections) != 2 {
		t.Fatalf("returned selections = %d, want 2 unique: %+v", len(a.Selections), a.Selections)
	}
	// 返回的授权按现有规则排序。
	for i := 1; i < len(a.Scopes); i++ {
		if scopeInOrder(a.Scopes[i], a.Scopes[i-1]) {
			t.Fatalf("returned scopes not in saved order: %+v", a.Scopes)
		}
	}
	for i := 1; i < len(a.Selections); i++ {
		if a.Selections[i-1].RecordID > a.Selections[i].RecordID {
			t.Fatalf("returned selections not sorted by record id: %+v", a.Selections)
		}
	}
	// 去重排序只是巧合相同的防线：返回内容必须恰好是两份唯一条目集合。
	gotScopes := map[Scope]int{}
	for _, sc := range a.Scopes {
		gotScopes[sc]++
	}
	for _, want := range uniqScopes {
		if gotScopes[want] != 1 {
			t.Fatalf("saved scope %+v missing or duplicated: %+v", want, a.Scopes)
		}
	}
	gotSels := map[RecordSelection]int{}
	for _, sc := range a.Selections {
		gotSels[sc]++
	}
	for _, want := range uniqSelections {
		if gotSels[want] != 1 {
			t.Fatalf("saved selection %+v missing or duplicated: %+v", want, a.Selections)
		}
	}

	// 调用方提交的两份列表保持原有顺序、重复次数和每项内容——
	// 不能为了整理授权而替调用方改写输入。
	if !reflect.DeepEqual(scopesIn, wantScopesIn) {
		t.Fatalf("caller scope list rewritten by grant:\ngot  %+v\nwant %+v", scopesIn, wantScopesIn)
	}
	if !reflect.DeepEqual(selectionsIn, wantSelectionsIn) {
		t.Fatalf("caller selection list rewritten by grant:\ngot  %+v\nwant %+v", selectionsIn, wantSelectionsIn)
	}
	// 输入排列确实与返回不同：否则“保留输入”与“整理授权”之间的区别无从谈起。
	if reflect.DeepEqual(scopesIn, a.Scopes) || reflect.DeepEqual(selectionsIn, a.Selections) {
		t.Fatal("input lists must differ from deduped/sorted saved lists")
	}

	// 保存的正式授权与当次返回逐字段一致（范围各自是独立切片）。
	stored, err := f.s.GetAuthorization(doc, f.pid, a.ID)
	if err != nil {
		t.Fatalf("get authorization: %v", err)
	}
	if !reflect.DeepEqual(stored, a) {
		t.Fatalf("stored authorization diverges from returned:\nstored = %+v\nret    = %+v", stored, a)
	}
	listed, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0], a) {
		t.Fatalf("listed authorization diverges: %+v %v", listed, err)
	}
	// 成功建立恰好新增一条授权与一条授权审计，没有其他副作用。
	if got := totalAuthorizations(f.s); got != totalBefore+1 {
		t.Fatalf("authorization count = %d, want %d", got, totalBefore+1)
	}
	if events := auditActions(mustAudit(t, f.s, f.pid)); events[ActionGranted] != 1 {
		t.Fatalf("granted audit count = %d, want 1 (%+v)", events[ActionGranted], events)
	}
	returnedAtFirst := a

	// ---- 授权保存后，调用方继续复用原列表准备下一次共享 ----
	//
	// 把一条整类范围改到同患者的另一就诊 eX；把一条限定选择换成同类的
	// 另一条已生效记录 notSel（就诊与类别不变）。
	for i := range scopesIn {
		if scopesIn[i] == (Scope{EncounterID: f.eW, Category: Diagnosis}) {
			scopesIn[i].EncounterID = f.eX
		}
	}
	selectionsIn[0].RecordID = f.notSel

	// 先前返回的授权、内部使用者重新查看到的正式范围都不被本地修改改动。
	if !reflect.DeepEqual(a, returnedAtFirst) {
		t.Fatalf("returned authorization changed through local list reuse:\ngot  %+v\nwant %+v",
			a, returnedAtFirst)
	}
	stored, err = f.s.GetAuthorization(doc, f.pid, returnedAtFirst.ID)
	if err != nil {
		t.Fatalf("re-get authorization: %v", err)
	}
	if !reflect.DeepEqual(stored, returnedAtFirst) {
		t.Fatalf("stored authorization changed through local list reuse:\ngot  %+v\nwant %+v",
			stored, returnedAtFirst)
	}
	if rows, err := f.s.ListAuthorizations(doc, f.pid, ""); err != nil ||
		len(rows) != 1 || !reflect.DeepEqual(rows[0], returnedAtFirst) {
		t.Fatalf("relisted authorization reflects local edits: %+v %v", rows, err)
	}

	// 接收方仍按首次成功提交的范围读取：
	// 整类范围内原本允许的记录仍可读（eW 诊断两条都在）。
	res, err := f.s.Read(rcv, f.pid, f.eW, Diagnosis)
	if err != nil {
		t.Fatalf("read whole-category encounter: %v", err)
	}
	wholeIDs := readRecordIDs(res)
	if len(wholeIDs) != 2 || !wholeIDs[f.wholeDx1] || !wholeIDs[f.wholeDx2] {
		t.Fatalf("whole-category read = %v, want both originally covered records", wholeIDs)
	}
	// 限定范围内原选记录仍可读；被本地换入、未获其他授权的同类记录不能出现。
	res, err = f.s.Read(rcv, f.pid, f.eL, Diagnosis)
	if err != nil {
		t.Fatalf("read limited encounter/category: %v", err)
	}
	limitedIDs := readRecordIDs(res)
	if len(limitedIDs) != 2 || !limitedIDs[f.selA] || !limitedIDs[f.selB] {
		t.Fatalf("limited read = %v, want both originally selected records", limitedIDs)
	}
	if limitedIDs[f.notSel] {
		t.Fatal("locally swapped-in record notSel must not appear without another grant")
	}
	// 同就诊的整类医嘱不受诊断侧本地换记录影响。
	res, err = f.s.Read(rcv, f.pid, f.eL, Order)
	if err != nil {
		t.Fatalf("read whole-category order: %v", err)
	}
	if ids := readRecordIDs(res); len(ids) != 1 || !ids[f.eLOrder] {
		t.Fatalf("whole-category order read = %v, want eLOrder only", ids)
	}
	// 本地把整类范围改投到 eX 不产生任何覆盖：拒绝且不带受保护内容。
	denied, err := f.s.Read(rcv, f.pid, f.eX, Diagnosis)
	assertDeniedWithoutContent(t, denied, err, "locally re-targeted whole scope encounter")

	// 本地复用不新增授权或审计。
	if got := totalAuthorizations(f.s); got != totalBefore+1 {
		t.Fatalf("authorization count changed through local reuse: %d", got)
	}
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore+1 {
		t.Fatalf("local list reuse added audit events: before=%d after=%d",
			auditBefore, got)
	}
}

// ---- 失败边界：已生效记录被声明为同患者另一次就诊下的记录 ----
//
// 同一批范围里混入这条不合法选择时返回 ErrInvalidArgument，整条授权不成立：
// 两份输入列表（含重复条目）保持原样，不保存合法部分，不新增授权创建审计，
// 已有授权与接收方可读内容不变。

func TestGrantSelectiveWrongDeclaredEncounterRejectsAllAndKeepsInputs(t *testing.T) {
	f := setupGrantInputIsolationFixture(t)

	// 合法整类条目保留重复；合法限定条目也保留重复，再混入一条不合法选择：
	// selB 实际属于 eL/诊断，却被声明在同患者另一次已登记就诊 eX 下。
	scopesIn := []Scope{
		{EncounterID: f.eW, Category: Diagnosis},
		{EncounterID: f.eW, Category: Diagnosis},
	}
	selectionsIn := []RecordSelection{
		sel(f.eL, Diagnosis, f.selA),
		sel(f.eL, Diagnosis, f.selA), // 重复的合法选择
		sel(f.eX, Diagnosis, f.selB), // 就诊声明不符：实际在 eL
	}
	wantScopesIn := append([]Scope(nil), scopesIn...)
	wantSelectionsIn := append([]RecordSelection(nil), selectionsIn...)

	auditBefore := len(mustAudit(t, f.s, f.pid))
	totalBefore := totalAuthorizations(f.s)

	zero, err := f.s.GrantSelective(doc, f.pid, rcv.ID, scopesIn, selectionsIn, f.start, f.end)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("wrong declared encounter err = %v, want ErrInvalidArgument", err)
	}
	if !reflect.DeepEqual(zero, Authorization{}) {
		t.Fatalf("failed grant returned a non-zero authorization: %+v", zero)
	}

	// 失败时两份输入列表也保持原样：顺序、重复次数、每项内容都不变。
	if !reflect.DeepEqual(scopesIn, wantScopesIn) {
		t.Fatalf("scope list mutated by failed grant:\ngot  %+v\nwant %+v", scopesIn, wantScopesIn)
	}
	if !reflect.DeepEqual(selectionsIn, wantSelectionsIn) {
		t.Fatalf("selection list mutated by failed grant:\ngot  %+v\nwant %+v", selectionsIn, wantSelectionsIn)
	}

	// 不保存其中合法部分：没有任何授权落盘。
	if rows, err := f.s.ListAuthorizations(doc, f.pid, ""); err != nil || len(rows) != 0 {
		t.Fatalf("rejected grant persisted legal parts: rows=%+v err=%v", rows, err)
	}
	if got := totalAuthorizations(f.s); got != totalBefore {
		t.Fatalf("rejected grant changed authorization count: %d -> %d", totalBefore, got)
	}
	// 不新增授权创建审计。
	if events := auditActions(mustAudit(t, f.s, f.pid)); events[ActionGranted] != 0 {
		t.Fatalf("rejected grant added granted audit: %+v", events)
	}
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("rejected grant changed audit count: %d -> %d", auditBefore, got)
	}

	// 接收方可读内容不变：合法的整类与限定部分都没有落盘，三处一律拒绝，
	// 拒绝结果不带受保护内容。
	denied, err := f.s.Read(rcv, f.pid, f.eW, Diagnosis)
	assertDeniedWithoutContent(t, denied, err, "whole scope from rejected batch")
	denied, err = f.s.Read(rcv, f.pid, f.eL, Diagnosis)
	assertDeniedWithoutContent(t, denied, err, "selection from rejected batch")
	denied, err = f.s.Read(rcv, f.pid, f.eX, Diagnosis)
	assertDeniedWithoutContent(t, denied, err, "mismatched encounter from rejected batch")
}

// ---- Grant 的整类授权用法保留：只有整类范围，读取仍按整类放行 ----

func TestGrantWholeCategoryUsageRemainsWholeCategory(t *testing.T) {
	f := setupGrantInputIsolationFixture(t)

	a, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.eW, Category: Diagnosis}}, f.start, f.end)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if len(a.Scopes) != 1 || a.Scopes[0] != (Scope{EncounterID: f.eW, Category: Diagnosis}) {
		t.Fatalf("whole-category grant scopes altered: %+v", a.Scopes)
	}
	if len(a.Selections) != 0 {
		t.Fatalf("Grant must not carry selections: %+v", a.Selections)
	}

	// 整类就诊+类别下全部已生效记录可读。
	res, err := f.s.Read(rcv, f.pid, f.eW, Diagnosis)
	if err != nil {
		t.Fatalf("read granted category: %v", err)
	}
	ids := readRecordIDs(res)
	if len(ids) != 2 || !ids[f.wholeDx1] || !ids[f.wholeDx2] {
		t.Fatalf("whole-category read = %v, want both diagnoses", ids)
	}
	// 未授权的其他就诊/类别仍被拒绝。
	denied, err := f.s.Read(rcv, f.pid, f.eL, Diagnosis)
	assertDeniedWithoutContent(t, denied, err, "ungranted encounter/category")
}
