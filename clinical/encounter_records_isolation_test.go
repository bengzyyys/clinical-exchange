package clinical

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// 本文件为内部查询 EncounterRecords 的“返回结果隔离”约定补回归保障：
// 内部使用者通过它查看某次就诊时，拿到的草稿与完整历史只是本次查询的独立
// 结果。调用方在手中的结果上整理内容，既不能改掉正式病历，也不能改掉另一
// 份先前取得的查询结果；诊断与医嘱一视同仁。
//
// 预期基线由测试自行深拷贝（刻意不调用生产代码的任何拷贝逻辑）：即便生产
// 侧 buildHistory 退化为浅拷贝，基线仍保持查询时内容，隔离断言才能真正
// 抓住底层数组或版本指针的泄漏。

// ---- 测试自用的独立深拷贝（不经过生产代码） ----

// snapshotHistory 独立深拷贝一条就诊记录视图作为预期基线。
func snapshotHistory(h RecordHistory) RecordHistory {
	cp := h
	cp.Record.Versions = append([]ID(nil), h.Record.Versions...)
	vs := make([]Version, len(h.Versions))
	copy(vs, h.Versions)
	cp.Versions = vs
	if h.CurrentVersion != nil {
		cv := *h.CurrentVersion
		cp.CurrentVersion = &cv
	}
	return cp
}

// snapshotHistories 独立深拷贝整份就诊查询结果。
func snapshotHistories(hs []RecordHistory) []RecordHistory {
	out := make([]RecordHistory, len(hs))
	for i, h := range hs {
		out[i] = snapshotHistory(h)
	}
	return out
}

func historiesByID(hs []RecordHistory) map[ID]RecordHistory {
	m := make(map[ID]RecordHistory, len(hs))
	for _, h := range hs {
		m[h.Record.ID] = h
	}
	return m
}

// tamperHistory 在调用方手中的一条结果上做尽可能广泛的本地修改：
// 记录标识与就诊归属、版本标识列表（就地改写）、草稿内容与草稿标记、
// 当前版本视图的全部字段，以及历史版本的内容/原因/上一版本关系并整体逆序。
func tamperHistory(h *RecordHistory) {
	h.Record.ID = "rec_local_tampered"
	h.Record.PatientID = "pat_local_tampered"
	h.Record.EncounterID = "enc_local_tampered"
	// 就地改写版本标识列表：与正式历史或另一份结果共享底层数组时会污染对方。
	for i := range h.Record.Versions {
		h.Record.Versions[i] = ID(fmt.Sprintf("ver_local_tampered_%d", i))
	}
	h.Record.DraftContent = "调用方本地篡改的草稿内容"
	h.HasDraft = !h.HasDraft // 翻转草稿标记

	if h.CurrentVersion != nil {
		h.CurrentVersion.ID = "ver_cv_local_tampered"
		h.CurrentVersion.RecordID = "rec_local_tampered"
		h.CurrentVersion.Number = 999
		h.CurrentVersion.Content = "调用方本地篡改的当前版本内容"
		h.CurrentVersion.PrevID = "ver_cv_local_prev"
		h.CurrentVersion.Reason = "调用方本地伪造的更正原因"
	}

	if len(h.Versions) > 0 {
		h.Versions[0].ID = "ver_hist_local_tampered"
		h.Versions[0].Content = "调用方本地篡改的历史内容"
		h.Versions[0].Reason = "调用方本地伪造的历史原因"
		h.Versions[0].PrevID = "ver_hist_local_prev"
		// 就地逆序：共享底层数组时会改掉另一份结果的版本顺序。
		for i, j := 0, len(h.Versions)-1; i < j; i, j = i+1, j-1 {
			h.Versions[i], h.Versions[j] = h.Versions[j], h.Versions[i]
		}
	}
}

func assertHistoryMatches(t *testing.T, got, want RecordHistory, label string) {
	t.Helper()
	gr, wr := got.Record, want.Record
	if gr.ID != wr.ID || gr.PatientID != wr.PatientID || gr.EncounterID != wr.EncounterID ||
		gr.Category != wr.Category || gr.DraftContent != wr.DraftContent || gr.HasDraft != wr.HasDraft ||
		gr.CurrentVersionID != wr.CurrentVersionID {
		t.Fatalf("%s: record header changed:\n got=%+v\nwant=%+v", label, gr, wr)
	}
	if !reflect.DeepEqual(gr.Versions, wr.Versions) {
		t.Fatalf("%s: official version id list changed:\n got=%v\nwant=%v", label, gr.Versions, wr.Versions)
	}
	if (got.CurrentVersion == nil) != (want.CurrentVersion == nil) {
		t.Fatalf("%s: current version presence changed: got present=%v, want present=%v",
			label, got.CurrentVersion != nil, want.CurrentVersion != nil)
	}
	if want.CurrentVersion != nil && *got.CurrentVersion != *want.CurrentVersion {
		t.Fatalf("%s: current version changed:\n got=%+v\nwant=%+v",
			label, *got.CurrentVersion, *want.CurrentVersion)
	}
	if len(got.Versions) != len(want.Versions) {
		t.Fatalf("%s: history length changed: got %d versions, want %d versions\ngot=%+v",
			label, len(got.Versions), len(want.Versions), got.Versions)
	}
	for i := range want.Versions {
		if got.Versions[i] != want.Versions[i] {
			t.Fatalf("%s: history version %d changed:\n got=%+v\nwant=%+v",
				label, i, got.Versions[i], want.Versions[i])
		}
	}
}

// assertHistoriesMatches 按位置逐元素比对（两种视图都按记录标识稳定排序）。
func assertHistoriesMatches(t *testing.T, got, want []RecordHistory, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: record count changed: got %d (%v), want %d (%v)",
			label, len(got), recordIDList(got), len(want), recordIDList(want))
	}
	for i := range want {
		assertHistoryMatches(t, got[i], want[i], fmt.Sprintf("%s [%d]", label, i))
	}
}

func recordIDList(hs []RecordHistory) []ID {
	ids := make([]ID, len(hs))
	for i, h := range hs {
		ids[i] = h.Record.ID
	}
	return ids
}

func mustCreateDraft(t *testing.T, s *Store, pid, eid ID, category, content string) ID {
	t.Helper()
	r, err := s.CreateDraft(doc, pid, eid, category, content)
	if err != nil {
		t.Fatalf("create draft %s/%s: %v", eid, category, err)
	}
	return r.ID
}

func mustActivate(t *testing.T, s *Store, rid ID) Version {
	t.Helper()
	v, err := s.ActivateRecord(doc, rid)
	if err != nil {
		t.Fatalf("activate %s: %v", rid, err)
	}
	return v
}

func mustCorrect(t *testing.T, s *Store, rid ID, expected int, content, reason string) Version {
	t.Helper()
	v, err := s.CorrectRecord(doc, rid, expected, content, reason)
	if err != nil {
		t.Fatalf("correct %s at %d: %v", rid, expected, err)
	}
	return v
}

// ---- 形态：草稿保留原草稿且当前版本为空；已更正记录保留标识、当前版本与完整版本链 ----
//
// 同一患者的两次就诊各放一条草稿与一条已更正记录，诊断、医嘱两个类别都覆盖；
// 查询只返回该次就诊的记录，其他就诊（含同一患者）的内容不能混入，草稿也不
// 能被表现为已生效版本。

func TestEncounterRecordsDraftAndCorrectedShapes(t *testing.T) {
	s, _ := newTestStore(t)
	pid, e1 := setupPatientEncounter(t, s)
	e2, err := s.AddEncounter(doc, pid, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	// 就诊一：草稿诊断 + 已更正医嘱。
	draftDx := mustCreateDraft(t, s, pid, e1, Diagnosis, "草稿诊断内容")
	corrOrd := mustCreateDraft(t, s, pid, e1, Order, "医嘱第 1 版内容")
	ov1 := mustActivate(t, s, corrOrd)
	ov2 := mustCorrect(t, s, corrOrd, 1, "医嘱第 2 版内容", "医嘱更正原因")

	// 就诊二：已更正诊断 + 草稿医嘱（保证两个类别在两种形态下都出现）。
	corrDx := mustCreateDraft(t, s, pid, e2.ID, Diagnosis, "诊断第 1 版内容")
	dv1 := mustActivate(t, s, corrDx)
	dv2 := mustCorrect(t, s, corrDx, 1, "诊断第 2 版内容", "诊断更正原因")
	draftOrd := mustCreateDraft(t, s, pid, e2.ID, Order, "草稿医嘱内容")

	got1, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got1) != 2 {
		t.Fatalf("encounter 1 should have exactly 2 records, got %d (%v)", len(got1), recordIDList(got1))
	}
	m1 := historiesByID(got1)

	// 草稿：保留原草稿内容，当前版本为空，没有任何历史版本，不得表现为已生效。
	d, ok := m1[draftDx]
	if !ok {
		t.Fatalf("draft diagnosis missing from encounter view: %v", recordIDList(got1))
	}
	if d.Record.ID != draftDx {
		t.Fatalf("draft record id changed: %q", d.Record.ID)
	}
	if !d.HasDraft || d.DraftContent != "草稿诊断内容" {
		t.Fatalf("draft content not preserved: HasDraft=%v Content=%q", d.HasDraft, d.DraftContent)
	}
	if d.CurrentVersion != nil {
		t.Fatalf("draft must not be presented as effective, got current version %+v", d.CurrentVersion)
	}
	if d.Record.CurrentVersionID != "" || len(d.Record.Versions) != 0 || len(d.Versions) != 0 {
		t.Fatalf("draft must carry no versions, got id list %v history %+v", d.Record.Versions, d.Versions)
	}

	// 已生效并更正过的记录：原记录标识、当前版本、旧到新的全部版本、
	// 各次更正原因与上一版本关系都保留。
	c, ok := m1[corrOrd]
	if !ok {
		t.Fatalf("corrected order missing from encounter view: %v", recordIDList(got1))
	}
	if c.Record.ID != corrOrd {
		t.Fatalf("corrected record lost its original id: %q", c.Record.ID)
	}
	if c.CurrentVersion == nil || c.CurrentVersion.ID != ov2.ID || c.CurrentVersion.Number != 2 ||
		c.CurrentVersion.Content != "医嘱第 2 版内容" {
		t.Fatalf("current version wrong: %+v", c.CurrentVersion)
	}
	if want := []ID{ov1.ID, ov2.ID}; !reflect.DeepEqual(c.Record.Versions, want) {
		t.Fatalf("version id list = %v, want %v", c.Record.Versions, want)
	}
	if len(c.Versions) != 2 {
		t.Fatalf("full history should keep 2 versions, got %d", len(c.Versions))
	}
	if c.Versions[0].Number != 1 || c.Versions[0].Content != "医嘱第 1 版内容" ||
		c.Versions[0].PrevID != "" || c.Versions[0].Reason != "" {
		t.Fatalf("v1 not preserved: %+v", c.Versions[0])
	}
	if c.Versions[1].ID != ov2.ID || c.Versions[1].Number != 2 ||
		c.Versions[1].Content != "医嘱第 2 版内容" ||
		c.Versions[1].PrevID != ov1.ID || c.Versions[1].Reason != "医嘱更正原因" {
		t.Fatalf("v2 chain/reason wrong: %+v", c.Versions[1])
	}
	if c.Versions[0].Number > c.Versions[1].Number {
		t.Fatal("history must be ordered old to new")
	}

	// 查询只返回该次就诊：同一患者其他就诊的草稿与版本均不能混入。
	for _, h := range got1 {
		if h.Record.EncounterID != e1 {
			t.Fatalf("record from another encounter leaked in: %+v", h.Record)
		}
		if h.Record.ID == corrDx || h.Record.ID == draftOrd {
			t.Fatalf("encounter 2 record %q leaked into encounter 1 view", h.Record.ID)
		}
	}

	// 就诊二的查询同样自成一域，且诊断类别的版本链同样完整。
	got2, err := s.EncounterRecords(doc, pid, e2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 2 {
		t.Fatalf("encounter 2 should have exactly 2 records, got %d (%v)", len(got2), recordIDList(got2))
	}
	m2 := historiesByID(got2)
	for _, h := range got2 {
		if h.Record.EncounterID != e2.ID {
			t.Fatalf("encounter 1 record leaked into encounter 2 view: %+v", h.Record)
		}
	}
	cd := m2[corrDx]
	if cd.CurrentVersion == nil || cd.CurrentVersion.ID != dv2.ID || cd.CurrentVersion.Number != 2 {
		t.Fatalf("corrected diagnosis current version wrong: %+v", cd.CurrentVersion)
	}
	if len(cd.Versions) != 2 ||
		cd.Versions[0].ID != dv1.ID || cd.Versions[0].PrevID != "" ||
		cd.Versions[1].ID != dv2.ID || cd.Versions[1].PrevID != dv1.ID ||
		cd.Versions[1].Reason != "诊断更正原因" {
		t.Fatalf("corrected diagnosis chain wrong: %+v", cd.Versions)
	}
	do2 := m2[draftOrd]
	if do2.CurrentVersion != nil || !do2.HasDraft || do2.DraftContent != "草稿医嘱内容" {
		t.Fatalf("draft order shape wrong: %+v", do2)
	}
}

// ---- 隔离：在手中的结果上整理内容，改不到正式病历，也改不到另一份结果 ----

func TestEncounterRecordsResultIsDetachedFromStore(t *testing.T) {
	s, _ := newTestStore(t)
	pid, e1 := setupPatientEncounter(t, s)
	e2, err := s.AddEncounter(doc, pid, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	draftDx := mustCreateDraft(t, s, pid, e1, Diagnosis, "草稿诊断内容")
	corrOrd := mustCreateDraft(t, s, pid, e1, Order, "医嘱第 1 版内容")
	ov1 := mustActivate(t, s, corrOrd)
	ov2 := mustCorrect(t, s, corrOrd, 1, "医嘱第 2 版内容", "医嘱更正原因")
	// 另一就诊放一条记录，确认版本不会跨记录串台。
	other := mustCreateDraft(t, s, pid, e2.ID, Order, "另一就诊的医嘱")
	mustActivate(t, s, other)

	// 第一份结果作为基线；随后取得第二份独立结果。
	first, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotHistories(first)
	auditBefore := mustAudit(t, s, pid)

	second, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}

	// 在第一份结果上做广泛的本地修改（草稿与已更正记录都改）。
	for i := range first {
		tamperHistory(&first[i])
	}

	// 先前取得的另一份结果保持原值：不丢版本、不换顺序、不混入其他记录的版本。
	assertHistoriesMatches(t, second, want, "previously fetched result after tampering")
	for _, h := range second {
		for _, v := range h.Versions {
			if v.ID == "" || v.RecordID != h.Record.ID {
				t.Fatalf("version %+v does not belong to its record", v)
			}
		}
	}

	// 随后重新读取的正式历史同样保持原值。
	again, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoriesMatches(t, again, want, "official history re-read after tampering")

	// 完整档案视图里该就诊的部分也不得被波及。
	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	var chartE1 []RecordHistory
	for _, h := range chart.Records {
		if h.Record.EncounterID == e1 {
			chartE1 = append(chartE1, h)
		}
	}
	assertHistoriesMatches(t, chartE1, want, "chart view after tampering")

	// 本地整理既不是查询也不是写操作：不能增加、更换或删除正式审计事件。
	auditAfter := mustAudit(t, s, pid)
	if len(auditAfter) != len(auditBefore) {
		t.Fatalf("local mutation changed audit events: before=%d after=%d", len(auditBefore), len(auditAfter))
	}
	for i := range auditBefore {
		if auditBefore[i] != auditAfter[i] {
			t.Fatalf("audit event %d changed:\n before=%+v\n after=%+v", i, auditBefore[i], auditAfter[i])
		}
	}

	// 防止 unused（这些标识同时是上面版本链断言的对象来源）。
	_ = draftDx
	_ = ov1
	_ = ov2
}

// ---- 当前版本视图与历史列表彼此独立：只改一处，另一处不跟着变 ----

func TestEncounterRecordsCurrentAndHistoryAreIndependent(t *testing.T) {
	s, _ := newTestStore(t)
	pid, e1 := setupPatientEncounter(t, s)
	rid := mustCreateDraft(t, s, pid, e1, Diagnosis, "诊断第 1 版内容")
	v1 := mustActivate(t, s, rid)
	v2 := mustCorrect(t, s, rid, 1, "诊断第 2 版内容", "诊断更正原因")

	baseline, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotHistories(baseline)

	// 只改手中结果的当前版本视图，历史列表中对应版本不得跟着变。
	fresh, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	hc := historiesByID(fresh)[rid]
	origLast := hc.Versions[len(hc.Versions)-1] // 值快照
	hc.CurrentVersion.ID = "ver_cv_only_local"
	hc.CurrentVersion.Number = 999
	hc.CurrentVersion.Content = "只改当前版本视图"
	hc.CurrentVersion.PrevID = "ver_cv_only_local_prev"
	hc.CurrentVersion.Reason = "只写在当前版本视图上的原因"
	if got := hc.Versions[len(hc.Versions)-1]; got != origLast {
		t.Fatalf("mutating CurrentVersion dragged the history entry with it:\n got=%+v\nwant=%+v", got, origLast)
	}

	// 只改手中结果的历史列表（内容、原因、上一版本关系），当前版本视图不得跟着变。
	fresh2, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	hc2 := historiesByID(fresh2)[rid]
	origCurrent := *hc2.CurrentVersion
	last := &hc2.Versions[len(hc2.Versions)-1]
	last.Content = "只改历史列表"
	last.Reason = "只写在历史列表上的原因"
	last.PrevID = "ver_hist_only_local_prev"
	if *hc2.CurrentVersion != origCurrent {
		t.Fatalf("mutating history list dragged CurrentVersion with it:\n got=%+v\nwant=%+v",
			*hc2.CurrentVersion, origCurrent)
	}

	// 两处本地改动后，重新读取的正式历史保持原值。
	again, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoriesMatches(t, again, want, "official history after local current/history edits")
	_ = v1
	_ = v2
}

// ---- 正常更正后：新查询显示真实新版本；先前结果停留在查询时状态 ----
//
// 更正判断只以正式记录的当前版本号为准，不依据调用方本地修改过的查询结果；
// 使用已过期的正式版本号仍返回 ErrConflict，完整历史与审计保持原样。

func TestEncounterRecordsCorrectionUsesOfficialVersion(t *testing.T) {
	s, _ := newTestStore(t)
	pid, e1 := setupPatientEncounter(t, s)
	rid := mustCreateDraft(t, s, pid, e1, Diagnosis, "诊断第 1 版内容")
	v1 := mustActivate(t, s, rid)
	v2 := mustCorrect(t, s, rid, 1, "诊断第 2 版内容", "第一次更正原因")

	// 更正前取得、此后不在本地改动的结果。
	frozen, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}

	// 另一份结果被调用方任意修改（抬高版本号、换掉记录标识）。
	local, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	tamperHistory(&local[0])

	auditBeforeCorrection := mustAudit(t, s, pid)

	// 以正式当前版本号 2 更正成功：证明判断不看本地被改成 999 的那份结果。
	v3, err := s.CorrectRecord(doc, rid, 2, "诊断第 3 版内容", "第二次更正原因")
	if err != nil {
		t.Fatalf("correction based on official current version should succeed: %v", err)
	}
	if v3.Number != 3 {
		t.Fatalf("new version number = %d, want official current + 1 = 3", v3.Number)
	}
	if v3.PrevID != v2.ID || v3.Reason != "第二次更正原因" || v3.Content != "诊断第 3 版内容" {
		t.Fatalf("new version chain/reason/content wrong: %+v", v3)
	}

	// 新查询显示真实生成的新版本与完整链路。
	fresh, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 {
		t.Fatalf("unexpected records: %v", recordIDList(fresh))
	}
	h := fresh[0]
	if h.Record.ID != rid {
		t.Fatalf("record id changed: %q", h.Record.ID)
	}
	if h.CurrentVersion == nil || h.CurrentVersion.ID != v3.ID || h.CurrentVersion.Number != 3 ||
		h.CurrentVersion.Content != "诊断第 3 版内容" ||
		h.CurrentVersion.PrevID != v2.ID || h.CurrentVersion.Reason != "第二次更正原因" {
		t.Fatalf("fresh query current version wrong: %+v", h.CurrentVersion)
	}
	if want := []ID{v1.ID, v2.ID, v3.ID}; !reflect.DeepEqual(h.Record.Versions, want) {
		t.Fatalf("version id list = %v, want %v", h.Record.Versions, want)
	}
	if len(h.Versions) != 3 {
		t.Fatalf("full history should keep all 3 versions, got %d", len(h.Versions))
	}
	nums := []int{h.Versions[0].Number, h.Versions[1].Number, h.Versions[2].Number}
	if want := []int{1, 2, 3}; !reflect.DeepEqual(nums, want) {
		t.Fatalf("version order = %v, want %v", nums, want)
	}
	if h.Versions[0].PrevID != "" || h.Versions[1].PrevID != v1.ID || h.Versions[2].PrevID != v2.ID {
		t.Fatalf("prev-version chain broken: %q %q %q",
			h.Versions[0].PrevID, h.Versions[1].PrevID, h.Versions[2].PrevID)
	}
	if h.Versions[1].Reason != "第一次更正原因" || h.Versions[2].Reason != "第二次更正原因" {
		t.Fatalf("correction reasons not preserved: %+v", h.Versions)
	}

	// 此前取得且未在本地改动的结果继续反映查询时状态，不自动变成新版本。
	fb := historiesByID(frozen)[rid]
	if fb.CurrentVersion == nil || fb.CurrentVersion.Number != 2 ||
		fb.CurrentVersion.ID != v2.ID || fb.CurrentVersion.Content != "诊断第 2 版内容" {
		t.Fatalf("earlier result must stay at its query-time snapshot: %+v", fb.CurrentVersion)
	}
	if len(fb.Versions) != 2 {
		t.Fatalf("earlier result must not gain the new version, got %d versions", len(fb.Versions))
	}
	for _, v := range fb.Record.Versions {
		if v == v3.ID {
			t.Fatal("new version leaked into the earlier query result")
		}
	}
	// 被本地改过的那份结果同样保持调用方留下的样子，不被正式状态覆盖或回写。
	if local[0].CurrentVersion == nil || local[0].CurrentVersion.Number != 999 {
		t.Fatalf("locally edited result changed unexpectedly: %+v", local[0].CurrentVersion)
	}

	// 使用已过期的正式版本号：ErrConflict，完整历史与审计保持原样。
	wantFresh := snapshotHistories(fresh)
	if _, err := s.CorrectRecord(doc, rid, 2, "凭过期版本号的抢占更正", "并发更正"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale official version err = %v, want ErrConflict", err)
	}
	if _, err := s.CorrectRecord(doc, rid, 1, "凭更旧版本号的抢占更正", "并发更正"); !errors.Is(err, ErrConflict) {
		t.Fatalf("older official version err = %v, want ErrConflict", err)
	}
	afterConflict, err := s.EncounterRecords(doc, pid, e1)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoriesMatches(t, afterConflict, wantFresh, "history after stale corrections")

	events := mustAudit(t, s, pid)
	if len(events) != len(auditBeforeCorrection)+1 {
		t.Fatalf("successful correction should add exactly one audit event, got delta %d",
			len(events)-len(auditBeforeCorrection))
	}
	counts := auditActions(events)
	if counts[ActionActivated] != 1 || counts[ActionCorrected] != 2 {
		t.Fatalf("audit counts wrong after conflicts: %v", counts)
	}
}

// ---- 权限：接收方不能调用内部查询；拒绝时不携带草稿或历史内容 ----

func TestEncounterRecordsReceiverDeniedWithoutContent(t *testing.T) {
	s, clk := newTestStore(t)
	pid, e1 := setupPatientEncounter(t, s)
	rid := mustCreateDraft(t, s, pid, e1, Diagnosis, "仅供内部查看的草稿内容")
	mustActivate(t, s, rid)
	mustCorrect(t, s, rid, 1, "仅供内部查看的更正内容", "仅供内部查看的更正原因")

	// 无授权：拒绝，结果为 nil，不夹带草稿或历史。
	got, err := s.EncounterRecords(rcv, pid, e1)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver internal query err = %v, want ErrAccessDenied", err)
	}
	if got != nil {
		t.Fatalf("denied query must carry no draft or history, got %+v", got)
	}

	// 即使持有覆盖该就诊诊断的有效接收授权，内部查询入口仍不对接收方开放。
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: e1, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(time.Hour)); err != nil {
		t.Fatalf("grant: %v", err)
	}
	got2, err2 := s.EncounterRecords(rcv, pid, e1)
	if !errors.Is(err2, ErrAccessDenied) {
		t.Fatalf("receiver with grant err = %v, want ErrAccessDenied", err2)
	}
	if got2 != nil {
		t.Fatalf("denied query must carry no draft or history, got %+v", got2)
	}
}
