package clinical

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

// 本文件为内部使用者“按患者查看完整档案”的 Chart 查询补多患者隔离回归保障：
// 同一存储中并存两名合成患者，二人都有多次就诊，各自保存了诊断、医嘱草稿与
// 经过更正的生效记录，且刻意使用相同文字、相同时间并让两人的操作互相交错。
// 这些测试保证 Chart 只汇总所查患者的资料：
//   - 档案信息、全部就诊、记录（草稿与完整版本历史）与审计事件都只属于所查
//     患者；另一人的就诊、草稿、版本内容与审计事件不得进入结果，哪怕另一人
//     的数据更多、文字完全相同、操作发生在同一时刻或穿插在中间；
//   - 就诊按发生时间排列，时间相同时按就诊标识排列；记录按记录标识稳定排列；
//     审计只保留所查患者的事件并保持真实追加次序（时间相同也不被打乱），
//     事件的操作身份、动作、对象与时间保持原值；
//   - 范围由患者决定而不是由查询者创建过什么决定：另一名内部使用者（包括
//     从未给该患者创建过任何材料的第三人）仍能看到该患者的全部内容；
//   - 查询是只读操作：不新增审计、不改变档案；患者停用后草稿与完整历史仍在；
//   - 刚登记、没有任何业务操作的患者返回自己的档案与空集合，不借用他人材料；
//   - 接收方即使持有所查患者的有效内容授权，Chart 也返回 ErrAccessDenied，
//     且结果不携带患者信息、草稿、历史版本或审计。

// ---- 本文件专用的小型构造助手（名字带 chartIso 前缀，避免与其他测试冲突） ----

func chartIsoRegister(t *testing.T, s *Store, actor Actor, name string) ID {
	t.Helper()
	p, err := s.RegisterPatient(actor, name)
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return p.ID
}

func chartIsoEncounter(t *testing.T, s *Store, actor Actor, pid ID, at time.Time) ID {
	t.Helper()
	e, err := s.AddEncounter(actor, pid, at)
	if err != nil {
		t.Fatalf("add encounter for %s at %v: %v", pid, at, err)
	}
	return e.ID
}

func chartIsoDraft(t *testing.T, s *Store, actor Actor, pid, eid ID, category, content string) ID {
	t.Helper()
	r, err := s.CreateDraft(actor, pid, eid, category, content)
	if err != nil {
		t.Fatalf("create draft %s/%s: %v", eid, category, err)
	}
	return r.ID
}

func chartIsoActivate(t *testing.T, s *Store, actor Actor, rid ID) Version {
	t.Helper()
	v, err := s.ActivateRecord(actor, rid)
	if err != nil {
		t.Fatalf("activate %s: %v", rid, err)
	}
	return v
}

func chartIsoCorrect(t *testing.T, s *Store, actor Actor, rid ID, expected int, content, reason string) Version {
	t.Helper()
	v, err := s.CorrectRecord(actor, rid, expected, content, reason)
	if err != nil {
		t.Fatalf("correct %s at %d: %v", rid, expected, err)
	}
	return v
}

// chartIsoFixture 在同一存储里造出两名数据交错的合成患者。
//
// 甲（被查询对象）与乙（干扰对象）：
//   - 各有三个就诊，其中两个就诊的发生时间相同（验证同刻按就诊标识排序，
//     以及另一人的同刻就诊不会混入）；
//   - 两人的诊断/医嘱使用逐字相同的内容文字，但更正原因带“甲/乙”前缀，
//     版本标识各不相同，足以识别归属；
//   - 两人的生效、更正、授权等操作严格交替执行，并共用注入时钟上的同一时刻，
//     形成跨患者的同刻事件对；
//   - 乙的记录与审计刻意更多（多一条已更正诊断、多一条就诊记录、交换创建与
//     回执事件），防止“数据更多的患者把别人的查询补齐/挤掉”。
type chartIsoData struct {
	pa, pb ID

	aTie1, aTie2, aLater ID
	bTie1, bTie2, bLater ID

	// 甲的记录标识。
	ar1, ao1  string // aTie1：诊断更正到第 3 版；医嘱更正到第 2 版
	ar2       string // aLater：诊断更正到第 2 版
	aDraft    string // aLater：始终未生效的医嘱草稿
	aTieDraft string // aTie2：始终未生效的医嘱草稿

	// 甲记录的各版本标识（旧到新）。
	ar1V                            [3]ID
	ao1V1, ao1V2                    ID
	ar2V1, ar2V2                    ID
	aDraftContent, aTieDraftContent string
	aGrant                          ID

	// 乙的对象（仅用于交错制造与反向核对）。
	br1 string

	// times 记录甲各审计事件对应的注入时钟时刻，用于逐条核对真实操作时间。
	times map[string]time.Time
}

func chartIsoFixture(t *testing.T) (*Store, *fakeClock, chartIsoData) {
	t.Helper()
	s, clk := newTestStore(t)
	d := chartIsoData{}

	// 两名患者在同一时刻登记（CreatedAt 相同），只有标识与姓名不同。
	t0 := clk.t
	d.pa = chartIsoRegister(t, s, doc, "合成患者隔离甲")
	d.pb = chartIsoRegister(t, s, doc2, "合成患者隔离乙")

	// 各三个就诊：前两个发生时间相同（同刻，靠标识定序），第三个更晚。
	tie := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	later := time.Date(2026, 2, 2, 10, 0, 0, 0, time.UTC)
	d.aTie1 = chartIsoEncounter(t, s, doc, d.pa, tie)
	d.bTie1 = chartIsoEncounter(t, s, doc2, d.pb, tie)
	d.aTie2 = chartIsoEncounter(t, s, doc, d.pa, tie)
	d.bTie2 = chartIsoEncounter(t, s, doc2, d.pb, tie)
	d.aLater = chartIsoEncounter(t, s, doc, d.pa, later)
	d.bLater = chartIsoEncounter(t, s, doc2, d.pb, later)

	// 共同文字：两人逐字相同，任何“按内容串起来”的错误实现都会把版本串台。
	const (
		dxV1 = "共同诊断文字-原始内容"
		dxV2 = "共同诊断文字-第二版内容"
		dxV3 = "共同诊断文字-第三版内容"
		orV1 = "共同医嘱文字-原始内容"
		orV2 = "共同医嘱文字-第二版内容"
	)
	d.aDraftContent = "未生效草稿当前内容"
	d.aTieDraftContent = "同刻就诊的共同草稿文字"

	// 甲、乙各自的草稿。
	d.ar1 = chartIsoDraft(t, s, doc, d.pa, d.aTie1, Diagnosis, dxV1)
	br1 := chartIsoDraft(t, s, doc2, d.pb, d.bTie1, Diagnosis, dxV1)
	d.br1 = br1
	d.ao1 = chartIsoDraft(t, s, doc, d.pa, d.aTie1, Order, orV1)
	bo1 := chartIsoDraft(t, s, doc2, d.pb, d.bTie1, Order, orV1)
	// 乙额外多一条诊断（乙的数据刻意更多）。
	bx1 := chartIsoDraft(t, s, doc, d.pb, d.bTie1, Diagnosis, dxV1)

	// 甲 aTie2 的草稿（同刻就诊，未生效）；乙 bTie2 放一条生效记录加一条草稿。
	d.aTieDraft = chartIsoDraft(t, s, doc2, d.pa, d.aTie2, Order, d.aTieDraftContent+"（初稿）")
	bt1 := chartIsoDraft(t, s, doc, d.pb, d.bTie2, Diagnosis, dxV1)
	btDraft := chartIsoDraft(t, s, doc2, d.pb, d.bTie2, Order, d.aTieDraftContent)

	// 甲 aLater：一条将更正的诊断 + 一条始终未生效的医嘱草稿；乙同文对应。
	d.ar2 = chartIsoDraft(t, s, doc2, d.pa, d.aLater, Diagnosis, dxV1)
	br2 := chartIsoDraft(t, s, doc, d.pb, d.bLater, Diagnosis, dxV1)
	d.aDraft = chartIsoDraft(t, s, doc, d.pa, d.aLater, Order, d.aDraftContent+"（初稿）")
	bdraft := chartIsoDraft(t, s, doc2, d.pb, d.bLater, Order, d.aDraftContent)

	// 未生效草稿保留“当前内容”：甲的两条草稿各更新一次，始终不生效。
	if _, err := s.UpdateDraft(doc, d.aDraft, d.aDraftContent); err != nil {
		t.Fatalf("update aDraft: %v", err)
	}
	if _, err := s.UpdateDraft(doc2, d.aTieDraft, d.aTieDraftContent); err != nil {
		t.Fatalf("update aTieDraft: %v", err)
	}

	// 授权交错（也各自产生审计）。接收方 rcv 持甲的有效内容授权，rcvB 持乙的。
	clk.t = t0.Add(24 * time.Hour)
	grantAt := clk.t
	gA, err := s.Grant(doc, d.pa, rcv.ID,
		[]Scope{{EncounterID: d.aTie1, Category: Diagnosis}},
		grantAt.Add(-time.Hour), grantAt.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("grant A: %v", err)
	}
	d.aGrant = gA.ID
	gB, err := s.Grant(doc2, d.pb, rcvB.ID,
		[]Scope{{EncounterID: d.bTie1, Category: Diagnosis}},
		grantAt.Add(-time.Hour), grantAt.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("grant B: %v", err)
	}

	// 生效操作严格交替，且同一时刻跨患者成对发生。
	clk.t = clk.t.Add(time.Hour)
	actAt := clk.t
	d.ar1V[0] = chartIsoActivate(t, s, doc, d.ar1).ID
	chartIsoActivate(t, s, doc2, br1)
	d.ao1V1 = chartIsoActivate(t, s, doc2, d.ao1).ID
	chartIsoActivate(t, s, doc, bo1)
	chartIsoActivate(t, s, doc, bx1) // 乙多一条

	// 更正同样严格交替：甲 ar1、乙 br1、甲 ao1、乙 bo1、乙 bx1，同一时刻。
	clk.t = clk.t.Add(time.Hour)
	corrAt1 := clk.t
	d.ar1V[1] = chartIsoCorrect(t, s, doc2, d.ar1, 1, dxV2, "甲诊断第一次更正原因").ID
	chartIsoCorrect(t, s, doc, br1, 1, dxV2, "乙诊断第一次更正原因")
	d.ao1V2 = chartIsoCorrect(t, s, doc, d.ao1, 1, orV2, "甲医嘱更正原因").ID
	chartIsoCorrect(t, s, doc2, bo1, 1, orV2, "乙医嘱更正原因")
	chartIsoCorrect(t, s, doc2, bx1, 1, dxV2, "乙额外诊断更正原因")

	// 甲 ar1 更正到第 3 版，乙 br1 同步更正到第 3 版（同文同长度版本链）。
	clk.t = clk.t.Add(time.Hour)
	corrAt2 := clk.t
	d.ar1V[2] = chartIsoCorrect(t, s, doc, d.ar1, 2, dxV3, "甲诊断第二次更正原因").ID
	chartIsoCorrect(t, s, doc2, br1, 2, dxV3, "乙诊断第二次更正原因")

	// 第二个就诊时间点：甲、乙再次同刻生效与更正，中间还夹着乙 bTie2 的生效。
	clk.t = clk.t.Add(time.Hour)
	actAt2 := clk.t
	d.ar2V1 = chartIsoActivate(t, s, doc2, d.ar2).ID
	chartIsoActivate(t, s, doc, br2)
	chartIsoActivate(t, s, doc, bt1)
	_ = btDraft

	clk.t = clk.t.Add(time.Hour)
	corrAt3 := clk.t
	d.ar2V2 = chartIsoCorrect(t, s, doc2, d.ar2, 1, dxV2, "甲第二就诊诊断更正原因").ID
	chartIsoCorrect(t, s, doc, br2, 1, dxV2, "乙第二就诊诊断更正原因")
	_ = bdraft

	// 乙独有的交换创建与回执（审计动作、对象与操作人都与甲无关），夹在甲查询前。
	clk.t = clk.t.Add(time.Hour)
	exchAt := clk.t
	exchB, err := s.CreateExchange(doc2, d.pb, rcvB.ID, gB.ID, []ID{br1}, "request-chart-iso-b")
	if err != nil {
		t.Fatalf("create exchange B: %v", err)
	}
	delivery, err := s.FetchPackage(rcvB, exchB.ID)
	if err != nil {
		t.Fatalf("fetch B package: %v", err)
	}
	if _, err := s.SubmitReceipt(rcvB, exchB.ID, delivery.Digest, ReceiptAccepted, ""); err != nil {
		t.Fatalf("submit B receipt: %v", err)
	}

	// 两名患者在夹具中都保持活跃；需要停用甲的测试自行停用并自行核对。

	// 记录期望的甲审计时间线（跨患者同刻事件对必须保持这个追加次序）。
	d.times = map[string]time.Time{
		"grant": grantAt, "act1": actAt, "corr1": corrAt1, "corr2": corrAt2,
		"act2": actAt2, "corr3": corrAt3, "exch": exchAt,
	}
	return s, clk, d
}

// chartIsoRecordIDs 取出档案中全部记录标识。
func chartIsoRecordIDs(c PatientChart) []ID {
	ids := make([]ID, 0, len(c.Records))
	for _, h := range c.Records {
		ids = append(ids, h.Record.ID)
	}
	return ids
}

// chartIsoEncounterIDs 取出档案中全部就诊标识（按返回顺序）。
func chartIsoEncounterIDs(c PatientChart) []ID {
	ids := make([]ID, 0, len(c.Encounters))
	for _, e := range c.Encounters {
		ids = append(ids, e.ID)
	}
	return ids
}

func chartIsoSorted(ids []ID) []ID {
	out := append([]ID(nil), ids...)
	sort.Strings(out)
	return out
}

// ---- 核心：查询甲时，档案、就诊、记录与审计只属于甲 ----

func TestChartScopedToRequestedPatientAcrossInterleavedPatients(t *testing.T) {
	s, _, d := chartIsoFixture(t)

	chart, err := s.Chart(doc, d.pa)
	if err != nil {
		t.Fatalf("chart A: %v", err)
	}

	// 档案信息是甲本人，不能被同名/同刻登记的乙顶替。
	if chart.Patient.ID != d.pa || chart.Patient.Name != "合成患者隔离甲" ||
		chart.Patient.Source != SyntheticSource || chart.Patient.Deactivated {
		t.Fatalf("chart patient header wrong: %+v", chart.Patient)
	}

	// 就诊：恰好甲的三个；先按发生时间、同刻按就诊标识排列；不含乙的任何就诊。
	wantEncTie := chartIsoSorted([]ID{d.aTie1, d.aTie2})
	gotEnc := chartIsoEncounterIDs(chart)
	wantEnc := append([]ID{wantEncTie[0], wantEncTie[1]}, d.aLater)
	if !reflect.DeepEqual(gotEnc, wantEnc) {
		t.Fatalf("encounter order = %v, want %v", gotEnc, wantEnc)
	}
	bEncounters := map[ID]bool{d.bTie1: true, d.bTie2: true, d.bLater: true}
	for i := 1; i < len(chart.Encounters); i++ {
		prev, cur := chart.Encounters[i-1], chart.Encounters[i]
		if prev.OccurredAt.After(cur.OccurredAt) ||
			(prev.OccurredAt.Equal(cur.OccurredAt) && prev.ID > cur.ID) {
			t.Fatalf("encounters not ordered by (time,id): %+v before %+v", prev, cur)
		}
		if cur.PatientID != d.pa || bEncounters[cur.ID] {
			t.Fatalf("encounter from patient B leaked into A chart: %+v", cur)
		}
	}

	// 记录：恰好甲的 5 条，按记录标识稳定排列，全部属于甲且挂在甲的就诊下。
	gotRecIDs := chartIsoRecordIDs(chart)
	wantRecIDs := chartIsoSorted([]ID{d.ar1, d.ao1, d.ar2, d.aDraft, d.aTieDraft})
	if !reflect.DeepEqual(gotRecIDs, wantRecIDs) {
		t.Fatalf("record set/order = %v, want sorted %v", gotRecIDs, wantRecIDs)
	}
	aEncounters := map[ID]bool{d.aTie1: true, d.aTie2: true, d.aLater: true}
	histories := historiesByID(chart.Records)

	// 甲的版本标识全集：任何出现在档案里的版本标识都必须在这个集合内。
	aVersionIDs := map[ID]bool{}
	for _, vid := range [3]ID{d.ar1V[0], d.ar1V[1], d.ar1V[2]} {
		aVersionIDs[vid] = true
	}
	for _, vid := range []ID{d.ao1V1, d.ao1V2, d.ar2V1, d.ar2V2} {
		aVersionIDs[vid] = true
	}

	for _, h := range chart.Records {
		r := h.Record
		if r.PatientID != d.pa || !aEncounters[r.EncounterID] {
			t.Fatalf("record leaked into A chart: %+v", r)
		}
		if r.ID == d.br1 {
			t.Fatalf("patient B record id leaked into A chart")
		}
		// 版本标识链与展开历史都只能引用甲自己的版本。
		for _, vid := range r.Versions {
			if !aVersionIDs[vid] {
				t.Fatalf("record %s carries a foreign version id %q", r.ID, vid)
			}
		}
		for _, v := range h.Versions {
			if !aVersionIDs[v.ID] || v.RecordID != r.ID {
				t.Fatalf("foreign version leaked into record %s: %+v", r.ID, v)
			}
		}
	}

	// 已更正记录 ar1：当前是第 3 版；旧到新三个版本的原内容、PrevID 链与
	// 更正原因都还属于 ar1，且不是乙 br1 的同文版本。
	ar1 := histories[d.ar1]
	if ar1.HasDraft || ar1.DraftContent != "" {
		t.Fatalf("effective record must not present its cleared draft: %+v", ar1)
	}
	if ar1.CurrentVersion == nil || ar1.CurrentVersion.ID != d.ar1V[2] ||
		ar1.CurrentVersion.Number != 3 || ar1.CurrentVersion.Content != "共同诊断文字-第三版内容" ||
		ar1.CurrentVersion.PrevID != d.ar1V[1] || ar1.CurrentVersion.Reason != "甲诊断第二次更正原因" ||
		ar1.CurrentVersion.RecordID != d.ar1 {
		t.Fatalf("ar1 current version wrong: %+v", ar1.CurrentVersion)
	}
	if want := []ID{d.ar1V[0], d.ar1V[1], d.ar1V[2]}; !reflect.DeepEqual(ar1.Record.Versions, want) {
		t.Fatalf("ar1 version id chain = %v, want %v", ar1.Record.Versions, want)
	}
	if len(ar1.Versions) != 3 {
		t.Fatalf("ar1 history should keep 3 versions, got %d", len(ar1.Versions))
	}
	v1, v2, v3 := ar1.Versions[0], ar1.Versions[1], ar1.Versions[2]
	if v1.ID != d.ar1V[0] || v1.Number != 1 || v1.Content != "共同诊断文字-原始内容" ||
		v1.PrevID != "" || v1.Reason != "" || v1.RecordID != d.ar1 {
		t.Fatalf("ar1 v1 not preserved as its own original: %+v", v1)
	}
	if v2.ID != d.ar1V[1] || v2.Number != 2 || v2.Content != "共同诊断文字-第二版内容" ||
		v2.PrevID != d.ar1V[0] || v2.Reason != "甲诊断第一次更正原因" || v2.RecordID != d.ar1 {
		t.Fatalf("ar1 v2 chain/reason wrong: %+v", v2)
	}
	if v3.PrevID != d.ar1V[1] || v3.RecordID != d.ar1 {
		t.Fatalf("ar1 v3 chain broken: %+v", v3)
	}

	// 已更正医嘱 ao1：第 2 版当前，第 1 版原样保留。
	ao1 := histories[d.ao1]
	if ao1.CurrentVersion == nil || ao1.CurrentVersion.ID != d.ao1V2 ||
		ao1.CurrentVersion.Number != 2 || ao1.CurrentVersion.Content != "共同医嘱文字-第二版内容" ||
		ao1.CurrentVersion.Reason != "甲医嘱更正原因" || ao1.CurrentVersion.PrevID != d.ao1V1 {
		t.Fatalf("ao1 current version wrong: %+v", ao1.CurrentVersion)
	}
	if len(ao1.Versions) != 2 || ao1.Versions[0].ID != d.ao1V1 ||
		ao1.Versions[0].Content != "共同医嘱文字-原始内容" || ao1.Versions[0].PrevID != "" {
		t.Fatalf("ao1 v1 not preserved: %+v", ao1.Versions)
	}

	// ar2 完整链路。
	ar2 := histories[d.ar2]
	if ar2.CurrentVersion == nil || ar2.CurrentVersion.ID != d.ar2V2 || ar2.CurrentVersion.Number != 2 ||
		ar2.CurrentVersion.Reason != "甲第二就诊诊断更正原因" || ar2.CurrentVersion.PrevID != d.ar2V1 {
		t.Fatalf("ar2 current version wrong: %+v", ar2.CurrentVersion)
	}
	if len(ar2.Versions) != 2 || ar2.Versions[0].ID != d.ar2V1 || ar2.Versions[0].PrevID != "" {
		t.Fatalf("ar2 history wrong: %+v", ar2.Versions)
	}

	// 未生效草稿：保留当前草稿内容，没有当前版本、没有任何历史版本，
	// 同文的乙草稿不能在这里表现出生效版本。
	for _, rid := range []ID{d.aDraft, d.aTieDraft} {
		h := histories[rid]
		if h.Record.EncounterID == d.aLater && rid == d.aDraft {
			if !h.HasDraft || h.DraftContent != d.aDraftContent {
				t.Fatalf("aDraft current content not preserved: %+v", h)
			}
		} else {
			if !h.HasDraft || h.DraftContent != d.aTieDraftContent {
				t.Fatalf("aTieDraft current content not preserved: %+v", h)
			}
		}
		if h.CurrentVersion != nil || h.Record.CurrentVersionID != "" ||
			len(h.Record.Versions) != 0 || len(h.Versions) != 0 {
			t.Fatalf("draft must carry no effective version, got %+v", h)
		}
	}

	// 任何乙的痕迹（“乙”字头原因、乙的交换/回执事件）都不得出现在甲的档案里。
	for _, h := range chart.Records {
		for _, v := range h.Versions {
			if containsChineseYi(v.Reason) {
				t.Fatalf("patient B correction reason leaked into A record %s: %q", h.Record.ID, v.Reason)
			}
		}
	}

	// 审计：恰好甲的 8 个事件，保持真实追加次序与原有操作身份/动作/对象/时间；
	// 跨患者同刻事件对中乙的事件被过滤后，甲这一侧的先后关系不变。
	wantAudit := []struct {
		action, objectType, objectID, actorID string
		atKey                                 string
	}{
		{ActionGranted, "authorization", d.aGrant, doc.ID, "grant"},
		{ActionActivated, "record", d.ar1, doc.ID, "act1"},
		{ActionActivated, "record", d.ao1, doc2.ID, "act1"},
		{ActionCorrected, "record", d.ar1, doc2.ID, "corr1"},
		{ActionCorrected, "record", d.ao1, doc.ID, "corr1"},
		{ActionCorrected, "record", d.ar1, doc.ID, "corr2"},
		{ActionActivated, "record", d.ar2, doc2.ID, "act2"},
		{ActionCorrected, "record", d.ar2, doc2.ID, "corr3"},
	}
	gotAudit := chart.AuditEvents
	if len(gotAudit) != len(wantAudit) {
		t.Fatalf("A audit count = %d (%v), want %d", len(gotAudit), auditIDList(gotAudit), len(wantAudit))
	}
	seenAudit := map[ID]bool{}
	for i, w := range wantAudit {
		ev := gotAudit[i]
		if ev.PatientID != d.pa || ev.Action != w.action || ev.ObjectType != w.objectType ||
			ev.ObjectID != w.objectID || ev.ActorID != w.actorID {
			t.Fatalf("A audit event %d = %+v, want %s on %s by %s", i, ev, w.action, w.objectID, w.actorID)
		}
		if !ev.OccurredAt.Equal(d.times[w.atKey]) {
			t.Fatalf("A audit event %d time = %v, want %v", i, ev.OccurredAt, d.times[w.atKey])
		}
		if ev.ID == "" || seenAudit[ev.ID] {
			t.Fatalf("A audit event %d id missing or duplicated: %q", i, ev.ID)
		}
		seenAudit[ev.ID] = true
	}
	// 同刻事件对必须确实同刻，保证“时间相同仍按追加次序”被覆盖。
	if !gotAudit[1].OccurredAt.Equal(gotAudit[2].OccurredAt) ||
		!gotAudit[3].OccurredAt.Equal(gotAudit[4].OccurredAt) {
		t.Fatal("test setup should interleave same-timestamp A audit events")
	}
	for _, ev := range gotAudit {
		// 乙的交换、回执（接收方身份）与授权事件一律不得出现。
		if ev.ObjectID == d.pb || ev.ActorID == rcvB.ID ||
			ev.Action == ActionExchanged || ev.Action == ActionReceipted {
			t.Fatalf("patient B audit event leaked into A chart: %+v", ev)
		}
	}

	// 乙的档案自成一域：数据确实比甲更多，且反向查询不包含甲的任何内容。
	chartB, err := s.Chart(doc2, d.pb)
	if err != nil {
		t.Fatalf("chart B: %v", err)
	}
	if len(chartB.Records) <= len(chart.Records) {
		t.Fatalf("setup should give B more records than A: A=%d B=%d",
			len(chart.Records), len(chartB.Records))
	}
	if len(chartB.AuditEvents) <= len(chart.AuditEvents) {
		t.Fatalf("setup should give B more audit events than A: A=%d B=%d",
			len(chart.AuditEvents), len(chartB.AuditEvents))
	}
	bIDs := map[ID]bool{}
	for _, h := range chartB.Records {
		bIDs[h.Record.ID] = true
		if h.Record.PatientID != d.pb {
			t.Fatalf("patient A record leaked into B chart: %+v", h.Record)
		}
	}
	for _, id := range wantRecIDs {
		if bIDs[id] {
			t.Fatalf("patient A record %q leaked into B chart", id)
		}
	}
}

// containsChineseYi 报告原因里是否出现乙患者特有的“乙”标记，用于反向断言
// 乙的更正原因没有串到甲的版本里。
func containsChineseYi(s string) bool {
	// “乙”是乙患者全部更正原因的统一前缀；只做子串判定即可。
	for _, r := range s {
		if r == '乙' {
			return true
		}
	}
	return false
}

// ---- 范围由患者决定：换任何有效内部使用者都看到同一份完整档案 ----

func TestChartScopeFollowsPatientNotAsker(t *testing.T) {
	s, _, d := chartIsoFixture(t)

	byDoc, err := s.Chart(doc, d.pa)
	if err != nil {
		t.Fatalf("chart by doc: %v", err)
	}
	// doc2 创建/更正过甲的部分材料（ao1 生效、ar1 的第 2 版、ar2 全部），
	// 但它必须看到甲的全部 5 条记录与 9 个审计事件，而不只是自己经手的部分。
	byDoc2, err := s.Chart(doc2, d.pa)
	if err != nil {
		t.Fatalf("chart by doc2: %v", err)
	}
	third := InternalActor("dr-third-iso")
	// 第三人从未给甲创建过任何材料：范围仍只由甲决定。
	byThird, err := s.Chart(third, d.pa)
	if err != nil {
		t.Fatalf("chart by third internal actor: %v", err)
	}

	wantRec := chartIsoSorted([]ID{d.ar1, d.ao1, d.ar2, d.aDraft, d.aTieDraft})
	wantEncTie := chartIsoSorted([]ID{d.aTie1, d.aTie2})
	wantEnc := append([]ID{wantEncTie[0], wantEncTie[1]}, d.aLater)
	for _, c := range []PatientChart{byDoc, byDoc2, byThird} {
		if c.Patient.ID != d.pa {
			t.Fatalf("chart patient header wrong for asker: %+v", c.Patient)
		}
		if got := chartIsoRecordIDs(c); !reflect.DeepEqual(got, wantRec) {
			t.Fatalf("records for an internal asker = %v, want all A records %v", got, wantRec)
		}
		if got := chartIsoEncounterIDs(c); !reflect.DeepEqual(got, wantEnc) {
			t.Fatalf("encounters for an internal asker = %v, want %v", got, wantEnc)
		}
		if len(c.AuditEvents) != 8 {
			t.Fatalf("audit for an internal asker = %d events, want all 8 A events", len(c.AuditEvents))
		}
	}

	// 三名查询者拿到的审计顺序与身份信息完全一致（查询者不参与过滤）。
	if !reflect.DeepEqual(byDoc.AuditEvents, byDoc2.AuditEvents) ||
		!reflect.DeepEqual(byDoc.AuditEvents, byThird.AuditEvents) {
		t.Fatalf("audit scope changed with the asking internal actor:\n doc=%+v\n doc2=%+v\n third=%+v",
			byDoc.AuditEvents, byDoc2.AuditEvents, byThird.AuditEvents)
	}
}

// ---- 查询只读：不新增审计、不改变档案；停用后草稿与完整历史仍在 ----

func TestChartIsReadOnlyAndWorksAfterDeactivation(t *testing.T) {
	s, clk, d := chartIsoFixture(t)

	auditBefore, err := s.AuditEvents(doc, d.pa)
	if err != nil {
		t.Fatalf("audit before: %v", err)
	}
	if len(auditBefore) != 8 {
		t.Fatalf("setup should leave 8 A events, got %d", len(auditBefore))
	}
	patientBefore, err := s.GetPatient(doc, d.pa)
	if err != nil {
		t.Fatalf("get patient before: %v", err)
	}

	// 停用前查询一次，确认活跃档案下草稿与完整历史都在。
	activeChart, err := s.Chart(doc2, d.pa)
	if err != nil {
		t.Fatalf("chart while active: %v", err)
	}
	if activeChart.Patient.Deactivated {
		t.Fatal("patient should still be active before deactivation")
	}

	// 停用甲：这是真实写操作，追加一条停用审计；随后只读查询不得再改变任何状态。
	clk.t = clk.t.Add(time.Hour)
	if err := s.DeactivatePatient(doc, d.pa); err != nil {
		t.Fatalf("deactivate A: %v", err)
	}
	auditDeactivated, err := s.AuditEvents(doc, d.pa)
	if err != nil {
		t.Fatalf("audit after deactivate: %v", err)
	}
	if len(auditDeactivated) != 9 {
		t.Fatalf("deactivation should add exactly one event, got %d", len(auditDeactivated))
	}
	if last := auditDeactivated[8]; last.Action != ActionDeactivated ||
		last.ObjectType != "patient" || last.ObjectID != d.pa || last.ActorID != doc.ID ||
		!last.OccurredAt.Equal(clk.t) {
		t.Fatalf("deactivation event wrong: %+v", last)
	}

	// 成功查询与被拒绝查询交错进行，均不得产生任何审计或档案变化。
	for i := 0; i < 3; i++ {
		c, err := s.Chart(doc2, d.pa)
		if err != nil {
			t.Fatalf("chart iteration %d: %v", i, err)
		}
		// 停用后仍拿到原有草稿的当前内容与完整版本历史。
		var draftSeen, fullHistorySeen bool
		for _, h := range c.Records {
			if h.Record.ID == d.aDraft && h.HasDraft && h.DraftContent == d.aDraftContent &&
				h.CurrentVersion == nil {
				draftSeen = true
			}
			if h.Record.ID == d.ar1 && h.CurrentVersion != nil && h.CurrentVersion.Number == 3 &&
				len(h.Versions) == 3 {
				fullHistorySeen = true
			}
		}
		if !c.Patient.Deactivated {
			t.Fatalf("iteration %d: chart should still report the deactivated flag", i)
		}
		if !draftSeen || !fullHistorySeen {
			t.Fatalf("iteration %d: deactivated chart lost draft=%v or full history=%v",
				i, draftSeen, fullHistorySeen)
		}
		if _, err := s.Chart(rcv, d.pa); !errors.Is(err, ErrAccessDenied) {
			t.Fatalf("iteration %d: receiver chart err = %v, want ErrAccessDenied", i, err)
		}
	}

	auditAfter, err := s.AuditEvents(doc, d.pa)
	if err != nil {
		t.Fatalf("audit after: %v", err)
	}
	if !reflect.DeepEqual(auditAfter, auditDeactivated) {
		t.Fatalf("Chart queries changed the audit trail after deactivation:\n before=%+v\n after=%+v",
			auditDeactivated, auditAfter)
	}
	patientAfter, err := s.GetPatient(doc, d.pa)
	if err != nil {
		t.Fatalf("get patient after: %v", err)
	}
	if patientAfter == patientBefore || !patientAfter.Deactivated {
		t.Fatalf("patient state should reflect deactivation (queries themselves change nothing):\n before=%+v\n after=%+v",
			patientBefore, patientAfter)
	}
}

// ---- 刚登记、没有任何业务操作的患者：自己的档案 + 空集合，不借他人材料 ----

func TestChartNewPatientHasOwnHeaderAndEmptyCollections(t *testing.T) {
	s, clk, d := chartIsoFixture(t)

	clk.t = clk.t.Add(time.Hour)
	freshID := chartIsoRegister(t, s, doc2, "刚登记患者丙")

	c, err := s.Chart(InternalActor("dr-third-iso"), freshID)
	if err != nil {
		t.Fatalf("chart fresh patient: %v", err)
	}
	if c.Patient.ID != freshID || c.Patient.Name != "刚登记患者丙" ||
		c.Patient.Source != SyntheticSource || c.Patient.Deactivated {
		t.Fatalf("fresh patient header wrong: %+v", c.Patient)
	}
	if len(c.Encounters) != 0 || len(c.Records) != 0 || len(c.AuditEvents) != 0 {
		t.Fatalf("fresh patient must have empty collections, got encounters=%d records=%d audits=%d",
			len(c.Encounters), len(c.Records), len(c.AuditEvents))
	}
	// 不得借用甲/乙的任何材料补齐：所有集合里都不应出现已有患者的标识。
	knownOthers := map[ID]bool{
		d.pa: true, d.pb: true,
		d.aTie1: true, d.aTie2: true, d.aLater: true,
		d.bTie1: true, d.bTie2: true, d.bLater: true,
		d.ar1: true, d.ao1: true, d.ar2: true, d.aDraft: true, d.aTieDraft: true,
	}
	for _, e := range c.Encounters {
		if knownOthers[e.ID] {
			t.Fatalf("fresh patient chart borrowed another patient's encounter: %+v", e)
		}
	}
	for _, h := range c.Records {
		if knownOthers[h.Record.ID] {
			t.Fatalf("fresh patient chart borrowed another patient's record: %+v", h.Record)
		}
	}

	// 反过来，已有患者的档案也不能因为新患者登记而多出什么。
	cA, err := s.Chart(doc, d.pa)
	if err != nil {
		t.Fatalf("chart A after fresh registration: %v", err)
	}
	if cA.Patient.ID != d.pa || len(cA.Encounters) != 3 || len(cA.Records) != 5 ||
		len(cA.AuditEvents) != 8 {
		t.Fatalf("A chart changed after registering a third patient: %+v", cA)
	}
}

// ---- 接收方即使持有效内容授权也不能走 Chart：ErrAccessDenied，结果为空 ----

func TestChartDeniedToReceiverEvenWithValidContentGrant(t *testing.T) {
	s, _, d := chartIsoFixture(t)

	// rcv 持有覆盖甲 aTie1 诊断的有效授权（窗口未到期、未撤回）。
	auths, err := s.ListAuthorizations(doc, d.pa, rcv.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(auths) != 1 || !auths[0].ActiveAt(s.now()) {
		t.Fatalf("test setup should leave rcv one active content grant for A, got %+v", auths)
	}
	// 对照：该授权确实让接收方在受限入口读到甲的当前生效诊断（内容授权有效）。
	read, err := s.Read(rcv, d.pa, d.aTie1, Diagnosis)
	if err != nil {
		t.Fatalf("receiver Read with a valid content grant should succeed, got %v", err)
	}
	if len(read.Records) != 1 || read.Records[0].RecordID != d.ar1 ||
		read.Records[0].Version != 3 || read.Records[0].Content != "共同诊断文字-第三版内容" {
		t.Fatalf("receiver Read should expose ar1 current version: %+v", read.Records)
	}

	// 但完整档案入口对接收方关闭：即便持有效内容授权也返回 ErrAccessDenied，
	// 结果不携带患者信息、草稿、历史版本或审计。
	denied, err := s.Chart(rcv, d.pa)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver Chart err = %v, want ErrAccessDenied", err)
	}
	if !reflect.DeepEqual(denied, PatientChart{}) {
		t.Fatalf("denied chart must carry no patient info, drafts, history or audit:\n %+v", denied)
	}

	// AuditEvents 走同一查询入口，同样拒绝且不携带事件。
	evs, err := s.AuditEvents(rcv, d.pa)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver AuditEvents err = %v, want ErrAccessDenied", err)
	}
	if evs != nil {
		t.Fatalf("denied audit query must return no events, got %+v", evs)
	}

	// 没有任何授权的接收方、以及无效身份同样被拒且结果为空。
	for _, actor := range []Actor{ReceiverActor("rcv-no-grant"), Actor{ID: "x", Kind: "other"}, InternalActor("")} {
		got, err := s.Chart(actor, d.pa)
		if !errors.Is(err, ErrAccessDenied) {
			t.Fatalf("actor %+v Chart err = %v, want ErrAccessDenied", actor, err)
		}
		if !reflect.DeepEqual(got, PatientChart{}) {
			t.Fatalf("actor %+v denied chart leaked content: %+v", actor, got)
		}
	}

	// 被拒查询不改变审计。
	if got := len(mustAudit(t, s, d.pa)); got != 8 {
		t.Fatalf("denied chart queries changed A audit count: %d", got)
	}
}
