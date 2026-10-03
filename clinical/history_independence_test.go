package clinical

import (
	"errors"
	"testing"
)

// setupTwoVersionedRecord 建一名患者、一次就诊与两条已生效记录，其中第一条
// 已更正到第 2 版。返回患者、就诊、两条记录及其版本标识。
func setupTwoVersionedRecord(t *testing.T, s *Store) (pid, eid ID, r1, r2 ID, r1v1, r1v2, r2v1 ID) {
	t.Helper()
	pid, eid = setupPatientEncounter(t, s)

	rec1, err := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断第一版内容")
	if err != nil {
		t.Fatalf("draft r1: %v", err)
	}
	v1, err := s.ActivateRecord(doc, rec1.ID)
	if err != nil {
		t.Fatalf("activate r1: %v", err)
	}
	v2, err := s.CorrectRecord(doc, rec1.ID, 1, "诊断第二版内容", "第一次更正原因")
	if err != nil {
		t.Fatalf("correct r1: %v", err)
	}

	rec2, err := s.CreateDraft(doc, pid, eid, Order, "另一条记录的内容")
	if err != nil {
		t.Fatalf("draft r2: %v", err)
	}
	o1, err := s.ActivateRecord(doc, rec2.ID)
	if err != nil {
		t.Fatalf("activate r2: %v", err)
	}
	return pid, eid, rec1.ID, rec2.ID, v1.ID, v2.ID, o1.ID
}

// tamperRecordIdentifiers 以各种方式篡改手中结果里的版本标识列表、当前版本
// 与版本内容：把旧版本标识换成空值或另一条记录的版本标识。
func tamperRecordIdentifiers(h *RecordHistory, foreignVersionID ID) {
	h.Record.Versions[0] = ""
	h.Record.Versions[1] = foreignVersionID
	h.Record.CurrentVersionID = ""
	h.CurrentVersion.Content = "调用方本地改写的内容"
	h.Versions[0].Content = "调用方本地改写的旧版本内容"
}

func assertOfficialHistoryIntact(t *testing.T, h *RecordHistory, r1, r1v1, r1v2, r2v1 ID) {
	t.Helper()
	if h.Record.ID != r1 {
		t.Fatalf("history record id = %q, want %q", h.Record.ID, r1)
	}
	if len(h.Record.Versions) != 2 || h.Record.Versions[0] != r1v1 || h.Record.Versions[1] != r1v2 {
		t.Fatalf("official version id list altered: %+v", h.Record.Versions)
	}
	if h.Record.CurrentVersionID != r1v2 {
		t.Fatalf("official current version altered: %q", h.Record.CurrentVersionID)
	}
	if h.CurrentVersion == nil || h.CurrentVersion.ID != r1v2 || h.CurrentVersion.Content != "诊断第二版内容" {
		t.Fatalf("current version view wrong: %+v", h.CurrentVersion)
	}
	if len(h.Versions) != 2 {
		t.Fatalf("got %d versions, want 2 (no missing versions)", len(h.Versions))
	}
	if h.Versions[0].ID != r1v1 || h.Versions[0].Content != "诊断第一版内容" || h.Versions[0].Reason != "" || h.Versions[0].PrevID != "" {
		t.Fatalf("v1 not preserved: %+v", h.Versions[0])
	}
	if h.Versions[1].ID != r1v2 || h.Versions[1].Content != "诊断第二版内容" ||
		h.Versions[1].Reason != "第一次更正原因" || h.Versions[1].PrevID != r1v1 {
		t.Fatalf("v2 not preserved: %+v", h.Versions[1])
	}
	for _, v := range h.Versions {
		if v.ID == r2v1 || v.Content == "另一条记录的内容" {
			t.Fatalf("foreign record content leaked into history: %+v", v)
		}
	}
}

func TestChartResultIsIndependentCopy(t *testing.T) {
	s, _ := newTestStore(t)
	pid, _, r1, r2, r1v1, r1v2, r2v1 := setupTwoVersionedRecord(t, s)

	chart1, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	h1 := findHistory(chart1, r1)
	tamperRecordIdentifiers(h1, r2v1)

	// 再次查看：正式历史不得被手中结果的改动污染，不得缺版本、不得混入
	// 另一条记录的标识与内容。
	chart2, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	assertOfficialHistoryIntact(t, findHistory(chart2, r1), r1, r1v1, r1v2, r2v1)

	// 另一条记录的历史也不得被改写。
	if hOther := findHistory(chart2, r2); hOther == nil ||
		len(hOther.Record.Versions) != 1 || hOther.Record.Versions[0] != r2v1 ||
		hOther.CurrentVersion == nil || hOther.CurrentVersion.ID != r2v1 {
		t.Fatalf("other record history affected: %+v", hOther)
	}
}

func TestEncounterRecordsResultIsIndependentCopy(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid, r1, _, r1v1, r1v2, r2v1 := setupTwoVersionedRecord(t, s)

	out1, err := s.EncounterRecords(doc, pid, eid)
	if err != nil {
		t.Fatal(err)
	}
	var h1 *RecordHistory
	for i := range out1 {
		if out1[i].Record.ID == r1 {
			h1 = &out1[i]
		}
	}
	tamperRecordIdentifiers(h1, r2v1)

	out2, err := s.EncounterRecords(doc, pid, eid)
	if err != nil {
		t.Fatal(err)
	}
	var h2 *RecordHistory
	for i := range out2 {
		if out2[i].Record.ID == r1 {
			h2 = &out2[i]
		}
	}
	assertOfficialHistoryIntact(t, h2, r1, r1v1, r1v2, r2v1)
}

func TestChartAndEncounterResultsDoNotCrossWrite(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid, r1, r2, r1v1, r1v2, r2v1 := setupTwoVersionedRecord(t, s)

	// 改完整档案的结果，不得影响随后取得的单次就诊结果。
	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	tamperRecordIdentifiers(findHistory(chart, r1), r2v1)

	enc, err := s.EncounterRecords(doc, pid, eid)
	if err != nil {
		t.Fatal(err)
	}
	var hFromEncounter *RecordHistory
	for i := range enc {
		if enc[i].Record.ID == r1 {
			hFromEncounter = &enc[i]
		}
	}
	assertOfficialHistoryIntact(t, hFromEncounter, r1, r1v1, r1v2, r2v1)

	// 反向同样成立：改单次就诊结果，不得影响完整档案。
	for i := range enc {
		if enc[i].Record.ID == r2 {
			enc[i].Record.Versions[0] = r1v1
		}
	}
	chart2, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	assertOfficialHistoryIntact(t, findHistory(chart2, r1), r1, r1v1, r1v2, r2v1)
	if hOther := findHistory(chart2, r2); hOther == nil || hOther.Record.Versions[0] != r2v1 {
		t.Fatalf("other record leaked via encounter result: %+v", hOther)
	}
}

func TestLocalTamperDoesNotEnterLaterCorrection(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid, r1, _, r1v1, r1v2, r2v1 := setupTwoVersionedRecord(t, s)

	chartBefore, _ := s.Chart(doc, pid)
	auditsBefore := len(chartBefore.AuditEvents)

	tampered, _ := s.Chart(doc, pid)
	tamperRecordIdentifiers(findHistory(tampered, r1), r2v1)
	enc, _ := s.EncounterRecords(doc, pid, eid)
	for i := range enc {
		if enc[i].Record.ID == r1 {
			enc[i].Record.Versions[0] = ""
		}
	}

	// 查看与本地改动本身不产生审计事件。
	chartAfterViews, _ := s.Chart(doc, pid)
	if len(chartAfterViews.AuditEvents) != auditsBefore {
		t.Fatalf("viewing or local edits generated %d audit events", len(chartAfterViews.AuditEvents)-auditsBefore)
	}

	// 本地把版本标识改乱后，按真实当前版本号与非空原因做合法更正，
	// 仍正常生成第 3 版并延续正式历史。
	v3, err := s.CorrectRecord(doc, r1, 2, "诊断第三版内容", "第二次更正原因")
	if err != nil {
		t.Fatalf("legitimate correction after local tamper failed: %v", err)
	}
	if v3.Number != 3 || v3.PrevID != r1v2 {
		t.Fatalf("new version does not extend official chain: %+v", v3)
	}

	chart3, _ := s.Chart(doc, pid)
	h3 := findHistory(chart3, r1)
	if len(h3.Record.Versions) != 3 {
		t.Fatalf("got %d official version ids, want 3: %+v", len(h3.Record.Versions), h3.Record.Versions)
	}
	if h3.Record.Versions[0] != r1v1 || h3.Record.Versions[1] != r1v2 || h3.Record.Versions[2] != v3.ID {
		t.Fatalf("official chain broken by local edits: %+v", h3.Record.Versions)
	}
	if len(h3.Versions) != 3 {
		t.Fatalf("got %d versions, want all 3 preserved", len(h3.Versions))
	}
	wantContent := []string{"诊断第一版内容", "诊断第二版内容", "诊断第三版内容"}
	wantReason := []string{"", "第一次更正原因", "第二次更正原因"}
	for i, v := range h3.Versions {
		if v.Content != wantContent[i] || v.Reason != wantReason[i] {
			t.Fatalf("version %d not preserved: %+v", i+1, v)
		}
	}
	if h3.Versions[0].PrevID != "" || h3.Versions[1].PrevID != r1v1 || h3.Versions[2].PrevID != r1v2 {
		t.Fatalf("prev-id chain altered: %+v", h3.Versions)
	}
	// 真正的更正仍按既有规则记入审计，且只多这一条。
	if len(chart3.AuditEvents) != auditsBefore+1 ||
		chart3.AuditEvents[len(chart3.AuditEvents)-1].Action != ActionCorrected {
		t.Fatalf("audit events after correction = %d (before %d)", len(chart3.AuditEvents), auditsBefore)
	}
}

func TestStaleResultStaysAtFetchedContent(t *testing.T) {
	s, _ := newTestStore(t)
	pid, _, r1, _, r1v1, r1v2, _ := setupTwoVersionedRecord(t, s)

	chart1, _ := s.Chart(doc, pid)
	h1 := findHistory(chart1, r1)

	v3, err := s.CorrectRecord(doc, r1, 2, "诊断第三版内容", "第二次更正原因")
	if err != nil {
		t.Fatal(err)
	}

	// 先前取得的结果仍代表取得时的内容，不会因后续更正变成新视图。
	if len(h1.Versions) != 2 || h1.CurrentVersion.ID != r1v2 {
		t.Fatalf("previously fetched view changed: %+v", h1)
	}
	for _, vid := range h1.Record.Versions {
		if vid == v3.ID {
			t.Fatal("stale result picked up the later version")
		}
	}
	if len(h1.Record.Versions) != 2 || h1.Record.Versions[0] != r1v1 || h1.Record.Versions[1] != r1v2 {
		t.Fatalf("stale result id list changed: %+v", h1.Record.Versions)
	}

	// 新查看则反映更正后的状态。
	chart2, _ := s.Chart(doc, pid)
	h2 := findHistory(chart2, r1)
	if len(h2.Versions) != 3 || h2.CurrentVersion.ID != v3.ID {
		t.Fatalf("fresh view does not reflect correction: %+v", h2)
	}
}

func TestLocalTamperDoesNotAffectConflict(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid, r1, _, r1v1, r1v2, _ := setupTwoVersionedRecord(t, s)

	chart, _ := s.Chart(doc, pid)
	tamperRecordIdentifiers(findHistory(chart, r1), r1v1)

	// 本地篡改不影响版本判定：过期版本号更正仍返回 ErrConflict，
	// 正式历史保持两版不变。
	if _, err := s.CorrectRecord(doc, r1, 1, "抢占更正", "并发更正"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale correction err = %v, want ErrConflict", err)
	}
	again, _ := s.EncounterRecords(doc, pid, eid)
	var h *RecordHistory
	for i := range again {
		if again[i].Record.ID == r1 {
			h = &again[i]
		}
	}
	if len(h.Versions) != 2 || h.Record.Versions[0] != r1v1 || h.Record.Versions[1] != r1v2 {
		t.Fatalf("history changed after rejected correction: %+v", h)
	}
}

func TestDraftHistoryViewUnchanged(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	d, err := s.CreateDraft(doc, pid, eid, Diagnosis, "草稿内容")
	if err != nil {
		t.Fatal(err)
	}

	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	h := findHistory(chart, d.ID)
	if h == nil || !h.HasDraft || h.DraftContent != "草稿内容" {
		t.Fatalf("draft missing from chart: %+v", h)
	}
	if h.CurrentVersion != nil || len(h.Versions) != 0 || h.Record.CurrentVersionID != "" {
		t.Fatalf("draft must have no current version: %+v", h)
	}

	enc, err := s.EncounterRecords(doc, pid, eid)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) != 1 || !enc[0].HasDraft || enc[0].CurrentVersion != nil || len(enc[0].Versions) != 0 {
		t.Fatalf("draft view from encounter wrong: %+v", enc)
	}
}

func TestHistoryVisibleAfterDeactivation(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid, r1, _, r1v1, r1v2, _ := setupTwoVersionedRecord(t, s)
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatal(err)
	}

	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatalf("chart after deactivation: %v", err)
	}
	if !chart.Patient.Deactivated {
		t.Fatal("chart patient not marked deactivated")
	}
	assertOfficialHistoryIntact(t, findHistory(chart, r1), r1, r1v1, r1v2, "")

	enc, err := s.EncounterRecords(doc, pid, eid)
	if err != nil {
		t.Fatalf("encounter records after deactivation: %v", err)
	}
	if len(enc) != 2 {
		t.Fatalf("got %d records after deactivation, want full history (2)", len(enc))
	}
}

func TestReceiverCannotReadHistory(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid, _, _, _, _, _ := setupTwoVersionedRecord(t, s)
	if _, err := s.Chart(rcv, pid); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver chart err = %v", err)
	}
	if _, err := s.EncounterRecords(rcv, pid, eid); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver encounter records err = %v", err)
	}
}
