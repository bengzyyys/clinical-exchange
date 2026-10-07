package clinical

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

// ---- 授权创建入参列表归属调用方的回归夹具 ----
//
// 一名未停用的合成患者、两次就诊，记录按覆盖关系分三类，互不掩盖：
//   - 整类覆盖：e1 下医嘱 orA、orB 与 e2 下医嘱 orE2，由两条整类范围覆盖；
//   - 限定覆盖：e1 下诊断 dxSel、dxSel2，由限定记录范围明确选出；
//   - 未覆盖：e1 下诊断 dxFree，已生效但从未被选中——整类范围只落在医嘱
//     类别上，诊断类别不存在整类授权，dxFree 一旦出现即说明限定范围被
//     本地改动外溢，不会被整类范围恰好覆盖全部候选记录而掩盖。
type grantInputFixture struct {
	s   *Store
	clk *fakeClock

	pid    ID
	e1, e2 ID

	dxSel  ID // e1 下已生效诊断：被限定选择
	dxSel2 ID // e1 下已生效诊断：同被限定选择
	dxFree ID // e1 下已生效诊断：从未被任何授权选中
	orA    ID // e1 下已生效医嘱：整类范围覆盖
	orB    ID // e1 下已生效医嘱：整类范围覆盖
	orE2   ID // e2 下已生效医嘱：第二条整类范围覆盖

	st, en time.Time
}

func setupGrantInputFixture(t *testing.T) grantInputFixture {
	t.Helper()
	s, clk := newTestStore(t)
	pid, e1 := setupPatientEncounter(t, s)
	e2enc, err := s.AddEncounter(doc, pid, clk.t.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("add second encounter: %v", err)
	}
	eff := func(eid ID, category, content string) ID {
		t.Helper()
		r, err := s.CreateDraft(doc, pid, eid, category, content)
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if _, err := s.ActivateRecord(doc, r.ID); err != nil {
			t.Fatalf("activate: %v", err)
		}
		return r.ID
	}
	return grantInputFixture{
		s: s, clk: clk, pid: pid, e1: e1, e2: e2enc.ID,
		dxSel:  eff(e1, Diagnosis, "诊断-选中"),
		dxSel2: eff(e1, Diagnosis, "诊断-选中二"),
		dxFree: eff(e1, Diagnosis, "诊断-未选中"),
		orA:    eff(e1, Order, "医嘱一"),
		orB:    eff(e1, Order, "医嘱二"),
		orE2:   eff(e2enc.ID, Order, "二诊医嘱"),
		st:     clk.t.Add(-time.Hour),
		en:     clk.t.Add(24 * time.Hour),
	}
}

// wantScopeOrder 是与库内保存规则一致的整类范围排序结果。
func wantScopeOrder(scopes ...Scope) []Scope {
	out := append([]Scope(nil), scopes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].EncounterID != out[j].EncounterID {
			return out[i].EncounterID < out[j].EncounterID
		}
		return out[i].Category < out[j].Category
	})
	return out
}

// wantSelectionOrder 是与库内保存规则一致的限定范围排序结果（按记录标识）。
func wantSelectionOrder(sels ...RecordSelection) []RecordSelection {
	out := append([]RecordSelection(nil), sels...)
	sort.Slice(out, func(i, j int) bool { return out[i].RecordID < out[j].RecordID })
	return out
}

// 成功路径：提交的整类/限定列表保留重复条目、排列与授权返回顺序不同。
// 创建成功后返回与保存的授权按既有规则去重、排序，但调用方提交的列表
// 保持原有顺序、重复次数与每项内容；随后调用方在本地复用并改写这些
// 列表（整类范围改到另一就诊、限定选择换成同类另一条已生效记录），
// 不改变先前返回的授权、内部使用者查看到的正式范围与接收方可读内容。

func TestGrantSelectiveCallerListsUnchangedOnSuccess(t *testing.T) {
	f := setupGrantInputFixture(t)

	// 先确定保存时的规范顺序，再按相反顺序并夹带重复条目构造输入，
	// 保证输入排列与授权返回顺序不同。
	wantScopes := wantScopeOrder(
		Scope{EncounterID: f.e1, Category: Order},
		Scope{EncounterID: f.e2, Category: Order},
	)
	scopesIn := []Scope{wantScopes[1], wantScopes[0], wantScopes[1]}
	wantSels := wantSelectionOrder(
		sel(f.e1, Diagnosis, f.dxSel),
		sel(f.e1, Diagnosis, f.dxSel2),
	)
	selsIn := []RecordSelection{wantSels[1], wantSels[0], wantSels[1], wantSels[0]}

	// 提交前的快照：用于逐字段核对调用方列表未被创建过程整理。
	scopesInOrig := append([]Scope(nil), scopesIn...)
	selsInOrig := append([]RecordSelection(nil), selsIn...)

	a, err := f.s.GrantSelective(doc, f.pid, rcv.ID, scopesIn, selsIn, f.st, f.en)
	if err != nil {
		t.Fatalf("grant selective: %v", err)
	}

	// 返回的授权按既有规则去重、排序：重复条目只算一次，不出现重复覆盖。
	if !reflect.DeepEqual(a.Scopes, wantScopes) {
		t.Fatalf("returned scopes = %+v, want deduped sorted %+v", a.Scopes, wantScopes)
	}
	if !reflect.DeepEqual(a.Selections, wantSels) {
		t.Fatalf("returned selections = %+v, want deduped sorted %+v", a.Selections, wantSels)
	}

	// 调用方提交的列表保持原有顺序、重复次数与每项内容。
	if !reflect.DeepEqual(scopesIn, scopesInOrig) {
		t.Fatalf("caller scopes rewritten by grant:\ngot  = %+v\nwant = %+v", scopesIn, scopesInOrig)
	}
	if !reflect.DeepEqual(selsIn, selsInOrig) {
		t.Fatalf("caller selections rewritten by grant:\ngot  = %+v\nwant = %+v", selsIn, selsInOrig)
	}

	// 保存的授权同样去重、排序，不因保留调用方输入而出现重复覆盖。
	stored, err := f.s.GetAuthorization(doc, f.pid, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(stored.Scopes, wantScopes) || !reflect.DeepEqual(stored.Selections, wantSels) {
		t.Fatalf("stored grant scopes/selections = %+v / %+v, want %+v / %+v",
			stored.Scopes, stored.Selections, wantScopes, wantSels)
	}

	// 授权保存后，调用方在本地复用并改写原列表准备下一次共享：
	// 整类范围改到另一就诊，限定选择换成同类的另一条已生效记录。
	scopesIn[0].EncounterID = f.e2
	scopesIn[1].Category = Diagnosis
	for i := range selsIn {
		if selsIn[i].RecordID == f.dxSel {
			selsIn[i].RecordID = f.dxFree
		}
	}

	// 本地改写不影响先前返回的授权。
	if !reflect.DeepEqual(a.Scopes, wantScopes) || !reflect.DeepEqual(a.Selections, wantSels) {
		t.Fatalf("previously returned authorization changed by caller list edits: %+v", a)
	}
	// 也不影响内部使用者重新查看到的正式范围。
	stored, err = f.s.GetAuthorization(doc, f.pid, a.ID)
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if !reflect.DeepEqual(stored.Scopes, wantScopes) || !reflect.DeepEqual(stored.Selections, wantSels) {
		t.Fatalf("stored grant changed by caller list edits: %+v", stored)
	}

	// 接收方仍按首次成功提交的范围取得内容：整类范围内原本允许的
	// 记录仍可读，限定范围内原选记录仍可读；被本地换入且未获其他
	// 授权的 dxFree 不能因此出现。
	ords, err := f.s.Read(rcv, f.pid, f.e1, Order)
	if err != nil {
		t.Fatalf("read orders: %v", err)
	}
	if ids := readRecordIDs(ords); len(ids) != 2 || !ids[f.orA] || !ids[f.orB] {
		t.Fatalf("whole-category order read = %v, want orA+orB", ids)
	}
	diags, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("read diagnoses: %v", err)
	}
	if ids := readRecordIDs(diags); len(ids) != 2 || !ids[f.dxSel] || !ids[f.dxSel2] || ids[f.dxFree] {
		t.Fatalf("limited diagnosis read = %v, want only dxSel+dxSel2 (dxFree never covered)", ids)
	}
	ords2, err := f.s.Read(rcv, f.pid, f.e2, Order)
	if err != nil {
		t.Fatalf("read e2 orders: %v", err)
	}
	if ids := readRecordIDs(ords2); len(ids) != 1 || !ids[f.orE2] {
		t.Fatalf("second whole-category scope read = %v, want orE2", ids)
	}

	// 保留 Grant 的整类授权用法：另一接收方经旧形式获得 e1 诊断整类
	// 授权，返回结果只有整类范围，读取自然包含未被限定选中的 dxFree。
	legacy, err := f.s.Grant(doc, f.pid, rcvB.ID,
		[]Scope{{EncounterID: f.e1, Category: Diagnosis}}, f.st, f.en)
	if err != nil {
		t.Fatalf("legacy grant: %v", err)
	}
	if len(legacy.Scopes) != 1 || len(legacy.Selections) != 0 {
		t.Fatalf("legacy Grant must stay whole-category only: %+v", legacy)
	}
	legacyDiags, err := f.s.Read(rcvB, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("legacy receiver read: %v", err)
	}
	if ids := readRecordIDs(legacyDiags); len(ids) != 3 || !ids[f.dxSel] || !ids[f.dxSel2] || !ids[f.dxFree] {
		t.Fatalf("whole-category diagnosis read = %v, want all three diagnoses", ids)
	}
}

// 失败边界：同一批范围里混入一条已生效记录，却把它声明为该患者另一次
// 已登记就诊下的记录，应返回 ErrInvalidArgument，整条授权不成立；两份
// 输入列表保持原样，不保存合法部分，不新增授权创建审计，已有授权与
// 接收方可读内容不变。

func TestGrantSelectiveFailureKeepsCallerListsAndState(t *testing.T) {
	f := setupGrantInputFixture(t)

	// 先建立一条已有授权，作为“已有授权与接收方可读内容不变”的基准。
	existing, err := f.s.GrantSelective(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Order}},
		[]RecordSelection{sel(f.e1, Diagnosis, f.dxSel)},
		f.st, f.en)
	if err != nil {
		t.Fatalf("existing grant: %v", err)
	}
	auditBefore := len(mustAudit(t, f.s, f.pid))
	totalBefore := totalAuthorizations(f.s)

	// 混入一条已生效记录，却声明在该患者另一次已登记就诊 e2 下。
	scopesIn := []Scope{
		{EncounterID: f.e1, Category: Order},
		{EncounterID: f.e1, Category: Order}, // 重复条目
	}
	selsIn := []RecordSelection{
		sel(f.e1, Diagnosis, f.dxSel2),
		sel(f.e2, Diagnosis, f.dxSel), // dxSel 属于 e1，声明到 e2 下
		sel(f.e1, Diagnosis, f.dxSel2),
	}
	scopesInOrig := append([]Scope(nil), scopesIn...)
	selsInOrig := append([]RecordSelection(nil), selsIn...)

	failed, err := f.s.GrantSelective(doc, f.pid, rcv.ID, scopesIn, selsIn, f.st, f.en)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("misdeclared encounter err = %v, want ErrInvalidArgument", err)
	}
	if !reflect.DeepEqual(failed, Authorization{}) {
		t.Fatalf("failed grant returned a value: %+v", failed)
	}

	// 两份输入列表保持原样：顺序、重复次数与每项内容都不被失败的整理改写。
	if !reflect.DeepEqual(scopesIn, scopesInOrig) {
		t.Fatalf("caller scopes rewritten by failed grant:\ngot  = %+v\nwant = %+v", scopesIn, scopesInOrig)
	}
	if !reflect.DeepEqual(selsIn, selsInOrig) {
		t.Fatalf("caller selections rewritten by failed grant:\ngot  = %+v\nwant = %+v", selsIn, selsInOrig)
	}

	// 整条授权不成立：不保存合法部分，不新增授权创建审计。
	if got := totalAuthorizations(f.s); got != totalBefore {
		t.Fatalf("failed grant changed authorization count: %d -> %d", totalBefore, got)
	}
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("failed grant added audit: %d -> %d", auditBefore, got)
	}
	auths, err := f.s.ListAuthorizations(doc, f.pid, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	assertAuthIDSet(t, auths, existing.ID)

	// 已有授权与接收方可读内容不变：整类医嘱仍可读，限定诊断仍只有
	// dxSel，dxFree 与失败批次里的 dxSel2 都不因此出现。
	stored, err := f.s.GetAuthorization(doc, f.pid, existing.ID)
	if err != nil {
		t.Fatalf("get existing: %v", err)
	}
	if !reflect.DeepEqual(stored, existing) {
		t.Fatalf("existing grant changed by failed batch:\ngot  = %+v\nwant = %+v", stored, existing)
	}
	ords, err := f.s.Read(rcv, f.pid, f.e1, Order)
	if err != nil {
		t.Fatalf("read orders: %v", err)
	}
	if ids := readRecordIDs(ords); len(ids) != 2 || !ids[f.orA] || !ids[f.orB] {
		t.Fatalf("order read after failed grant = %v, want orA+orB", ids)
	}
	diags, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("read diagnoses: %v", err)
	}
	if ids := readRecordIDs(diags); len(ids) != 1 || !ids[f.dxSel] || ids[f.dxSel2] || ids[f.dxFree] {
		t.Fatalf("diagnosis read after failed grant = %v, want only dxSel", ids)
	}
}
