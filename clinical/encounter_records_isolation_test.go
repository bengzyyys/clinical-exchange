package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// erFixture 构造 EncounterRecords 隔离回归所需的固定档案：
//
//   - 就诊 e1：
//   - draftDx：仍是草稿的诊断（HasDraft，无当前版本）；
//   - order1：已生效的医嘱（仅第 1 版）；
//   - rec1：已生效并更正两次的诊断（v1→v2→v3，带原因与版本链）。
//   - 就诊 e2（同一患者的另一次就诊）：
//   - draftOther：草稿；rec2：已生效并更正过的诊断。
type erFixture struct {
	s   *Store
	pid ID
	e1  ID
	e2  ID

	draftDx ID

	order1  ID
	orderV1 Version

	rec1                   ID
	rec1V1, rec1V2, rec1V3 Version
	draftOther             ID
	rec2                   ID
	rec2V1, rec2V2         Version
}

func setupERFixture(t *testing.T) erFixture {
	t.Helper()
	s, _ := newTestStore(t)

	p, err := s.RegisterPatient(doc, "EncounterRecords 隔离患者")
	if err != nil {
		t.Fatalf("register patient: %v", err)
	}
	enc1, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatalf("add encounter e1: %v", err)
	}
	enc2, err := s.AddEncounter(doc, p.ID, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("add encounter e2: %v", err)
	}
	e1, e2 := enc1.ID, enc2.ID

	draftDx, err := s.CreateDraft(doc, p.ID, e1, Diagnosis, "pending-dx")
	if err != nil {
		t.Fatalf("create draft dx: %v", err)
	}

	orderDraft, err := s.CreateDraft(doc, p.ID, e1, Order, "order-draft")
	if err != nil {
		t.Fatalf("create order draft: %v", err)
	}
	orderV1, err := s.ActivateRecord(doc, orderDraft.ID)
	if err != nil {
		t.Fatalf("activate order: %v", err)
	}

	r1Draft, err := s.CreateDraft(doc, p.ID, e1, Diagnosis, "dx-v1")
	if err != nil {
		t.Fatalf("create rec1 draft: %v", err)
	}
	r1V1, err := s.ActivateRecord(doc, r1Draft.ID)
	if err != nil {
		t.Fatalf("activate rec1: %v", err)
	}
	r1V2, err := s.CorrectRecord(doc, r1Draft.ID, 1, "dx-v2", "reason-a")
	if err != nil {
		t.Fatalf("correct rec1 v2: %v", err)
	}
	r1V3, err := s.CorrectRecord(doc, r1Draft.ID, 2, "dx-v3", "reason-b")
	if err != nil {
		t.Fatalf("correct rec1 v3: %v", err)
	}

	draftOther, err := s.CreateDraft(doc, p.ID, e2, Diagnosis, "other-pending")
	if err != nil {
		t.Fatalf("create other draft: %v", err)
	}
	r2Draft, err := s.CreateDraft(doc, p.ID, e2, Diagnosis, "other-v1")
	if err != nil {
		t.Fatalf("create rec2 draft: %v", err)
	}
	r2V1, err := s.ActivateRecord(doc, r2Draft.ID)
	if err != nil {
		t.Fatalf("activate rec2: %v", err)
	}
	r2V2, err := s.CorrectRecord(doc, r2Draft.ID, 1, "other-v2", "other-reason")
	if err != nil {
		t.Fatalf("correct rec2: %v", err)
	}

	return erFixture{
		s:          s,
		pid:        p.ID,
		e1:         e1,
		e2:         e2,
		draftDx:    draftDx.ID,
		order1:     orderDraft.ID,
		orderV1:    orderV1,
		rec1:       r1Draft.ID,
		rec1V1:     r1V1,
		rec1V2:     r1V2,
		rec1V3:     r1V3,
		draftOther: draftOther.ID,
		rec2:       r2Draft.ID,
		rec2V1:     r2V1,
		rec2V2:     r2V2,
	}
}

func queryER(t *testing.T, s *Store, pid, enc ID) []RecordHistory {
	t.Helper()
	got, err := s.EncounterRecords(doc, pid, enc)
	if err != nil {
		t.Fatalf("EncounterRecords(%s): %v", enc, err)
	}
	return got
}

func findER(list []RecordHistory, recordID ID) *RecordHistory {
	for i := range list {
		if list[i].Record.ID == recordID {
			return &list[i]
		}
	}
	return nil
}

// tamperERHistory 改写一份查询结果中规格列出的全部字段：记录标识、版本标识
// 列表、草稿内容、当前版本内容，以及历史版本的内容/原因/上一版本关系。
func tamperERHistory(t *testing.T, h *RecordHistory) {
	t.Helper()
	h.Record.ID = "rec_TAMPERED"
	h.Record.PatientID = "pat_TAMPERED"
	h.Record.EncounterID = "enc_TAMPERED"
	h.Record.Category = "tampered-category"
	h.Record.CurrentVersionID = "ver_TAMPERED_CURRENT"
	if len(h.Record.Versions) > 0 {
		h.Record.Versions[0] = "ver_TAMPERED_MIXED_IN"
		h.Record.Versions = append(h.Record.Versions, "ver_TAMPERED_EXTRA")
	}
	h.DraftContent = "TAMPERED DRAFT"
	h.HasDraft = !h.HasDraft
	if h.CurrentVersion != nil {
		h.CurrentVersion.ID = "ver_TAMPERED_CURRENT"
		h.CurrentVersion.RecordID = "rec_TAMPERED"
		h.CurrentVersion.Number = 999
		h.CurrentVersion.Content = "TAMPERED CURRENT"
		h.CurrentVersion.PrevID = "ver_TAMPERED_PREV"
		h.CurrentVersion.Reason = "TAMPERED CURRENT REASON"
	}
	for i := range h.Versions {
		h.Versions[i].ID = "ver_TAMPERED_" + h.Versions[i].Content
		h.Versions[i].RecordID = "rec_TAMPERED"
		h.Versions[i].Number += 500
		h.Versions[i].Content = "TAMPERED " + h.Versions[i].Content
		h.Versions[i].PrevID = "ver_TAMPERED_PREV"
		h.Versions[i].Reason = "TAMPERED REASON"
	}
}

// assertRec1AtV3 校验 rec1 的视图仍为正式的 v1→v2→v3 状态：
// 原记录标识、当前版本、旧到新的全部版本、各次更正原因与上一版本关系。
func assertRec1AtV3(t *testing.T, f erFixture, h *RecordHistory) {
	t.Helper()
	if h == nil {
		t.Fatal("rec1 missing from encounter result")
	}
	if h.Record.ID != f.rec1 {
		t.Fatalf("record id = %q, want %q", h.Record.ID, f.rec1)
	}
	if h.HasDraft || h.DraftContent != "" {
		t.Fatalf("effective record presented with draft: %+v", h.Record)
	}
	if h.Record.CurrentVersionID != f.rec1V3.ID {
		t.Fatalf("current version id = %q, want %q", h.Record.CurrentVersionID, f.rec1V3.ID)
	}
	if h.CurrentVersion == nil {
		t.Fatal("current version is empty for an effective record")
	}
	if *h.CurrentVersion != f.rec1V3 {
		t.Fatalf("current version = %+v, want %+v", *h.CurrentVersion, f.rec1V3)
	}
	wantIDs := []ID{f.rec1V1.ID, f.rec1V2.ID, f.rec1V3.ID}
	if got := h.Record.Versions; !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("version id list = %v, want %v", got, wantIDs)
	}
	if len(h.Versions) != 3 {
		t.Fatalf("history versions = %d, want 3", len(h.Versions))
	}
	want := []Version{f.rec1V1, f.rec1V2, f.rec1V3}
	for i := range want {
		if h.Versions[i] != want[i] {
			t.Fatalf("version[%d] = %+v, want %+v", i, h.Versions[i], want[i])
		}
		if h.Versions[i].Number != i+1 {
			t.Fatalf("version[%d] number = %d, want %d (order changed)", i, h.Versions[i].Number, i+1)
		}
	}
}

// TestEncounterRecordsResultsAreIndependentCopies 是核心回归：在一份查询结果上
// 整理内容，不能改掉正式病历，也不能改掉另一份查询结果；诊断与医嘱都适用。
func TestEncounterRecordsResultsAreIndependentCopies(t *testing.T) {
	f := setupERFixture(t)

	auditBefore, err := f.s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatalf("audit events: %v", err)
	}

	first := queryER(t, f.s, f.pid, f.e1)
	second := queryER(t, f.s, f.pid, f.e1)
	e2Before := queryER(t, f.s, f.pid, f.e2)

	// 首次结果必须如实呈现：草稿保留原草稿内容且当前版本为空；
	// 已更正记录保留原标识、当前版本、全部版本、原因与版本链。
	draft := findER(first, f.draftDx)
	if draft == nil || !draft.HasDraft || draft.DraftContent != "pending-dx" ||
		draft.CurrentVersion != nil || len(draft.Versions) != 0 ||
		len(draft.Record.Versions) != 0 || draft.Record.CurrentVersionID != "" {
		t.Fatalf("draft record misrepresented: %+v", draft)
	}
	order := findER(first, f.order1)
	if order == nil || order.CurrentVersion == nil || *order.CurrentVersion != f.orderV1 ||
		len(order.Versions) != 1 || order.Versions[0] != f.orderV1 {
		t.Fatalf("order record misrepresented: %+v", order)
	}
	assertRec1AtV3(t, f, findER(first, f.rec1))

	// 在“手中的结果”上全面改写：诊断、医嘱、草稿无一例外。
	for i := range first {
		tamperERHistory(t, &first[i])
	}

	// 另一份先前取得的结果与随后重新读取的正式历史必须逐字节保持原值：
	// 不丢版本、不变顺序、不混入其他记录的版本。
	again := queryER(t, f.s, f.pid, f.e1)
	if !reflect.DeepEqual(second, again) {
		t.Fatalf("official history changed by local mutation\nsecond: %+v\nagain:  %+v", second, again)
	}
	assertRec1AtV3(t, f, findER(again, f.rec1))
	if d := findER(again, f.draftDx); d == nil || !d.HasDraft || d.DraftContent != "pending-dx" {
		t.Fatalf("official draft changed by local mutation: %+v", d)
	}

	// 同患者其他就诊的结果不能受影响，也不能在就诊查询间串内容。
	e2After := queryER(t, f.s, f.pid, f.e2)
	if !reflect.DeepEqual(e2Before, e2After) {
		t.Fatalf("other encounter result changed: %+v vs %+v", e2Before, e2After)
	}

	// 查询与本地改动都不能增加、更换或删除正式审计事件。
	auditAfter, err := f.s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatalf("audit events after tamper: %v", err)
	}
	if !reflect.DeepEqual(auditBefore, auditAfter) {
		t.Fatalf("audit events changed by query/local edit:\nbefore=%+v\nafter= %+v", auditBefore, auditAfter)
	}
}

// TestEncounterRecordsCurrentAndHistoryViewsIndependent 保证同一份结果内
// “当前版本视图”与“历史版本列表”是相互独立的副本，且记录上的版本标识列表
// 也不与详细版本切片共享存储。
func TestEncounterRecordsCurrentAndHistoryViewsIndependent(t *testing.T) {
	f := setupERFixture(t)

	fresh := queryER(t, f.s, f.pid, f.e1)
	h := findER(fresh, f.rec1)

	// 只改当前版本视图，历史列表不能跟着变。
	historyLast := h.Versions[len(h.Versions)-1]
	h.CurrentVersion.Content = "local-only-current"
	h.CurrentVersion.PrevID = "ver_LOCAL_PREV"
	h.CurrentVersion.Reason = "local-only-current-reason"
	if got := h.Versions[len(h.Versions)-1]; got != historyLast {
		t.Fatalf("history entry followed current-view edit: %+v vs %+v", got, historyLast)
	}

	// 只改历史列表中的对应版本，当前版本视图不能跟着变。
	h.Versions[len(h.Versions)-1].Content = "local-only-history"
	h.Versions[len(h.Versions)-1].Reason = "local-only-history-reason"
	if h.CurrentVersion.Content != "local-only-current" || h.CurrentVersion.Reason != "local-only-current-reason" {
		t.Fatalf("current view followed history-list edit: %+v", h.CurrentVersion)
	}

	// 只改记录上的版本标识列表，详细历史切片不能跟着变。
	h.Record.Versions[0] = "ver_LOCAL_LIST"
	h.Record.Versions = append(h.Record.Versions, "ver_LOCAL_LIST_EXTRA")
	if len(h.Versions) != 3 || h.Versions[0].ID != f.rec1V1.ID {
		t.Fatalf("detail history followed version-id list edit: %+v", h.Versions)
	}

	// 正式病历对这些本地改动无感知。
	assertRec1AtV3(t, f, findER(queryER(t, f.s, f.pid, f.e1), f.rec1))
}

// TestEncounterRecordsScopedToEncounterAndDraftsNotEffective 保证查询只返回该次
// 就诊的记录，草稿不被表现为已生效版本，且本地改写不影响接收方读到的正式当前版本。
func TestEncounterRecordsScopedToEncounterAndDraftsNotEffective(t *testing.T) {
	f := setupERFixture(t)

	got1 := queryER(t, f.s, f.pid, f.e1)
	got2 := queryER(t, f.s, f.pid, f.e2)

	ids1 := map[ID]bool{}
	for _, h := range got1 {
		ids1[h.Record.ID] = true
		if h.Record.EncounterID != f.e1 {
			t.Fatalf("record %q from another encounter leaked into e1", h.Record.ID)
		}
	}
	if !reflect.DeepEqual(ids1, map[ID]bool{f.draftDx: true, f.order1: true, f.rec1: true}) {
		t.Fatalf("e1 records = %v", ids1)
	}
	ids2 := map[ID]bool{}
	for _, h := range got2 {
		ids2[h.Record.ID] = true
		if h.Record.EncounterID != f.e2 {
			t.Fatalf("record %q from another encounter leaked into e2", h.Record.ID)
		}
	}
	if !reflect.DeepEqual(ids2, map[ID]bool{f.draftOther: true, f.rec2: true}) {
		t.Fatalf("e2 records = %v", ids2)
	}

	// 草稿不能被表现为已经生效的版本；已生效记录不带草稿。
	d := findER(got1, f.draftDx)
	if d == nil || !d.HasDraft || d.CurrentVersion != nil || len(d.Versions) != 0 {
		t.Fatalf("draft presented as effective: %+v", d)
	}
	r := findER(got1, f.rec1)
	if r == nil || r.HasDraft || r.CurrentVersion == nil || r.CurrentVersion.Number != 3 {
		t.Fatalf("effective record misrepresented: %+v", r)
	}

	// 改写本地结果后，接收方经正式授权读到的仍是正式当前版本，且看不到草稿。
	for i := range got1 {
		tamperERHistory(t, &got1[i])
	}
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if _, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Diagnosis}},
		base, base.Add(24*time.Hour)); err != nil {
		t.Fatalf("grant: %v", err)
	}
	rr, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("receiver read: %v", err)
	}
	if len(rr.Records) != 1 {
		t.Fatalf("receiver saw %d records (draft leaked?): %+v", len(rr.Records), rr.Records)
	}
	eff := rr.Records[0]
	if eff.RecordID != f.rec1 || eff.Version != 3 || eff.Content != "dx-v3" || eff.VersionID != f.rec1V3.ID {
		t.Fatalf("receiver read official state changed by local tamper: %+v", eff)
	}
}

// TestEncounterRecordsLocalEditsDoNotAffectCorrection 保证随后通过正常更正操作
// 提交时：新版本号在正式当前版本上增加一，上一版本关系指向原当前版本并保留
// 原因；更早取得的结果不自动变成新版本；冲突判断以正式版本号为准。
func TestEncounterRecordsLocalEditsDoNotAffectCorrection(t *testing.T) {
	f := setupERFixture(t)

	snapshot := queryER(t, f.s, f.pid, f.e1) // 未在本地改动，反映查询时状态（v3）

	work := queryER(t, f.s, f.pid, f.e1)
	for i := range work {
		tamperERHistory(t, &work[i])
	}

	auditBefore, err := f.s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatalf("audit events: %v", err)
	}

	// 过期的正式版本号仍返回 ErrConflict；完整历史与审计保持原样。
	if _, err := f.s.CorrectRecord(doc, f.rec1, 2, "stale-content", "stale-reason"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale official version err = %v, want ErrConflict", err)
	}
	if got := queryER(t, f.s, f.pid, f.e1); !reflect.DeepEqual(got, snapshot) {
		t.Fatalf("history changed after failed stale correction")
	}
	auditStale, err := f.s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatalf("audit events: %v", err)
	}
	if !reflect.DeepEqual(auditStale, auditBefore) {
		t.Fatal("failed correction added/replaced/removed an audit event")
	}

	// 本地把结果里的版本号改成 999 等不影响判断：仍以正式当前版本号 3 为准。
	v4, err := f.s.CorrectRecord(doc, f.rec1, 3, "dx-v4", "reason-c")
	if err != nil {
		t.Fatalf("correct to v4: %v", err)
	}
	if v4.Number != 4 || v4.PrevID != f.rec1V3.ID || v4.Reason != "reason-c" || v4.Content != "dx-v4" {
		t.Fatalf("new version not based on official current: %+v", v4)
	}

	fresh := queryER(t, f.s, f.pid, f.e1)
	h := findER(fresh, f.rec1)
	if h == nil || h.CurrentVersion == nil {
		t.Fatalf("rec1 missing after correction: %+v", fresh)
	}
	if *h.CurrentVersion != v4 {
		t.Fatalf("current version = %+v, want %+v", *h.CurrentVersion, v4)
	}
	if len(h.Versions) != 4 {
		t.Fatalf("history versions = %d, want 4", len(h.Versions))
	}
	want := []Version{f.rec1V1, f.rec1V2, f.rec1V3, v4}
	for i := range want {
		if h.Versions[i] != want[i] {
			t.Fatalf("version[%d] = %+v, want %+v", i, h.Versions[i], want[i])
		}
	}
	if h.Versions[3].PrevID != f.rec1V3.ID || h.Versions[3].Reason != "reason-c" {
		t.Fatalf("newest history link/reason lost: %+v", h.Versions[3])
	}

	// 先前取得且未本地改动的结果继续反映查询时状态，不自动变成新版本。
	old := findER(snapshot, f.rec1)
	if old == nil || old.CurrentVersion == nil {
		t.Fatalf("snapshot result lost rec1: %+v", snapshot)
	}
	if old.CurrentVersion.Number != 3 || old.CurrentVersion.ID != f.rec1V3.ID || len(old.Versions) != 3 {
		t.Fatalf("earlier result auto-updated to new version: %+v", old)
	}

	// 版本号 2、3 都已过期；正式记录当前为 4，冲突依旧，历史与审计不动。
	for _, stale := range []int{2, 3} {
		if _, err := f.s.CorrectRecord(doc, f.rec1, stale, "x", "y"); !errors.Is(err, ErrConflict) {
			t.Fatalf("correction at stale version %d err = %v, want ErrConflict", stale, err)
		}
	}
	if got := queryER(t, f.s, f.pid, f.e1); !reflect.DeepEqual(got, fresh) {
		t.Fatal("history changed after post-correction stale attempts")
	}
	auditAfter, err := f.s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatalf("audit events: %v", err)
	}
	if len(auditAfter) != len(auditBefore)+1 {
		t.Fatalf("audit events delta = %d, want exactly 1 (successful correction)", len(auditAfter)-len(auditBefore))
	}
	if auditAfter[len(auditAfter)-1].Action != ActionCorrected || auditAfter[len(auditAfter)-1].ObjectID != f.rec1 {
		t.Fatalf("last audit event = %+v, want corrected %s", auditAfter[len(auditAfter)-1], f.rec1)
	}
}

// TestEncounterRecordsDeniedCarriesNoContent 保证接收方（及其他非内部身份）
// 不能调用这一内部查询，拒绝时不携带草稿或历史内容。
func TestEncounterRecordsDeniedCarriesNoContent(t *testing.T) {
	f := setupERFixture(t)
	denied := []Actor{
		rcv,
		ReceiverActor("anyone-else"),
		ReceiverActor(""),
		{ID: "ghost", Kind: "other"},
		InternalActor(""),
	}
	for _, actor := range denied {
		res, err := f.s.EncounterRecords(actor, f.pid, f.e1)
		if !errors.Is(err, ErrAccessDenied) {
			t.Fatalf("actor %+v: err = %v, want ErrAccessDenied", actor, err)
		}
		if res != nil {
			t.Fatalf("actor %+v: denial carried %d records", actor, len(res))
		}
	}
}
