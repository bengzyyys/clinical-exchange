package clinical

import (
	"errors"
	"testing"
	"time"
)

// ---- 授权校验 ----

func TestGrantValidation(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	other, _ := s.RegisterPatient(doc, "合成患者乙")
	encOther, _ := s.AddEncounter(doc, other.ID, time.Time{})

	t0 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(24 * time.Hour)

	if _, err := s.Grant(doc, pid, "rcv-x", nil, t0, t1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty scope err = %v", err)
	}
	if _, err := s.Grant(doc, pid, "rcv-x", []Scope{{EncounterID: eid, Category: "bad"}}, t0, t1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad category err = %v", err)
	}
	if _, err := s.Grant(doc, pid, "rcv-x", []Scope{{EncounterID: encOther.ID, Category: Diagnosis}}, t0, t1); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient scope err = %v, want ErrMismatchedPatient", err)
	}
	if _, err := s.Grant(doc, pid, "", []Scope{{EncounterID: eid, Category: Diagnosis}}, t0, t1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty receiver err = %v", err)
	}
	if _, err := s.Grant(doc, pid, "rcv-x", []Scope{{EncounterID: eid, Category: Diagnosis}}, t1, t1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("start == end err = %v", err)
	}
	if _, err := s.Grant(doc, pid, "rcv-x", []Scope{{EncounterID: eid, Category: Diagnosis}}, t1, t0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("start > end err = %v", err)
	}
	if _, err := s.Grant(doc, "pat_missing", "rcv-x", []Scope{{EncounterID: eid, Category: Diagnosis}}, t0, t1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing patient err = %v", err)
	}

	// 失败的授权不落任何记录。
	if auths, _ := s.ListAuthorizations(doc, pid, ""); len(auths) != 0 {
		t.Fatalf("failed grants persisted: %d", len(auths))
	}

	// 成功授权：重复范围项去重、按稳定顺序保存。
	a, err := s.Grant(doc, pid, "rcv-x",
		[]Scope{
			{EncounterID: eid, Category: Order},
			{EncounterID: eid, Category: Diagnosis},
			{EncounterID: eid, Category: Order},
		},
		t0, t1)
	if err != nil {
		t.Fatalf("grant dedup: %v", err)
	}
	if len(a.Scopes) != 2 || a.Scopes[0] != (Scope{EncounterID: eid, Category: Diagnosis}) {
		t.Fatalf("scopes not deduped/sorted: %+v", a.Scopes)
	}

	if _, err := s.GetAuthorization(doc, pid, a.ID); err != nil {
		t.Fatalf("get auth: %v", err)
	}
	if _, err := s.GetAuthorization(doc, pid, "auth_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing auth err = %v", err)
	}
	if _, err := s.GetAuthorization(doc, other.ID, a.ID); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient auth err = %v", err)
	}
	if _, err := s.GetAuthorization(rcv, pid, a.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver get auth err = %v", err)
	}
	if auths, _ := s.ListAuthorizations(doc, pid, "rcv-x"); len(auths) != 1 || auths[0].ID != a.ID {
		t.Fatalf("receiver filter = %+v", auths)
	}
	if auths, _ := s.ListAuthorizations(doc, other.ID, ""); len(auths) != 0 {
		t.Fatalf("auths leaked across patients: %d", len(auths))
	}
	if _, err := s.ListAuthorizations(rcv, pid, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver list err = %v", err)
	}
}

// ---- 接收方读取：只看当前生效版本 ----

func TestReadOnlyCurrentEffective(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	// 草稿一条（尚未生效）。
	draft, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "未生效草稿")
	// 生效一条，再更正一次。
	rec, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断v1")
	v1, _ := s.ActivateRecord(doc, rec.ID)
	v2, _ := s.CorrectRecord(doc, rec.ID, 1, "诊断v2", "笔误")
	_ = v1
	// 一条医嘱。
	ord, _ := s.CreateDraft(doc, pid, eid, Order, "医嘱v1")
	if _, err := s.ActivateRecord(doc, ord.ID); err != nil {
		t.Fatal(err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(time.Hour)
	if _, err := s.Grant(doc, pid, rcv.ID, []Scope{{EncounterID: eid, Category: Diagnosis}}, start, end); err != nil {
		t.Fatalf("grant: %v", err)
	}

	res, err := s.Read(rcv, pid, eid, Diagnosis)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("records = %d, want 1 (draft hidden)", len(res.Records))
	}
	got := res.Records[0]
	if got.RecordID != rec.ID || got.VersionID != v2.ID || got.Version != 2 || got.Content != "诊断v2" {
		t.Fatalf("unexpected payload: %+v", got)
	}
	if !got.EffectiveAt.Equal(v2.CreatedAt) {
		t.Fatalf("effective time %v != %v", got.EffectiveAt, v2.CreatedAt)
	}

	// 授权仅限诊断：医嘱被拒绝。
	if _, err := s.Read(rcv, pid, eid, Order); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("read orders err = %v", err)
	}
	// 草稿始终不出现（再确认：删除旧草稿引用无效）。
	_ = draft
	// 其他接收方无权。
	if _, err := s.Read(rcvB, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other receiver err = %v", err)
	}
	// 不存在/跨患者查询统一拒绝，不泄露存在性。
	if _, err := s.Read(rcv, pid, "enc_missing", Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("missing encounter err = %v", err)
	}
	if _, err := s.Read(rcv, "pat_missing", eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("missing patient err = %v", err)
	}
}

func TestReadLaterActivatedAndCorrectedCovered(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	rec, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "v1内容")

	// 先授权（此刻还没有任何生效记录）。
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Read(rcv, pid, eid, Diagnosis); err != nil || len(res.Records) != 0 {
		t.Fatalf("before activation: res=%+v err=%v", res, err)
	}

	if _, err := s.ActivateRecord(doc, rec.ID); err != nil {
		t.Fatal(err)
	}
	res, err := s.Read(rcv, pid, eid, Diagnosis)
	if err != nil || len(res.Records) != 1 || res.Records[0].Content != "v1内容" {
		t.Fatalf("after activation: %+v %v", res, err)
	}

	if _, err := s.CorrectRecord(doc, rec.ID, 1, "v2内容", "依据补充"); err != nil {
		t.Fatal(err)
	}
	res, _ = s.Read(rcv, pid, eid, Diagnosis)
	if res.Records[0].Content != "v2内容" || res.Records[0].Version != 2 {
		t.Fatalf("after correction receiver did not get new current version: %+v", res.Records)
	}

	// 授权不自动扩大到新增就诊。
	enc2, _ := s.AddEncounter(doc, pid, time.Time{})
	rec2, _ := s.CreateDraft(doc, pid, enc2.ID, Diagnosis, "新就诊诊断")
	if _, err := s.ActivateRecord(doc, rec2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(rcv, pid, enc2.ID, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("new encounter should not be auto-covered: %v", err)
	}
}

// ---- 时间窗边界 ----

func TestReadTimeWindowBoundaries(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	rec, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "x")
	if _, err := s.ActivateRecord(doc, rec.ID); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	if _, err := s.Grant(doc, pid, rcv.ID, []Scope{{EncounterID: eid, Category: Diagnosis}}, start, end); err != nil {
		t.Fatal(err)
	}

	clk.t = start.Add(-time.Nanosecond)
	if _, err := s.Read(rcv, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("before start must be denied")
	}
	clk.t = start // 开始时刻生效
	if _, err := s.Read(rcv, pid, eid, Diagnosis); err != nil {
		t.Fatalf("at start: %v", err)
	}
	clk.t = end.Add(-time.Nanosecond)
	if _, err := s.Read(rcv, pid, eid, Diagnosis); err != nil {
		t.Fatalf("just before end: %v", err)
	}
	clk.t = end // 截止时刻立即失效
	if _, err := s.Read(rcv, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("at end must be denied")
	}
	clk.t = end.Add(time.Hour)
	if _, err := s.Read(rcv, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("after end must be denied")
	}
}

// ---- 撤回：多授权独立、幂等 ----

func TestMultipleGrantsSameReceiverJudgedIndependently(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	rec, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "x")
	if _, err := s.ActivateRecord(doc, rec.ID); err != nil {
		t.Fatal(err)
	}
	now := clk.t

	// 同一接收方的两个授权：一个已过期，一个有效。
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		now.Add(-48*time.Hour), now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(rcv, pid, eid, Diagnosis); err != nil {
		t.Fatalf("valid grant must grant access despite expired sibling: %v", err)
	}

	// 再加一个尚未开始的授权：不影响。
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		now.Add(time.Hour), now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(rcv, pid, eid, Diagnosis); err != nil {
		t.Fatalf("future grant must not affect current access: %v", err)
	}

	// 推进到第二个授权已到期、第三个已开始：仍可读。
	clk.t = now.Add(90 * time.Minute)
	if _, err := s.Read(rcv, pid, eid, Diagnosis); err != nil {
		t.Fatalf("third grant now active: %v", err)
	}

	// 全部到期后明确拒绝。
	clk.t = now.Add(2 * time.Hour)
	if _, err := s.Read(rcv, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("all grants expired: %v", err)
	}
}

func TestRevokeIndependentAndIdempotent(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	rec, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "x")
	if _, err := s.ActivateRecord(doc, rec.ID); err != nil {
		t.Fatal(err)
	}
	window := func() (time.Time, time.Time) { return clk.t.Add(-time.Hour), clk.t.Add(time.Hour) }
	st, en := window()

	a1, err := s.Grant(doc, pid, rcv.ID, []Scope{{EncounterID: eid, Category: Diagnosis}}, st, en)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(doc, pid, rcvB.ID, []Scope{{EncounterID: eid, Category: Diagnosis}}, st, en); err != nil {
		t.Fatal(err)
	}

	eventsBefore := len(mustAudit(t, s, pid))
	if err := s.Revoke(doc, pid, a1.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// 重复撤回：无新事件、无新变化。
	if err := s.Revoke(doc, pid, a1.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	events := mustAudit(t, s, pid)
	if got := auditActions(events)[ActionRevoked]; got != 1 {
		t.Fatalf("revoke events = %d, want 1", got)
	}
	_ = eventsBefore

	if _, err := s.Read(rcv, pid, eid, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("revoked receiver must be denied")
	}
	// 撤回 rcv 的授权不影响 rcvB。
	if _, err := s.Read(rcvB, pid, eid, Diagnosis); err != nil {
		t.Fatalf("other receiver still valid: %v", err)
	}

	// 撤回不存在/跨患者授权。
	if err := s.Revoke(doc, pid, "auth_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing revoke err = %v", err)
	}
	other, _ := s.RegisterPatient(doc, "合成患者乙")
	if err := s.Revoke(doc, other.ID, a1.ID); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient revoke err = %v", err)
	}
}

func mustAudit(t *testing.T, s *Store, pid ID) []AuditEvent {
	t.Helper()
	evs, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// ---- 停用 ----

func TestDeactivationLocksWritesAndReadsButKeepsHistory(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	r, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "草稿")
	active, _ := s.CreateDraft(doc, pid, eid, Order, "医嘱")
	if _, err := s.ActivateRecord(doc, active.ID); err != nil {
		t.Fatal(err)
	}
	st := time.Now().Add(-time.Hour)
	auth, err := s.Grant(doc, pid, rcv.ID, []Scope{{EncounterID: eid, Category: Order}}, st, st.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	eventsBefore := len(mustAudit(t, s, pid))
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	// 重复停用幂等：无额外事件。
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatalf("second deactivate: %v", err)
	}
	if got := len(mustAudit(t, s, pid)); got != eventsBefore+1 {
		t.Fatalf("audit count %d, want %d", got, eventsBefore+1)
	}

	if _, err := s.AddEncounter(doc, pid, time.Time{}); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("add encounter err = %v", err)
	}
	if _, err := s.CreateDraft(doc, pid, eid, Diagnosis, "x"); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("create draft err = %v", err)
	}
	if _, err := s.UpdateDraft(doc, r.ID, "x"); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("update draft err = %v", err)
	}
	if err := s.DeleteDraft(doc, r.ID); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("delete draft err = %v", err)
	}
	if _, err := s.ActivateRecord(doc, r.ID); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("activate err = %v", err)
	}
	if _, err := s.CorrectRecord(doc, active.ID, 1, "x", "原因"); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("correct err = %v", err)
	}
	if _, err := s.Grant(doc, pid, "rcv-z", []Scope{{EncounterID: eid, Category: Order}}, st, st.Add(2*time.Hour)); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("grant err = %v", err)
	}
	// 撤回不在停用后的禁止清单内：收缩权限始终允许，且保持幂等。
	if err := s.Revoke(doc, pid, auth.ID); err != nil {
		t.Fatalf("revoke after deactivation: %v", err)
	}
	nAfterRevoke := len(mustAudit(t, s, pid))
	if err := s.Revoke(doc, pid, auth.ID); err != nil {
		t.Fatalf("repeat revoke: %v", err)
	}
	if n := len(mustAudit(t, s, pid)); n != nAfterRevoke {
		t.Fatalf("repeat revoke after deactivation added events: %d -> %d", nAfterRevoke, n)
	}
	// 接收方不能继续读取。
	if _, err := s.Read(rcv, pid, eid, Order); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver read err = %v", err)
	}

	// 内部使用者仍能查看历史与审计。
	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	if !chart.Patient.Deactivated {
		t.Fatal("chart should show deactivated")
	}
	if len(chart.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(chart.Records))
	}
	if auditActions(chart.AuditEvents)[ActionDeactivated] != 1 {
		t.Fatalf("deactivation audit missing: %+v", chart.AuditEvents)
	}
}

// ---- 审计事件内容 ----

func TestAuditEventDetails(t *testing.T) {
	s, _ := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	r, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "x")
	v, _ := s.ActivateRecord(doc, r.ID)
	if _, err := s.CorrectRecord(doc2, r.ID, 1, "y", "原因"); err != nil {
		t.Fatal(err)
	}
	st := time.Now()
	a, _ := s.Grant(doc, pid, rcv.ID, []Scope{{EncounterID: eid, Category: Diagnosis}}, st, st.Add(time.Hour))
	if err := s.Revoke(doc, pid, a.ID); err != nil {
		t.Fatal(err)
	}

	events := mustAudit(t, s, pid)
	want := []struct{ action, objectType, actorID, objectID string }{
		{ActionActivated, "record", doc.ID, r.ID},
		{ActionCorrected, "record", doc2.ID, r.ID},
		{ActionGranted, "authorization", doc.ID, a.ID},
		{ActionRevoked, "authorization", doc.ID, a.ID},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		ev := events[i]
		if ev.Action != w.action || ev.ObjectType != w.objectType || ev.ActorID != w.actorID || ev.ObjectID != w.objectID {
			t.Fatalf("event %d = %+v, want action %s object %s actor %s", i, ev, w.action, w.objectID, w.actorID)
		}
		if ev.PatientID != pid || ev.ID == "" || ev.OccurredAt.IsZero() {
			t.Fatalf("event %d missing fields: %+v", i, ev)
		}
	}
	// v 仅用于确认版本存在。
	_ = v

	// 审计按患者隔离：另一患者看不到这些事件。
	other, _ := s.RegisterPatient(doc, "乙")
	if evs, _ := s.AuditEvents(doc, other.ID); len(evs) != 0 {
		t.Fatalf("audit leaked across patients: %d", len(evs))
	}
	// 接收方看不到审计。
	if _, err := s.AuditEvents(rcv, pid); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver audit err = %v", err)
	}
}

// ---- 持久化：关闭后同位置重开 ----

func TestReopenPreservesStateAndRecomputesExpiry(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "持久化患者")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := s.AddEncounter(doc, p.ID, time.Time{})
	rec, _ := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "v1")
	v1, _ := s.ActivateRecord(doc, rec.ID)
	v2, _ := s.CorrectRecord(doc, rec.ID, 1, "v2", "原因A")

	// 授权：在当前窗口内有效，但未来会到期。
	grantStart := clk.t.Add(-time.Hour)
	grantEnd := clk.t.Add(2 * time.Hour)
	a, _ := s.Grant(doc, p.ID, rcv.ID, []Scope{{EncounterID: e.ID, Category: Diagnosis}}, grantStart, grantEnd)
	_ = v1
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开：档案、版本、授权状态、审计都在。
	s2 := open()
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chart.Records) != 1 || len(chart.Records[0].Versions) != 2 {
		t.Fatalf("versions lost: %+v", chart.Records)
	}
	h := chart.Records[0]
	if h.CurrentVersion.ID != v2.ID || h.Versions[0].Content != "v1" || h.Versions[1].Reason != "原因A" {
		t.Fatalf("history not preserved: %+v", h)
	}
	if len(chart.AuditEvents) != 3 {
		t.Fatalf("audit lost: %d", len(chart.AuditEvents))
	}
	gotAuth, err := s2.GetAuthorization(doc, p.ID, a.ID)
	if err != nil {
		t.Fatalf("authorization lost: %v", err)
	}
	if gotAuth.RevokedAt != nil {
		t.Fatal("authorization should not be revoked")
	}
	// 窗口内可读。
	if res, err := s2.Read(rcv, p.ID, e.ID, Diagnosis); err != nil || len(res.Records) != 1 || res.Records[0].Content != "v2" {
		t.Fatalf("read after reopen: %+v %v", res, err)
	}
	// 撤回后重开：撤回状态与撤回审计持久保留，接收方仍被拒绝。
	if err := s2.Revoke(doc, p.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	s2b := open()
	gotAuth2, err := s2b.GetAuthorization(doc, p.ID, a.ID)
	if err != nil {
		t.Fatalf("authorization lost after reopen: %v", err)
	}
	if gotAuth2.RevokedAt == nil {
		t.Fatal("revocation not persisted across reopen")
	}
	if _, err := s2b.Read(rcv, p.ID, e.ID, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked grant must deny after reopen: %v", err)
	}
	// 重开后重复撤回仍幂等。
	rn1 := len(mustAudit(t, s2b, p.ID))
	if err := s2b.Revoke(doc, p.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if rn2 := len(mustAudit(t, s2b, p.ID)); rn2 != rn1 {
		t.Fatalf("idempotent revoke after reopen added events: %d -> %d", rn1, rn2)
	}
	if err := s2b.Close(); err != nil {
		t.Fatal(err)
	}

	// 时间推进到授权到期之后再次打开：到期按本次读取时间判断。
	clk.t = grantEnd.Add(time.Hour)
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })
	if _, err := s3.Read(rcv, p.ID, e.ID, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expired grant must deny after reopen: %v", err)
	}

	// 停用状态持久化，且重开后重复停用不再产生事件。
	if err := s3.DeactivatePatient(doc, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s3.Close(); err != nil {
		t.Fatal(err)
	}
	s4 := open()
	t.Cleanup(func() { _ = s4.Close() })
	pat, _ := s4.GetPatient(doc, p.ID)
	if !pat.Deactivated {
		t.Fatal("deactivation not persisted")
	}
	n1 := len(mustAudit(t, s4, p.ID))
	if err := s4.DeactivatePatient(doc, p.ID); err != nil {
		t.Fatal(err)
	}
	if n2 := len(mustAudit(t, s4, p.ID)); n2 != n1 {
		t.Fatalf("idempotent deactivate after reopen added events: %d -> %d", n1, n2)
	}
}

func TestSecondOpenSameDirRejected(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s1.Close() })
	s2, err := Open(dir)
	if err == nil {
		_ = s2.Close()
		t.Fatal("second concurrent Open must fail")
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	// 关闭后同位置可再次打开。
	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	_ = s3.Close()
}

func TestOperationsAfterCloseFail(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterPatient(doc, "x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("register after close err = %v", err)
	}
}
