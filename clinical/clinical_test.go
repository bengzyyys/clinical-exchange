package clinical

import (
	"errors"
	"testing"
	"time"
)

// ---- 测试基础设施 ----

type fakeClock struct{ t time.Time }

func newTestStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

var (
	doc  = InternalActor("doc-1")
	doc2 = InternalActor("doc-2")
	rcv  = ReceiverActor("rcv-a")
	rcvB = ReceiverActor("rcv-b")
)

func setupPatientEncounter(t *testing.T, s *Store) (ID, ID) {
	t.Helper()
	p, err := s.RegisterPatient(doc, "合成患者甲")
	if err != nil {
		t.Fatalf("register patient: %v", err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatalf("add encounter: %v", err)
	}
	return p.ID, e.ID
}

func countRecords(s *Store) int {
	c := 0
	_ = s.view(func(snap *snapshot) error { c = len(snap.Records); return nil })
	return c
}

func auditActions(events []AuditEvent) map[string]int {
	m := map[string]int{}
	for _, e := range events {
		m[e.Action]++
	}
	return m
}

// ---- 基线行为保留 ----

func TestReadyStillTrue(t *testing.T) {
	if !Ready() {
		t.Fatal("Ready must remain true")
	}
}

// ---- 患者与就诊 ----

func TestRegisterPatientIsSyntheticAndStableID(t *testing.T) {
	s, _ := newTestStore(t)
	p, err := s.RegisterPatient(doc, "合成患者甲")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if p.ID == "" {
		t.Fatal("patient id empty")
	}
	if p.Source != SyntheticSource {
		t.Fatalf("source = %q, want %q", p.Source, SyntheticSource)
	}
	got, err := s.GetPatient(doc, p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != p.ID {
		t.Fatalf("id not stable: %q vs %q", got.ID, p.ID)
	}
}

func TestRegisterPatientRequiresInternal(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.RegisterPatient(rcv, "x"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver register err = %v, want ErrAccessDenied", err)
	}
	if _, err := s.RegisterPatient(InternalActor(""), "x"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("empty actor err = %v", err)
	}
	if _, err := s.RegisterPatient(doc, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty name err = %v", err)
	}
}

func TestAddEncounterValidation(t *testing.T) {
	s, _ := newTestStore(t)
	pid, _ := setupPatientEncounter(t, s)

	if _, err := s.AddEncounter(doc, "pat_missing", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing patient err = %v, want ErrNotFound", err)
	}

	other, err := s.RegisterPatient(doc, "合成患者乙")
	if err != nil {
		t.Fatal(err)
	}
	// 手工构造“其他患者的就诊 id”直接作为引用：通过第二患者的真实就诊验证。
	encOther, err := s.AddEncounter(doc, other.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 在草稿上跨患者引用就诊。
	if _, err := s.CreateDraft(doc, pid, encOther.ID, Diagnosis, "x"); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient encounter err = %v, want ErrMismatchedPatient", err)
	}
}

// ---- 草稿生命周期 ----

func TestDraftCRUDAndFailureLeavesNothing(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	before := countRecords(s)
	if _, err := s.CreateDraft(doc, pid, "enc_missing", Diagnosis, "高血压"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing encounter err = %v", err)
	}
	if countRecords(s) != before {
		t.Fatal("failed create left a record")
	}
	if _, err := s.CreateDraft(doc, pid, eid, "bogus", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad category err = %v", err)
	}
	if _, err := s.CreateDraft(doc, pid, eid, Diagnosis, "   "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank content err = %v", err)
	}

	r, err := s.CreateDraft(doc, pid, eid, Diagnosis, "高血压 I10")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := s.UpdateDraft(doc, r.ID, "高血压 I10（修订）"); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if err := s.DeleteDraft(doc, r.ID); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	if _, err := s.UpdateDraft(doc, r.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update deleted draft err = %v", err)
	}
}

func TestActivationSnapshotsContentAndTime(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	r, err := s.CreateDraft(doc, pid, eid, Order, "阿司匹林 100mg")
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(2 * time.Hour)
	v, err := s.ActivateRecord(doc, r.ID)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if v.Number != 1 || v.Content != "阿司匹林 100mg" {
		t.Fatalf("unexpected v1: %+v", v)
	}
	if !v.CreatedAt.Equal(clk.t) {
		t.Fatalf("effective time %v != %v", v.CreatedAt, clk.t)
	}

	// 生效后不能按草稿方式覆盖或删除。
	if _, err := s.UpdateDraft(doc, r.ID, "覆盖"); !errors.Is(err, ErrActive) {
		t.Fatalf("update active err = %v, want ErrActive", err)
	}
	if err := s.DeleteDraft(doc, r.ID); !errors.Is(err, ErrActive) {
		t.Fatalf("delete active err = %v, want ErrActive", err)
	}
	// 重复生效也被拒绝。
	if _, err := s.ActivateRecord(doc, r.ID); !errors.Is(err, ErrActive) {
		t.Fatalf("re-activate err = %v, want ErrActive", err)
	}
}

func TestCorrectionVersionChainAndConflict(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	r, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "旧诊断")
	v1, err := s.ActivateRecord(doc, r.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 缺原因、非法版本号。
	if _, err := s.CorrectRecord(doc, r.ID, 1, "新诊断", "  "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank reason err = %v", err)
	}
	if _, err := s.CorrectRecord(doc, r.ID, 0, "新诊断", "原因"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero version err = %v", err)
	}

	clk.t = clk.t.Add(time.Hour)
	v2, err := s.CorrectRecord(doc, r.ID, 1, "新诊断", "录入笔误")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if v2.Number != 2 || v2.PrevID != v1.ID || v2.Reason != "录入笔误" {
		t.Fatalf("bad v2: %+v", v2)
	}
	if !v2.CreatedAt.Equal(clk.t) {
		t.Fatalf("correction time %v != %v", v2.CreatedAt, clk.t)
	}

	// v1 内容与原因原样保留。
	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	var hist *RecordHistory
	for i := range chart.Records {
		if chart.Records[i].Record.ID == r.ID {
			hist = &chart.Records[i]
		}
	}
	if hist == nil || len(hist.Versions) != 2 {
		t.Fatalf("history = %+v", hist)
	}
	if hist.Versions[0].Content != "旧诊断" || hist.Versions[0].Reason != "" {
		t.Fatalf("v1 mutated: %+v", hist.Versions[0])
	}
	if hist.Versions[1].PrevID != v1.ID {
		t.Fatal("version link lost")
	}

	eventsBefore := len(chart.AuditEvents)
	// 基于过期版本号更正：拒绝且保持现状，不产生事件。
	if _, err := s.CorrectRecord(doc, r.ID, 1, "抢占更正", "并发"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale correction err = %v, want ErrConflict", err)
	}
	chart2, _ := s.Chart(doc, pid)
	if len(chart2.AuditEvents) != eventsBefore {
		t.Fatal("failed correction generated an audit event")
	}
	if hist2 := findHistory(chart2, r.ID); hist2 == nil || hist2.CurrentVersion.Content != "新诊断" || len(hist2.Versions) != 2 {
		t.Fatal("state changed after failed correction")
	}

	// 以当前版本号继续更正成功，链继续延伸。
	v3, err := s.CorrectRecord(doc2, r.ID, 2, "最终诊断", "补充检查")
	if err != nil {
		t.Fatalf("correct v3: %v", err)
	}
	if v3.Number != 3 || v3.PrevID != v2.ID {
		t.Fatalf("bad v3: %+v", v3)
	}
}

func TestReceiverCannotUseInternalAPIs(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	r, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "x")

	if _, err := s.CreateDraft(rcv, pid, eid, Diagnosis, "x"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver draft: %v", err)
	}
	if _, err := s.ActivateRecord(rcv, r.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver activate: %v", err)
	}
	if _, err := s.CorrectRecord(rcv, r.ID, 1, "x", "y"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver correct: %v", err)
	}
	if _, err := s.Chart(rcv, pid); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver chart: %v", err)
	}
	if _, err := s.Grant(rcv, pid, "z", nil, time.Now(), time.Now()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver grant: %v", err)
	}
	if err := s.DeactivatePatient(rcv, pid); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver deactivate: %v", err)
	}
	if _, err := s.Read(doc, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("internal actor cannot use receiver Read: %v", err)
	}
	if _, err := s.Read(Actor{ID: "x", Kind: "other"}, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("unknown actor kind must be denied")
	}
	if _, err := s.Read(rcv, pid, eid, "bogus"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad category err = %v", err)
	}
	if _, err := s.Read(rcv, "", eid, Diagnosis); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty patient err = %v", err)
	}
}

func TestOpenEmptyDirRejected(t *testing.T) {
	if _, err := Open(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty dir err = %v", err)
	}
}

func findHistory(chart PatientChart, recordID ID) *RecordHistory {
	for i := range chart.Records {
		if chart.Records[i].Record.ID == recordID {
			return &chart.Records[i]
		}
	}
	return nil
}

func TestInternalQueryHelpers(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	later := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	e2, err := s.AddEncounter(doc, pid, later)
	if err != nil {
		t.Fatal(err)
	}

	// 就诊按发生时间排序。
	encs, err := s.ListEncounters(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(encs) != 2 || encs[0].ID != eid || encs[1].ID != e2.ID {
		t.Fatalf("encounters not ordered: %+v", encs)
	}
	if _, err := s.ListEncounters(doc, "pat_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing patient err = %v", err)
	}
	if _, err := s.ListEncounters(rcv, pid); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver err = %v", err)
	}

	r, _ := s.CreateDraft(doc, pid, eid, Order, "医嘱草稿")
	if _, err := s.ActivateRecord(doc, r.ID); err != nil {
		t.Fatal(err)
	}
	hist, err := s.EncounterRecords(doc, pid, eid)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Record.ID != r.ID || hist[0].CurrentVersion == nil {
		t.Fatalf("encounter records = %+v", hist)
	}
	if _, err := s.EncounterRecords(doc, pid, "enc_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing encounter err = %v", err)
	}
	other, _ := s.RegisterPatient(doc, "乙")
	if _, err := s.EncounterRecords(doc, other.ID, eid); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient encounter err = %v", err)
	}
	if _, err := s.EncounterRecords(rcv, pid, eid); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver err = %v", err)
	}

	// 患者不存在的错误。
	if _, err := s.GetPatient(doc, "pat_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing err = %v", err)
	}
}
