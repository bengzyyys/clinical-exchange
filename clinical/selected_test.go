package clinical

import (
	"errors"
	"testing"
	"time"
)

// ---- 限定授权：基本行为 ----

func TestGrantSelectedBasic(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	// 两条诊断 + 一条医嘱，全部生效。
	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}
	d2, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}
	o1, _ := s.CreateDraft(doc, pid, eid, Order, "医嘱1")
	if _, err := s.ActivateRecord(doc, o1.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	// 纯限定授权：只选 d1 和 o1。
	a, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{
			{EncounterID: eid, Category: Diagnosis, RecordID: d1.ID},
			{EncounterID: eid, Category: Order, RecordID: o1.ID},
		}, start, end)
	if err != nil {
		t.Fatalf("grant selected: %v", err)
	}
	if len(a.Scopes) != 0 {
		t.Fatalf("scopes should be empty: %+v", a.Scopes)
	}
	if len(a.SelectedRecords) != 2 {
		t.Fatalf("selected records = %d, want 2: %+v", len(a.SelectedRecords), a.SelectedRecords)
	}

	// Read 诊断：只看到 d1，看不到 d2。
	res, err := s.Read(rcv, pid, eid, Diagnosis)
	if err != nil {
		t.Fatalf("read diagnosis: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != d1.ID {
		t.Fatalf("diagnosis records = %+v, want only %s", res.Records, d1.ID)
	}

	// Read 医嘱：只看到 o1。
	res, err = s.Read(rcv, pid, eid, Order)
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != o1.ID {
		t.Fatalf("order records = %+v, want only %s", res.Records, o1.ID)
	}
}

// ---- 限定授权：重复选择只算一次 ----

func TestGrantSelectedDeduplicates(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	a, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{
			{EncounterID: eid, Category: Diagnosis, RecordID: d1.ID},
			{EncounterID: eid, Category: Diagnosis, RecordID: d1.ID},
		}, start, end)
	if err != nil {
		t.Fatalf("grant selected: %v", err)
	}
	if len(a.SelectedRecords) != 1 {
		t.Fatalf("selected records = %d, want 1 (deduped)", len(a.SelectedRecords))
	}
}

// ---- 限定授权：与整类授权并存，Read 取并集 ----

func TestGrantSelectedMixedWithScope(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}
	d2, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}
	d3, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断3")
	if _, err := s.ActivateRecord(doc, d3.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	// 混合授权：整类覆盖诊断 + 限定只选 d3。
	a, err := s.GrantSelected(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: d3.ID}},
		start, end)
	if err != nil {
		t.Fatalf("grant mixed: %v", err)
	}
	if len(a.Scopes) != 1 || len(a.SelectedRecords) != 1 {
		t.Fatalf("mixed auth wrong: scopes=%+v selected=%+v", a.Scopes, a.SelectedRecords)
	}

	// Read 诊断：整类覆盖全部三条。
	res, err := s.Read(rcv, pid, eid, Diagnosis)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Records) != 3 {
		t.Fatalf("records = %d, want 3 (whole-category covers all)", len(res.Records))
	}
}

// ---- 整类撤回后只剩限定记录 ----

func TestReadAfterScopeRevokedKeepsSelected(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}
	d2, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	// 整类授权。
	scopeAuth, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	// 限定授权：只选 d2。
	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: d2.ID}},
		start, end); err != nil {
		t.Fatal(err)
	}

	// 撤回整类授权。
	if err := s.Revoke(doc, pid, scopeAuth.ID); err != nil {
		t.Fatal(err)
	}

	// Read：只剩 d2。
	res, err := s.Read(rcv, pid, eid, Diagnosis)
	if err != nil {
		t.Fatalf("read after revoke: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != d2.ID {
		t.Fatalf("records = %+v, want only %s", res.Records, d2.ID)
	}
}

// ---- 限定记录更正后覆盖当前版本 ----

func TestSelectedRecordCorrectionCoversCurrentVersion(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断v1")
	v1, err := s.ActivateRecord(doc, d1.ID)
	if err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: d1.ID}},
		start, end); err != nil {
		t.Fatal(err)
	}

	// 更正到 v2。
	v2, err := s.CorrectRecord(doc, d1.ID, 1, "诊断v2", "复核")
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Read(rcv, pid, eid, Diagnosis)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(res.Records))
	}
	if res.Records[0].VersionID != v2.ID || res.Records[0].Version != 2 || res.Records[0].Content != "诊断v2" {
		t.Fatalf("selected record not current version: %+v (v1=%s v2=%s)", res.Records[0], v1.ID, v2.ID)
	}
}

// ---- 限定授权不自动覆盖新增记录 ----

func TestSelectedScopeDoesNotAutoCoverNewRecords(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: d1.ID}},
		start, end); err != nil {
		t.Fatal(err)
	}

	// 新增一条诊断并生效。
	d2, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}

	// Read：只看到 d1，看不到 d2。
	res, err := s.Read(rcv, pid, eid, Diagnosis)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != d1.ID {
		t.Fatalf("records = %+v, want only %s (new record not auto-covered)", res.Records, d1.ID)
	}
}

// ---- 限定授权校验 ----

func TestGrantSelectedValidation(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	other, _ := s.RegisterPatient(doc, "合成患者乙")
	encOther, _ := s.AddEncounter(doc, other.ID, time.Time{})

	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}
	draft, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "草稿")
	recOther, _ := s.CreateDraft(doc, other.ID, encOther.ID, Diagnosis, "他人诊断")
	if _, err := s.ActivateRecord(doc, recOther.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	// 空选择。
	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil, nil, start, end); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty selected err = %v, want ErrInvalidArgument", err)
	}
	// 空白记录标识。
	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: "  "}}, start, end); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank record id err = %v, want ErrInvalidArgument", err)
	}
	// 不存在的记录。
	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: "rec_missing"}}, start, end); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing record err = %v, want ErrNotFound", err)
	}
	// 跨患者记录。
	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: encOther.ID, Category: Diagnosis, RecordID: recOther.ID}}, start, end); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient record err = %v, want ErrMismatchedPatient", err)
	}
	// 草稿（尚未生效）。
	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: draft.ID}}, start, end); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("draft record err = %v, want ErrInvalidArgument", err)
	}
	// 记录与声明的就诊不符。
	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: encOther.ID, Category: Diagnosis, RecordID: d1.ID}}, start, end); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("record-encounter mismatch err = %v, want ErrInvalidArgument", err)
	}
	// 记录与声明的类别不符。
	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Order, RecordID: d1.ID}}, start, end); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("record-category mismatch err = %v, want ErrInvalidArgument", err)
	}

	// 失败不留下任何授权。
	if auths, _ := s.ListAuthorizations(doc, pid, ""); len(auths) != 0 {
		t.Fatalf("failed grants persisted: %d", len(auths))
	}
}

// ---- 限定授权与交换：绑定授权覆盖 ----

func TestCreateExchangeWithSelectedScope(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	v1, err := s.ActivateRecord(doc, d1.ID)
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	// 限定授权：只选 d1。
	a, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: d1.ID}},
		start, end)
	if err != nil {
		t.Fatal(err)
	}

	// 用绑定授权打包 d1：成功。
	x, err := s.CreateExchange(doc, pid, rcv.ID, a.ID, []ID{d1.ID}, "req-sel")
	if err != nil {
		t.Fatalf("create exchange with selected scope: %v", err)
	}
	if len(x.Package.Records) != 1 || x.Package.Records[0].RecordID != d1.ID {
		t.Fatalf("package records = %+v, want only %s", x.Package.Records, d1.ID)
	}
	if x.Package.Records[0].VersionID != v1.ID {
		t.Fatalf("package version wrong: %+v", x.Package.Records[0])
	}

	// 用绑定授权打包 d2（未被该授权覆盖）：拒绝。
	if _, err := s.CreateExchange(doc, pid, rcv.ID, a.ID, []ID{d2.ID}, "req-sel-2"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("uncovered record err = %v, want ErrAccessDenied", err)
	}

	// 失败不留下交换或审计。
	if xs, _ := s.ListExchanges(doc, pid); len(xs) != 1 {
		t.Fatalf("exchanges = %d, want 1", len(xs))
	}
}

// ---- 限定授权与交换：不能借用其他授权 ----

func TestCreateExchangeCannotBorrowOtherAuth(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}
	d2, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	// 授权 A：限定只选 d1。
	authA, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: d1.ID}},
		start, end)
	if err != nil {
		t.Fatal(err)
	}
	// 授权 B：整类覆盖诊断（包含 d2）。
	authB, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	_ = authB

	// 用授权 A 打包 d2：虽然授权 B 覆盖 d2，但不能借用 → 拒绝。
	if _, err := s.CreateExchange(doc, pid, rcv.ID, authA.ID, []ID{d2.ID}, "req-borrow"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("borrowed coverage err = %v, want ErrAccessDenied", err)
	}
}

// ---- 限定授权：关闭重开后保留 ----

func TestSelectedScopePersistsAcrossReopen(t *testing.T) {
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
	p, _ := s.RegisterPatient(doc, "持久化限定患者")
	e, _ := s.AddEncounter(doc, p.ID, time.Time{})
	d1, _ := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}
	d2, _ := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(48 * time.Hour)
	a, err := s.GrantSelected(doc, p.ID, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: e.ID, Category: Diagnosis, RecordID: d1.ID}},
		start, end)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	// 限定授权保留。
	got, err := s2.GetAuthorization(doc, p.ID, a.ID)
	if err != nil {
		t.Fatalf("get auth after reopen: %v", err)
	}
	if len(got.SelectedRecords) != 1 || got.SelectedRecords[0].RecordID != d1.ID {
		t.Fatalf("selected records lost: %+v", got.SelectedRecords)
	}

	// Read 仍只看到 d1。
	res, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != d1.ID {
		t.Fatalf("records = %+v, want only %s", res.Records, d1.ID)
	}

	// 更正 d1 后重开：覆盖当前版本。
	if _, err := s2.CorrectRecord(doc, d1.ID, 1, "诊断v2", "更正"); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })
	res, err = s3.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("read after correction reopen: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].Content != "诊断v2" || res.Records[0].Version != 2 {
		t.Fatalf("records = %+v, want v2", res.Records)
	}
}

// ---- 限定授权：时间窗边界 ----

func TestSelectedScopeTimeWindow(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)

	if _, err := s.GrantSelected(doc, pid, rcv.ID, nil,
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: d1.ID}},
		start, end); err != nil {
		t.Fatal(err)
	}

	clk.t = start.Add(-time.Nanosecond)
	if _, err := s.Read(rcv, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("before start must be denied")
	}
	clk.t = start
	if _, err := s.Read(rcv, pid, eid, Diagnosis); err != nil {
		t.Fatalf("at start: %v", err)
	}
	clk.t = end
	if _, err := s.Read(rcv, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("at end must be denied")
	}
}

// ---- 限定授权：内部使用者可分辨并看到选中记录 ----

func TestGetAuthorizationShowsSelectedRecords(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	a, err := s.GrantSelected(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Order}},
		[]SelectedRecord{{EncounterID: eid, Category: Diagnosis, RecordID: d1.ID}},
		start, end)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetAuthorization(doc, pid, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 整类范围与限定范围都可见。
	if len(got.Scopes) != 1 || got.Scopes[0] != (Scope{EncounterID: eid, Category: Order}) {
		t.Fatalf("scopes wrong: %+v", got.Scopes)
	}
	if len(got.SelectedRecords) != 1 || got.SelectedRecords[0].RecordID != d1.ID {
		t.Fatalf("selected records wrong: %+v", got.SelectedRecords)
	}

	// 列表中也能看到。
	auths, err := s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || len(auths[0].SelectedRecords) != 1 {
		t.Fatalf("list auths wrong: %+v", auths)
	}
}
