package clinical

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

// 本文件为内部使用者按患者查看完整档案的 Chart 功能补“多患者同库”归属回归
// 保障：同一存储中存在两名（及以上）合成患者，且二人都已有多次就诊、诊断与
// 医嘱草稿、经过更正的生效记录与交错的审计操作时，Chart 的汇总范围只能由
// “所查患者”决定——不能因为另一人的资料更多、文字相同、就诊时间相同或操作
// 交错而漏掉本人的记录、混入他人的就诊/草稿/版本/审计。
//
// 覆盖的约定：
//   - 查到的档案信息、全部就诊、全部记录（含各自所有就诊下的草稿与完整版本
//     历史）只属于所查患者；另一人的就诊、仍为草稿的内容、版本内容与审计
//     事件一律不进入结果，相同文字、相同时间戳也不改变归属；
//   - 未生效草稿保留当前草稿内容，不出现生效版本；已更正记录带当前版本与
//     从旧到新的完整历史，原内容、PrevID 版本关系与更正原因仍挂在原记录上；
//   - 就诊按发生时间排列，时间相同时按就诊标识排列；记录按记录标识稳定排列；
//   - 审计只保留所查患者的事件，保持真实追加次序以及操作身份、动作、对象、
//     时间；他人操作穿插其间不会打乱或污染；
//   - 换另一名内部使用者查询，范围仍由患者决定，而不是只剩自己创建的材料；
//   - 查询本身是只读操作：不新增审计、不改变档案；患者停用后内部使用者仍可
//     取得其原有草稿与完整历史；
//   - 刚登记、没有任何业务操作的患者返回本人档案与空的相关集合，不用他人
//     材料补齐；
//   - 接收方即使持有所查患者的有效内容授权，Chart 仍返回 ErrAccessDenied，
//     且结果不携带患者信息、草稿、历史版本或审计。

// ---- 小工具（显式带操作人，避免复用固定 doc 的辅助函数） ----

func activateAs(t *testing.T, s *Store, actor Actor, rid ID) Version {
	t.Helper()
	v, err := s.ActivateRecord(actor, rid)
	if err != nil {
		t.Fatalf("activate %s as %s: %v", rid, actor.ID, err)
	}
	return v
}

func correctAs(t *testing.T, s *Store, actor Actor, rid ID, expected int, content, reason string) Version {
	t.Helper()
	v, err := s.CorrectRecord(actor, rid, expected, content, reason)
	if err != nil {
		t.Fatalf("correct %s at v%d as %s: %v", rid, expected, actor.ID, err)
	}
	return v
}

func grantAs(t *testing.T, s *Store, actor Actor, pid ID, receiver string, scopes []Scope, start, end time.Time) Authorization {
	t.Helper()
	a, err := s.Grant(actor, pid, receiver, scopes, start, end)
	if err != nil {
		t.Fatalf("grant for %s as %s: %v", pid, actor.ID, err)
	}
	return a
}

type expectedAudit struct {
	action     string
	objectType string
	objectID   ID
	actorID    string
	at         time.Time
}

// assertAuditSequence 逐位置核对审计事件：追加次序、动作、对象类型/标识、
// 操作身份、发生时间与所属患者，全部必须与真实操作一一对应。
func assertAuditSequence(t *testing.T, got []AuditEvent, pid ID, want []expectedAudit, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: audit count = %d (%v), want %d", label, len(got), auditIDList(got), len(want))
	}
	for i, w := range want {
		ev := got[i]
		if ev.PatientID != pid {
			t.Fatalf("%s event %d belongs to patient %q, want %q: %+v", label, i, ev.PatientID, pid, ev)
		}
		if ev.Action != w.action || ev.ObjectType != w.objectType || ev.ObjectID != w.objectID ||
			ev.ActorID != w.actorID {
			t.Fatalf("%s event %d:\n got ={action:%s object:%s/%s actor:%s}\nwant={action:%s object:%s/%s actor:%s}",
				label, i, ev.Action, ev.ObjectType, ev.ObjectID, ev.ActorID,
				w.action, w.objectType, w.objectID, w.actorID)
		}
		if !ev.OccurredAt.Equal(w.at) {
			t.Fatalf("%s event %d time = %v, want %v", label, i, ev.OccurredAt, w.at)
		}
		if ev.ID == "" {
			t.Fatalf("%s event %d has empty id", label, i)
		}
		if i > 0 && ev.ID == got[i-1].ID {
			t.Fatalf("%s event %d reuses previous event id %q", label, i, ev.ID)
		}
	}
}

// assertEncounterOrder 核对就诊按（发生时间, 就诊标识）排列。
func assertEncounterOrder(t *testing.T, got []Encounter, label string) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if prev.OccurredAt.After(cur.OccurredAt) ||
			(prev.OccurredAt.Equal(cur.OccurredAt) && prev.ID > cur.ID) {
			t.Fatalf("%s: encounters not ordered by (occurred_at, id) at %d: %+v before %+v",
				label, i, prev, cur)
		}
	}
}

// assertRecordOrder 核对记录按记录标识稳定排列。
func assertRecordOrder(t *testing.T, hs []RecordHistory, label string) {
	t.Helper()
	for i := 1; i < len(hs); i++ {
		if hs[i-1].Record.ID > hs[i].Record.ID {
			t.Fatalf("%s: records not ordered by id: %q before %q",
				label, hs[i-1].Record.ID, hs[i].Record.ID)
		}
	}
}

func sortedRecordIDs(hs []RecordHistory) []ID {
	ids := recordIDList(hs)
	sort.Strings(ids)
	return ids
}

func idSet(ids ...ID) map[ID]bool {
	m := make(map[ID]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// ---- 双患者交错场景：归属、形态、排序、审计与只读约定 ----

func TestChartScopedToPatientWithInterleavedDualPatients(t *testing.T) {
	s, clk := newTestStore(t)
	t0 := clk.t // 2026-01-01 09:00 UTC

	// 两名患者由不同内部使用者登记。
	pa, err := s.RegisterPatient(doc, "合成患者甲")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := s.RegisterPatient(doc2, "合成患者乙")
	if err != nil {
		t.Fatal(err)
	}

	// 两名患者的第一次就诊发生在完全相同的时刻。
	a1, err := s.AddEncounter(doc, pa.ID, t0)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := s.AddEncounter(doc2, pb.ID, t0)
	if err != nil {
		t.Fatal(err)
	}

	// 乙的数据更多：除了两次与甲同刻的就诊外还多一次就诊（乙的两次就诊
	// 同样发生在同一时刻），用来保证“他人资料更多”不会挤丢甲的内容。
	clk.t = t0.Add(time.Hour)
	t1 := clk.t
	a2, err := s.AddEncounter(doc, pa.ID, t1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s.AddEncounter(doc2, pb.ID, t1)
	if err != nil {
		t.Fatal(err)
	}
	b3, err := s.AddEncounter(doc2, pb.ID, t1)
	if err != nil {
		t.Fatal(err)
	}

	// 两人使用完全相同的文字（草稿、版本内容与更正原因）。
	const (
		sharedDraftText = "相同文字：头痛待查（草稿）"
		sharedDxV1Text  = "相同文字：上呼吸道感染 J06"
		sharedDxV2Text  = "相同文字：上呼吸道感染 J06.9"
		sharedOrdV1Text = "相同文字：血常规 CBC"
		sharedOrdV2Text = "相同文字：血常规+CRP"
		sharedReason    = "相同文字：复核补录"
		aDxV3Text       = "甲专有：急性鼻咽炎 J00（复核）"
		bExtraV1Text    = "乙专有第三就诊：维生素 D 缺乏 E55"
		bExtraV2Text    = "乙专有第三就诊：维生素 D 缺乏 E55.9"
		bExtraV3Text    = "乙专有第三就诊：维生素 D 缺乏（已复核）"
	)

	// 草稿（不产生审计）：甲、乙各保留一条永不生效的诊断草稿，文字相同。
	aDraftDx, err := s.CreateDraft(doc, pa.ID, a1.ID, Diagnosis, sharedDraftText)
	if err != nil {
		t.Fatal(err)
	}
	bDraftDx, err := s.CreateDraft(doc2, pb.ID, b1.ID, Diagnosis, sharedDraftText)
	if err != nil {
		t.Fatal(err)
	}
	// 待更正的诊断（第一次就诊）与医嘱（第二次就诊）。
	aDx, err := s.CreateDraft(doc, pa.ID, a1.ID, Diagnosis, sharedDxV1Text)
	if err != nil {
		t.Fatal(err)
	}
	bDx, err := s.CreateDraft(doc2, pb.ID, b1.ID, Diagnosis, sharedDxV1Text)
	if err != nil {
		t.Fatal(err)
	}
	aOrd, err := s.CreateDraft(doc, pa.ID, a2.ID, Order, sharedOrdV1Text)
	if err != nil {
		t.Fatal(err)
	}
	bOrd, err := s.CreateDraft(doc2, pb.ID, b2.ID, Order, sharedOrdV1Text)
	if err != nil {
		t.Fatal(err)
	}
	// 乙在第三次就诊下还有一条额外记录并被更正两次——纯属乙的资料。
	bExtra, err := s.CreateDraft(doc2, pb.ID, b3.ID, Diagnosis, bExtraV1Text)
	if err != nil {
		t.Fatal(err)
	}

	// 随后的操作严格交错，并刻意让两名内部使用者互相操作对方登记的患者；
	// 同一名患者也会在同一时刻留下多个事件（中间夹着另一人的操作），使
	// “同刻按真实追加次序”在跨患者场景下同样被覆盖。全局审计追加次序如下
	// （左列序号即真实操作次序）。
	clk.t = t1.Add(time.Hour)
	t2 := clk.t
	//  1 甲: doc  生效诊断      t2
	aDxV1 := activateAs(t, s, doc, aDx.ID)
	//  2 乙: doc2 生效诊断      t2
	bDxV1 := activateAs(t, s, doc2, bDx.ID)
	//  3 甲: doc2 生效医嘱      t2（与 1 同刻，中间夹着乙的操作；操作人也不同）
	aOrdV1 := activateAs(t, s, doc2, aOrd.ID)

	clk.t = t2.Add(time.Hour)
	t3 := clk.t
	//  4 乙: doc2 生效医嘱      t3
	bOrdV1 := activateAs(t, s, doc2, bOrd.ID)
	//  5 甲: doc2 更正诊断 ->v2 t3
	aDxV2 := correctAs(t, s, doc2, aDx.ID, 1, sharedDxV2Text, sharedReason)
	//  6 乙: doc  更正诊断 ->v2 t3（与 5 同刻，甲乙事件交错）
	bDxV2 := correctAs(t, s, doc, bDx.ID, 1, sharedDxV2Text, sharedReason)
	//  7 甲: doc  更正医嘱 ->v2 t3（与 5 同刻，甲在同一时刻有两条更正）
	aOrdV2 := correctAs(t, s, doc, aOrd.ID, 1, sharedOrdV2Text, sharedReason)

	clk.t = t3.Add(time.Hour)
	t4 := clk.t
	//  8 乙: doc2 生效额外诊断  t4（乙比甲多的事件）
	bExtraV1 := activateAs(t, s, doc2, bExtra.ID)

	clk.t = t4.Add(time.Hour)
	t5 := clk.t
	//  9 乙: doc  更正医嘱 ->v2 t5
	bOrdV2 := correctAs(t, s, doc, bOrd.ID, 1, sharedOrdV2Text, sharedReason)
	// 10 乙: doc  更正额外 ->v2 t5（乙在同一时刻有两条更正，追加次序须保留）
	bExtraV2 := correctAs(t, s, doc, bExtra.ID, 1, bExtraV2Text, "乙专有：复查补项")

	clk.t = t5.Add(time.Hour)
	t6 := clk.t
	// 11 甲: doc  更正诊断 ->v3 t6
	aDxV3 := correctAs(t, s, doc, aDx.ID, 2, aDxV3Text, "甲专有：门诊复核")
	// 12 乙: doc2 更正额外 ->v3 t6（与 11 同刻）
	bExtraV3 := correctAs(t, s, doc2, bExtra.ID, 2, bExtraV3Text, "乙专有：再次复核")

	// 13/14 两名内部使用者分别给同一接收方建立覆盖各患者内容的有效授权（同刻）。
	clk.t = t6.Add(time.Hour)
	t7 := clk.t
	grantA := grantAs(t, s, doc, pa.ID, rcv.ID,
		[]Scope{{EncounterID: a1.ID, Category: Diagnosis}, {EncounterID: a2.ID, Category: Order}},
		t7.Add(-time.Hour), t7.Add(48*time.Hour))
	grantB := grantAs(t, s, doc2, pb.ID, rcv.ID,
		[]Scope{{EncounterID: b1.ID, Category: Diagnosis}, {EncounterID: b2.ID, Category: Order}},
		t7.Add(-time.Hour), t7.Add(48*time.Hour))

	// 预期审计序列（编号对应上面注释中的全局真实操作次序）。
	wantAAudits := []expectedAudit{
		{ActionActivated, "record", aDx.ID, doc.ID, t2},         // 1
		{ActionActivated, "record", aOrd.ID, doc2.ID, t2},       // 3
		{ActionCorrected, "record", aDx.ID, doc2.ID, t3},        // 5
		{ActionCorrected, "record", aOrd.ID, doc.ID, t3},        // 7
		{ActionCorrected, "record", aDx.ID, doc.ID, t6},         // 11
		{ActionGranted, "authorization", grantA.ID, doc.ID, t7}, // 13
	}
	wantBAudits := []expectedAudit{
		{ActionActivated, "record", bDx.ID, doc2.ID, t2},         // 2
		{ActionActivated, "record", bOrd.ID, doc2.ID, t3},        // 4
		{ActionCorrected, "record", bDx.ID, doc.ID, t3},          // 6
		{ActionActivated, "record", bExtra.ID, doc2.ID, t4},      // 8
		{ActionCorrected, "record", bOrd.ID, doc.ID, t5},         // 9
		{ActionCorrected, "record", bExtra.ID, doc.ID, t5},       // 10
		{ActionCorrected, "record", bExtra.ID, doc2.ID, t6},      // 12
		{ActionGranted, "authorization", grantB.ID, doc2.ID, t7}, // 14
	}

	// ---- 查甲：由登记人 doc 查询 ----

	chartA, err := s.Chart(doc, pa.ID)
	if err != nil {
		t.Fatalf("chart A as doc: %v", err)
	}
	if chartA.Patient.ID != pa.ID || chartA.Patient.Name != "合成患者甲" ||
		chartA.Patient.Source != SyntheticSource || chartA.Patient.Deactivated {
		t.Fatalf("chart A patient header wrong: %+v", chartA.Patient)
	}

	// 就诊：甲的两次，按（时间, 标识）排列；乙的三次一次都不能出现。
	if len(chartA.Encounters) != 2 {
		t.Fatalf("chart A encounters = %d, want 2: %+v", len(chartA.Encounters), chartA.Encounters)
	}
	assertEncounterOrder(t, chartA.Encounters, "chart A encounters")
	gotAEncIDs := map[ID]bool{}
	for _, e := range chartA.Encounters {
		if e.PatientID != pa.ID {
			t.Fatalf("chart A contains encounter owned by %q: %+v", e.PatientID, e)
		}
		gotAEncIDs[e.ID] = true
	}
	if !gotAEncIDs[a1.ID] || !gotAEncIDs[a2.ID] || gotAEncIDs[b1.ID] || gotAEncIDs[b2.ID] || gotAEncIDs[b3.ID] {
		t.Fatalf("chart A encounter ownership wrong: %v", idList(chartA.Encounters))
	}
	// 同刻就诊对确实存在，排序规则被真实覆盖。
	if !chartA.Encounters[0].OccurredAt.Equal(t0) {
		t.Fatalf("chart A first encounter time = %v, want %v", chartA.Encounters[0].OccurredAt, t0)
	}

	// 记录：甲的三条（两条就诊都要有），按记录标识稳定排列。
	if len(chartA.Records) != 3 {
		t.Fatalf("chart A records = %d, want 3: %v", len(chartA.Records), recordIDList(chartA.Records))
	}
	assertRecordOrder(t, chartA.Records, "chart A records")
	if got, want := recordIDList(chartA.Records), sortedRecordIDs(chartA.Records); !reflect.DeepEqual(got, want) {
		t.Fatalf("chart A record order = %v, want sorted %v", got, want)
	}
	aRecs := historiesByID(chartA.Records)
	for _, h := range chartA.Records {
		if h.Record.PatientID != pa.ID || !gotAEncIDs[h.Record.EncounterID] {
			t.Fatalf("chart A record with wrong ownership: %+v", h.Record)
		}
	}
	if _, ok := aRecs[aDraftDx.ID]; !ok {
		t.Fatal("chart A missing its draft diagnosis")
	}
	if _, ok := aRecs[aDx.ID]; !ok {
		t.Fatal("chart A missing its corrected diagnosis")
	}
	if _, ok := aRecs[aOrd.ID]; !ok {
		t.Fatal("chart A missing its order from the second encounter")
	}
	for _, leaked := range []ID{bDraftDx.ID, bDx.ID, bOrd.ID, bExtra.ID} {
		if _, ok := aRecs[leaked]; ok {
			t.Fatalf("patient B record %q leaked into chart A", leaked)
		}
	}

	// 未生效草稿：保留当前草稿内容，没有当前版本、没有任何历史版本，
	// 相同文字不能让乙的同名草稿混进来。
	ad := aRecs[aDraftDx.ID]
	if !ad.HasDraft || ad.DraftContent != sharedDraftText {
		t.Fatalf("chart A draft content wrong: HasDraft=%v Content=%q", ad.HasDraft, ad.DraftContent)
	}
	if ad.CurrentVersion != nil || ad.Record.CurrentVersionID != "" ||
		len(ad.Record.Versions) != 0 || len(ad.Versions) != 0 {
		t.Fatalf("ineffective draft must carry no effective version: %+v", ad)
	}

	// 已更正诊断：当前 v3，完整历史 v1->v2->v3，旧内容、PrevID 链与原因保留。
	adx := aRecs[aDx.ID]
	if adx.HasDraft || adx.DraftContent != "" {
		t.Fatalf("effective diagnosis must not present a draft: %+v", adx.Record)
	}
	if adx.CurrentVersion == nil || adx.CurrentVersion.ID != aDxV3.ID ||
		adx.CurrentVersion.Number != 3 || adx.CurrentVersion.Content != aDxV3Text {
		t.Fatalf("chart A diagnosis current version wrong: %+v", adx.CurrentVersion)
	}
	if wantIDs := []ID{aDxV1.ID, aDxV2.ID, aDxV3.ID}; !reflect.DeepEqual(adx.Record.Versions, wantIDs) {
		t.Fatalf("chart A diagnosis version id chain = %v, want %v", adx.Record.Versions, wantIDs)
	}
	if len(adx.Versions) != 3 {
		t.Fatalf("chart A diagnosis history len = %d, want 3", len(adx.Versions))
	}
	if adx.Versions[0].Content != sharedDxV1Text || adx.Versions[0].PrevID != "" || adx.Versions[0].Reason != "" {
		t.Fatalf("chart A diagnosis v1 not preserved: %+v", adx.Versions[0])
	}
	if adx.Versions[1].ID != aDxV2.ID || adx.Versions[1].PrevID != aDxV1.ID ||
		adx.Versions[1].Content != sharedDxV2Text || adx.Versions[1].Reason != sharedReason {
		t.Fatalf("chart A diagnosis v2 chain/reason wrong: %+v", adx.Versions[1])
	}
	if adx.Versions[2].ID != aDxV3.ID || adx.Versions[2].PrevID != aDxV2.ID ||
		adx.Versions[2].Content != aDxV3Text || adx.Versions[2].Reason != "甲专有：门诊复核" {
		t.Fatalf("chart A diagnosis v3 chain/reason wrong: %+v", adx.Versions[2])
	}
	for _, v := range adx.Versions {
		if v.RecordID != aDx.ID {
			t.Fatalf("version %+v detached from its own record %q", v, aDx.ID)
		}
	}

	// 已更正医嘱：当前 v2 与完整两版历史，且来自甲的第二次就诊。
	aord := aRecs[aOrd.ID]
	if aord.Record.EncounterID != a2.ID {
		t.Fatalf("chart A order must come from encounter A2: %+v", aord.Record)
	}
	if aord.CurrentVersion == nil || aord.CurrentVersion.ID != aOrdV2.ID || aord.CurrentVersion.Number != 2 {
		t.Fatalf("chart A order current version wrong: %+v", aord.CurrentVersion)
	}
	if len(aord.Versions) != 2 ||
		aord.Versions[0].ID != aOrdV1.ID || aord.Versions[0].Content != sharedOrdV1Text ||
		aord.Versions[1].ID != aOrdV2.ID || aord.Versions[1].PrevID != aOrdV1.ID ||
		aord.Versions[1].Content != sharedOrdV2Text || aord.Versions[1].Reason != sharedReason {
		t.Fatalf("chart A order history wrong: %+v", aord.Versions)
	}

	// 审计：只保留甲的 6 个事件，乙的 8 个穿插事件不得进入，相同时间戳下
	// 仍保持真实追加次序、身份、动作、对象与时间。
	assertAuditSequence(t, chartA.AuditEvents, pa.ID, wantAAudits, "chart A audits")

	// 任何乙专有的标识与文字都不能在甲的结果中出现。
	bVersionIDs := idSet(bDxV1.ID, bDxV2.ID, bOrdV1.ID, bOrdV2.ID, bExtraV1.ID, bExtraV2.ID, bExtraV3.ID)
	bObjectIDs := idSet(bDraftDx.ID, bDx.ID, bOrd.ID, bExtra.ID, b1.ID, b2.ID, b3.ID, grantB.ID, pb.ID)
	for _, h := range chartA.Records {
		for _, vid := range h.Record.Versions {
			if bVersionIDs[vid] {
				t.Fatalf("patient B version %q leaked into chart A record %q", vid, h.Record.ID)
			}
		}
		for _, v := range h.Versions {
			if v.RecordID != h.Record.ID || bVersionIDs[v.ID] {
				t.Fatalf("chart A version ownership wrong: %+v", v)
			}
			if v.Content == bExtraV1Text || v.Content == bExtraV2Text || v.Content == bExtraV3Text {
				t.Fatalf("patient B exclusive content leaked into chart A: %q", v.Content)
			}
		}
	}
	for _, ev := range chartA.AuditEvents {
		if bObjectIDs[ev.ObjectID] {
			t.Fatalf("patient B audit object %q leaked into chart A audit view", ev.ObjectID)
		}
	}

	// ---- 查乙：由另一人 doc 查询，且乙由 doc2 登记、资料更多 ----

	chartB, err := s.Chart(doc, pb.ID)
	if err != nil {
		t.Fatalf("chart B as doc: %v", err)
	}
	if chartB.Patient.ID != pb.ID || chartB.Patient.Name != "合成患者乙" ||
		chartB.Patient.Source != SyntheticSource {
		t.Fatalf("chart B patient header wrong: %+v", chartB.Patient)
	}
	if len(chartB.Encounters) != 3 {
		t.Fatalf("chart B encounters = %d, want 3: %+v", len(chartB.Encounters), chartB.Encounters)
	}
	assertEncounterOrder(t, chartB.Encounters, "chart B encounters")
	gotEncBInOrder := make([]ID, len(chartB.Encounters))
	for i, e := range chartB.Encounters {
		if e.PatientID != pb.ID {
			t.Fatalf("chart B encounter owned by %q: %+v", e.PatientID, e)
		}
		gotEncBInOrder[i] = e.ID
	}
	// 期望顺序：同刻的 b2/b3 必须按标识排列。
	wantEncOrder := []ID{b1.ID}
	switch {
	case b2.ID < b3.ID:
		wantEncOrder = append(wantEncOrder, b2.ID, b3.ID)
	default:
		wantEncOrder = append(wantEncOrder, b3.ID, b2.ID)
	}
	if !reflect.DeepEqual(gotEncBInOrder, wantEncOrder) {
		t.Fatalf("chart B encounter order = %v, want %v (same-time tie by id)", gotEncBInOrder, wantEncOrder)
	}

	if len(chartB.Records) != 4 {
		t.Fatalf("chart B records = %d, want 4: %v", len(chartB.Records), recordIDList(chartB.Records))
	}
	assertRecordOrder(t, chartB.Records, "chart B records")
	bRecs := historiesByID(chartB.Records)
	for _, rid := range []ID{bDraftDx.ID, bDx.ID, bOrd.ID, bExtra.ID} {
		if _, ok := bRecs[rid]; !ok {
			t.Fatalf("chart B missing own record %q: %v", rid, recordIDList(chartB.Records))
		}
	}
	for _, leaked := range []ID{aDraftDx.ID, aDx.ID, aOrd.ID} {
		if _, ok := bRecs[leaked]; ok {
			t.Fatalf("patient A record %q leaked into chart B", leaked)
		}
	}
	// 乙的草稿仍是草稿（文字与甲相同也不串台）。
	bd := bRecs[bDraftDx.ID]
	if !bd.HasDraft || bd.DraftContent != sharedDraftText || bd.CurrentVersion != nil {
		t.Fatalf("chart B draft shape wrong: %+v", bd)
	}
	// 乙的额外记录：三次版本完整。
	bex := bRecs[bExtra.ID]
	if bex.Record.EncounterID != b3.ID {
		t.Fatalf("chart B extra record must stay in encounter B3: %+v", bex.Record)
	}
	if bex.CurrentVersion == nil || bex.CurrentVersion.ID != bExtraV3.ID || bex.CurrentVersion.Number != 3 {
		t.Fatalf("chart B extra current version wrong: %+v", bex.CurrentVersion)
	}
	if len(bex.Versions) != 3 ||
		bex.Versions[0].ID != bExtraV1.ID || bex.Versions[0].PrevID == bDxV1.ID ||
		bex.Versions[1].PrevID != bExtraV1.ID || bex.Versions[2].PrevID != bExtraV2.ID {
		t.Fatalf("chart B extra version chain wrong: %+v", bex.Versions)
	}
	if bex.Versions[0].Content != bExtraV1Text || bex.Versions[2].Content != bExtraV3Text {
		t.Fatalf("chart B extra version contents wrong: %+v", bex.Versions)
	}
	// 乙的诊断同样只有自己的两版历史；相同文字不共享甲的 v3。
	bdx := bRecs[bDx.ID]
	if bdx.CurrentVersion == nil || bdx.CurrentVersion.Number != 2 || bdx.CurrentVersion.ID != bDxV2.ID {
		t.Fatalf("chart B diagnosis current version wrong: %+v", bdx.CurrentVersion)
	}
	if len(bdx.Versions) != 2 || bdx.Versions[1].PrevID != bDxV1.ID {
		t.Fatalf("chart B diagnosis history wrong: %+v", bdx.Versions)
	}
	if bdx.CurrentVersion.Content == aDxV3Text {
		t.Fatal("chart B diagnosis picked up patient A's v3 text through shared wording")
	}
	assertAuditSequence(t, chartB.AuditEvents, pb.ID, wantBAudits, "chart B audits")

	// ---- 换另一名有效内部使用者：范围仍由患者决定，不退化为“只看自己创建的” ----

	chartAByOther, err := s.Chart(doc2, pa.ID)
	if err != nil {
		t.Fatalf("chart A as doc2: %v", err)
	}
	if !reflect.DeepEqual(chartAByOther, chartA) {
		t.Fatalf("chart A depends on querying internal actor:\n doc  =%+v\n doc2 =%+v",
			chartA, chartAByOther)
	}
	chartBByCreator, err := s.Chart(doc2, pb.ID)
	if err != nil {
		t.Fatalf("chart B as doc2: %v", err)
	}
	if !reflect.DeepEqual(chartBByCreator, chartB) {
		t.Fatalf("chart B depends on querying internal actor:\n doc  =%+v\n doc2 =%+v",
			chartB, chartBByCreator)
	}
	// 特别强调：doc2 查询甲时仍能看到 doc 登记的草稿与 doc 做的生效事件；
	// doc 查询乙时也能看到 doc2 登记的全部材料。
	if h := historiesByID(chartAByOther.Records)[aDraftDx.ID]; !h.HasDraft || h.DraftContent != sharedDraftText {
		t.Fatal("non-creator internal actor lost another actor's draft")
	}
	if evs := chartBByCreator.AuditEvents; evs[0].ActorID != doc2.ID {
		t.Fatalf("chart B first audit actor = %q, want the real operator %q", evs[0].ActorID, doc2.ID)
	}

	// ---- 查询是只读操作：不新增审计、不改变档案 ----

	baselineA := snapshotAudit(chartA.AuditEvents)
	baselineB := snapshotAudit(chartB.AuditEvents)
	for i := 0; i < 3; i++ {
		if _, err := s.Chart(doc, pa.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Chart(doc2, pb.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AuditEvents(doc2, pa.ID); err != nil {
			t.Fatal(err)
		}
	}
	assertAuditMatches(t, mustAudit(t, s, pa.ID), baselineA, "chart A audits after repeated queries")
	assertAuditMatches(t, mustAudit(t, s, pb.ID), baselineB, "chart B audits after repeated queries")
	againA, err := s.Chart(doc, pa.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(againA, chartA) {
		t.Fatal("chart A changed as a result of querying")
	}
	if againA.Patient.Deactivated {
		t.Fatal("query flipped patient deactivation flag")
	}

	// ---- 接收方持有效内容授权也不能通过 Chart 取完整档案 ----

	// 先证明授权对接收方确实有效：Read 能取到甲诊断的当前生效版本 v3。
	readRes, err := s.Read(rcv, pa.ID, a1.ID, Diagnosis)
	if err != nil {
		t.Fatalf("receiver Read with valid grant should succeed: %v", err)
	}
	if len(readRes.Records) != 1 || readRes.Records[0].RecordID != aDx.ID ||
		readRes.Records[0].Version != 3 || readRes.Records[0].Content != aDxV3Text {
		t.Fatalf("receiver Read result wrong, grant may not be effective: %+v", readRes)
	}
	// 但 Chart 与 AuditEvents 一律拒绝，且结果不携带任何受保护内容。
	denied, err := s.Chart(rcv, pa.ID)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver Chart err = %v, want ErrAccessDenied", err)
	}
	if denied.Patient != (Patient{}) || denied.Encounters != nil || denied.Records != nil || denied.AuditEvents != nil {
		t.Fatalf("denied chart must carry no patient info, drafts, history or audits, got %+v", denied)
	}
	deniedEvents, err := s.AuditEvents(rcv, pb.ID)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver AuditEvents err = %v, want ErrAccessDenied", err)
	}
	if deniedEvents != nil {
		t.Fatalf("denied audit query must carry no events, got %+v", deniedEvents)
	}
	// 被拒绝的查询同样不留下审计。
	assertAuditMatches(t, mustAudit(t, s, pa.ID), baselineA, "chart A audits after denied receiver query")

	// ---- 刚登记、没有任何业务操作的患者：返回本人档案与空集合 ----

	pc, err := s.RegisterPatient(doc2, "合成患者丙")
	if err != nil {
		t.Fatal(err)
	}
	chartC, err := s.Chart(doc, pc.ID)
	if err != nil {
		t.Fatalf("chart C: %v", err)
	}
	if chartC.Patient.ID != pc.ID || chartC.Patient.Name != "合成患者丙" ||
		chartC.Patient.Source != SyntheticSource || chartC.Patient.Deactivated {
		t.Fatalf("chart C patient header wrong: %+v", chartC.Patient)
	}
	if len(chartC.Encounters) != 0 || len(chartC.Records) != 0 || len(chartC.AuditEvents) != 0 {
		t.Fatalf("fresh patient must have empty collections, got encounters=%d records=%d audits=%d",
			len(chartC.Encounters), len(chartC.Records), len(chartC.AuditEvents))
	}
	// 不能借用别人的材料补齐：所有甲乙标识均不得出现。
	for _, e := range chartC.Encounters {
		t.Fatalf("fresh patient chart leaked encounter: %+v", e)
	}
	for _, h := range chartC.Records {
		t.Fatalf("fresh patient chart leaked record: %+v", h.Record)
	}
	// 换接收方查空档案同样被拒绝且不泄露患者是否存在之外的任何信息。
	if _, err := s.Chart(rcv, pc.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver chart on fresh patient err = %v, want ErrAccessDenied", err)
	}

	// ---- 患者停用后：内部使用者仍取得原有草稿与完整历史 ----

	clk.t = t7.Add(time.Hour)
	t8 := clk.t
	// 由并非登记人的 doc 停用乙（乙由 doc2 登记）。
	if err := s.DeactivatePatient(doc, pb.ID); err != nil {
		t.Fatalf("deactivate B: %v", err)
	}
	chartBAfter, err := s.Chart(doc2, pb.ID)
	if err != nil {
		t.Fatalf("chart B after deactivation: %v", err)
	}
	if !chartBAfter.Patient.Deactivated {
		t.Fatal("deactivated flag missing from chart")
	}
	// 草稿原样保留。
	if h := historiesByID(chartBAfter.Records)[bDraftDx.ID]; !h.HasDraft || h.DraftContent != sharedDraftText ||
		h.CurrentVersion != nil {
		t.Fatalf("draft lost after deactivation: %+v", h)
	}
	// 完整版本历史一条不缺（乙原有 3+2+2 = 7 个版本，分布在 3 条生效记录上）。
	if len(chartBAfter.Records) != 4 || len(chartBAfter.Encounters) != 3 {
		t.Fatalf("deactivated chart lost material: encounters=%d records=%d",
			len(chartBAfter.Encounters), len(chartBAfter.Records))
	}
	bexAfter := historiesByID(chartBAfter.Records)[bExtra.ID]
	if len(bexAfter.Versions) != 3 || bexAfter.CurrentVersion.ID != bExtraV3.ID {
		t.Fatalf("deactivated chart history incomplete: %+v", bexAfter)
	}
	// 审计只在乙的序列末尾追加停用事件；甲不受影响。
	wantBAuditsAfter := append(snapshotExpectedAudits(wantBAudits),
		expectedAudit{ActionDeactivated, "patient", pb.ID, doc.ID, t8})
	assertAuditSequence(t, chartBAfter.AuditEvents, pb.ID, wantBAuditsAfter, "chart B audits after deactivation")
	assertAuditMatches(t, mustAudit(t, s, pa.ID), baselineA, "chart A audits unaffected by B deactivation")
	// 停用后接收方仍不能通过 Chart 取乙的完整档案。
	if _, err := s.Chart(rcv, pb.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver chart on deactivated patient err = %v, want ErrAccessDenied", err)
	}
}

// snapshotExpectedAudits 复制期望审计序列，避免 append 复用底层数组改动原切片。
func snapshotExpectedAudits(in []expectedAudit) []expectedAudit {
	out := make([]expectedAudit, len(in))
	copy(out, in)
	return out
}

// idList 列出就诊标识，用于失败信息。
func idList(es []Encounter) []ID {
	ids := make([]ID, len(es))
	for i, e := range es {
		ids[i] = e.ID
	}
	return ids
}

// ---- 关闭后从同一目录重开：跨患者归属、排序与审计次序在 JSON 恢复后不变 ----

func TestChartPatientScopingSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	pa, err := s.RegisterPatient(doc, "重开患者甲")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := s.RegisterPatient(doc2, "重开患者乙")
	if err != nil {
		t.Fatal(err)
	}
	at := clk.t
	// 两名患者各有两次同刻就诊。
	a1, err := s.AddEncounter(doc, pa.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := s.AddEncounter(doc2, pb.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.AddEncounter(doc, pa.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s.AddEncounter(doc2, pb.ID, at)
	if err != nil {
		t.Fatal(err)
	}

	const shared = "重开后仍须归属正确的相同文字"
	aDraft, err := s.CreateDraft(doc, pa.ID, a1.ID, Diagnosis, shared)
	if err != nil {
		t.Fatal(err)
	}
	bDraft, err := s.CreateDraft(doc2, pb.ID, b1.ID, Diagnosis, shared)
	if err != nil {
		t.Fatal(err)
	}
	aRec, err := s.CreateDraft(doc, pa.ID, a2.ID, Order, shared)
	if err != nil {
		t.Fatal(err)
	}
	bRec, err := s.CreateDraft(doc2, pb.ID, b2.ID, Order, shared)
	if err != nil {
		t.Fatal(err)
	}

	clk.t = at.Add(time.Hour)
	ts := clk.t
	// 同刻交错：甲生效、乙生效、甲更正、乙更正，验证同刻追加次序在重开后仍保留。
	aV1 := activateAs(t, s, doc, aRec.ID)
	bV1 := activateAs(t, s, doc2, bRec.ID)
	clk.t = ts.Add(time.Hour)
	tc := clk.t
	aV2 := correctAs(t, s, doc2, aRec.ID, 1, shared+"（甲更正）", "甲的更正原因")
	bV2 := correctAs(t, s, doc, bRec.ID, 1, shared+"（乙更正）", "乙的更正原因")

	beforeA, err := s.Chart(doc2, pa.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeB, err := s.Chart(doc, pb.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	afterA, err := s2.Chart(doc2, pa.ID)
	if err != nil {
		t.Fatalf("chart A after reopen: %v", err)
	}
	afterB, err := s2.Chart(doc, pb.ID)
	if err != nil {
		t.Fatalf("chart B after reopen: %v", err)
	}
	if !reflect.DeepEqual(afterA, beforeA) {
		t.Fatalf("chart A changed across reopen:\n before=%+v\n after =%+v", beforeA, afterA)
	}
	if !reflect.DeepEqual(afterB, beforeB) {
		t.Fatalf("chart B changed across reopen:\n before=%+v\n after =%+v", beforeB, afterB)
	}

	// 归属在恢复后仍严格成立。
	wantAEnc := []ID{a1.ID, a2.ID}
	wantBEnc := []ID{b1.ID, b2.ID}
	sort.Strings(wantAEnc)
	sort.Strings(wantBEnc)
	if got := idList(afterA.Encounters); !reflect.DeepEqual(got, wantAEnc) {
		t.Fatalf("after reopen chart A encounters = %v, want %v", got, wantAEnc)
	}
	if got := idList(afterB.Encounters); !reflect.DeepEqual(got, wantBEnc) {
		t.Fatalf("after reopen chart B encounters = %v, want %v", got, wantBEnc)
	}
	assertEncounterOrder(t, afterA.Encounters, "reopened chart A encounters")
	assertEncounterOrder(t, afterB.Encounters, "reopened chart B encounters")

	aMap := historiesByID(afterA.Records)
	bMap := historiesByID(afterB.Records)
	if _, ok := aMap[aDraft.ID]; !ok {
		t.Fatal("after reopen chart A lost its draft")
	}
	if _, ok := bMap[bDraft.ID]; !ok {
		t.Fatal("after reopen chart B lost its draft")
	}
	if _, ok := aMap[bRec.ID]; ok {
		t.Fatal("after reopen patient B record leaked into chart A")
	}
	if _, ok := bMap[aRec.ID]; ok {
		t.Fatal("after reopen patient A record leaked into chart B")
	}
	if h := aMap[aRec.ID]; len(h.Versions) != 2 || h.CurrentVersion.ID != aV2.ID ||
		h.Versions[1].PrevID != aV1.ID || h.Versions[1].Reason != "甲的更正原因" {
		t.Fatalf("after reopen chart A history broken: %+v", h.Versions)
	}
	if h := bMap[bRec.ID]; len(h.Versions) != 2 || h.CurrentVersion.ID != bV2.ID ||
		h.Versions[1].PrevID != bV1.ID || h.Versions[1].Reason != "乙的更正原因" {
		t.Fatalf("after reopen chart B history broken: %+v", h.Versions)
	}

	// 同刻交错的审计在重开后仍按真实追加次序、真实操作人归属各患者。
	assertAuditSequence(t, afterA.AuditEvents, pa.ID, []expectedAudit{
		{ActionActivated, "record", aRec.ID, doc.ID, ts},
		{ActionCorrected, "record", aRec.ID, doc2.ID, tc},
	}, "reopened chart A audits")
	assertAuditSequence(t, afterB.AuditEvents, pb.ID, []expectedAudit{
		{ActionActivated, "record", bRec.ID, doc2.ID, ts},
		{ActionCorrected, "record", bRec.ID, doc.ID, tc},
	}, "reopened chart B audits")

	// 接收方重开后依然不能通过 Chart 取档案。
	if _, err := s2.Chart(rcv, pa.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver chart after reopen err = %v, want ErrAccessDenied", err)
	}
}
