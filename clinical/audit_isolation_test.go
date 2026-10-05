package clinical

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// 本文件为内部使用者按患者查看审计历史的“返回结果隔离”约定补回归保障：
// 内部使用者通过 AuditEvents 或 Chart 拿到审计列表后，可以在自己的程序里
// 整理这些结果；这种本地整理不能成为修改正式审计历史的另一条入口。
// 无论改动某条事件的操作身份、动作、对象标识、所属患者或时间，还是替换、
// 调换、删除列表元素，再次查询都应得到原来的事件标识、内容、数量与追加
// 次序；较早取得的另一份结果也保持原样。本地改动不产生审计事件；随后的
// 合法更正只在正式历史末尾追加真实事件，不继承任何被改过的本地值。
//
// 预期基线由测试自行拷贝（刻意不调用生产代码的任何拷贝逻辑）：即便生产
// 侧查询退化为共享内部状态，基线仍保持查询时内容，隔离断言才能真正抓住
// 正式历史被本地整理污染的回归。

// ---- 测试自用的独立拷贝（不经过生产代码） ----

// snapshotAudit 独立拷贝整份审计列表作为预期基线。
// AuditEvent 只含值字段，逐元素复制即为真正的深拷贝。
func snapshotAudit(evs []AuditEvent) []AuditEvent {
	out := make([]AuditEvent, len(evs))
	copy(out, evs)
	return out
}

// tamperAuditEvents 在调用方手中的结果上做尽可能广泛的本地修改：
// 逐条改写操作身份、动作、对象类型、对象标识、所属患者与时间；
// 整体替换其中一条为伪造事件；就地调换首尾元素；并截掉末尾一条。
func tamperAuditEvents(evs []AuditEvent) []AuditEvent {
	for i := range evs {
		evs[i].ID = ID(fmt.Sprintf("aud_local_tampered_%d", i))
		evs[i].PatientID = "pat_local_tampered"
		evs[i].ActorID = "actor_local_tampered"
		evs[i].Action = "local_forged_action"
		evs[i].ObjectType = "local_forged_type"
		evs[i].ObjectID = "obj_local_tampered"
		evs[i].OccurredAt = time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)
	}
	if len(evs) > 1 {
		// 整体替换一条为本地伪造的事件。
		evs[1] = AuditEvent{
			ID:         "aud_local_replacement",
			PatientID:  "pat_local_replacement",
			ActorID:    "actor_local_replacement",
			Action:     "local_replacement_action",
			ObjectType: "local_replacement_type",
			ObjectID:   "obj_local_replacement",
			OccurredAt: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		// 就地调换首尾元素：与正式历史共享底层数组时会打乱正式次序。
		evs[0], evs[len(evs)-1] = evs[len(evs)-1], evs[0]
	}
	// 删除末尾元素（对调用方手中的切片生效）。
	if len(evs) > 0 {
		evs = evs[:len(evs)-1]
	}
	return evs
}

func assertAuditEventsMatch(t *testing.T, got, want []AuditEvent, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: audit event count changed: got %d (%v), want %d (%v)",
			label, len(got), auditIDList(got), len(want), auditIDList(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: audit event %d changed:\n got=%+v\nwant=%+v",
				label, i, got[i], want[i])
		}
	}
}

func auditIDList(evs []AuditEvent) []ID {
	ids := make([]ID, len(evs))
	for i, e := range evs {
		ids[i] = e.ID
	}
	return ids
}

// ---- 合成患者：多次生效、更正、授权与撤回，事件的操作人、动作、对象与时间足以区分 ----

// setupAuditedPatient 建立一名带多条可区分审计事件的合成患者：
// 两次生效、一次更正、一次授权与一次撤回，交替由两名内部使用者操作，
// 每步推进时钟使各事件时间互不相同。返回患者标识、被更正记录的标识与授权标识。
func setupAuditedPatient(t *testing.T, s *Store, clk *fakeClock) (pid, recID, authID ID) {
	t.Helper()
	p, err := s.RegisterPatient(doc, "合成患者甲")
	if err != nil {
		t.Fatalf("register patient: %v", err)
	}
	enc, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatalf("add encounter: %v", err)
	}

	r1 := mustCreateDraft(t, s, p.ID, enc.ID, Diagnosis, "诊断第 1 版内容")
	r2 := mustCreateDraft(t, s, p.ID, enc.ID, Order, "医嘱第 1 版内容")

	clk.t = clk.t.Add(time.Hour)
	if _, err := s.ActivateRecord(doc, r1); err != nil {
		t.Fatalf("activate r1: %v", err)
	}
	clk.t = clk.t.Add(time.Hour)
	if _, err := s.CorrectRecord(doc2, r1, 1, "诊断第 2 版内容", "更正原因"); err != nil {
		t.Fatalf("correct r1: %v", err)
	}
	clk.t = clk.t.Add(time.Hour)
	if _, err := s.ActivateRecord(doc2, r2); err != nil {
		t.Fatalf("activate r2: %v", err)
	}
	clk.t = clk.t.Add(time.Hour)
	auth, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: enc.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	clk.t = clk.t.Add(time.Hour)
	if err := s.Revoke(doc2, p.ID, auth.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	return p.ID, r1, auth.ID
}

// ---- 隔离：在手中的结果上整理，改不到正式历史，也改不到另一份结果 ----

func TestAuditEventsResultIsDetachedFromStore(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, _ := setupAuditedPatient(t, s, clk)

	// 基线：测试自行拷贝，不依赖生产代码的拷贝行为。
	first, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 5 {
		t.Fatalf("expected 5 audit events, got %d (%v)", len(first), auditIDList(first))
	}
	want := snapshotAudit(first)

	// 同一入口取得的第二份结果，以及从 Chart 入口取得的一份，都在本地改动前取得。
	second, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	chartBefore, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}

	// 在第一份结果上做广泛的本地修改：改字段、替换、调换、删除。
	tamperAuditEvents(first)
	// 从 Chart 取得的那份也同样在本地改一遍。
	tamperAuditEvents(chartBefore.AuditEvents)

	// 再次查询：正式历史保持原来的事件标识、内容、数量与追加次序。
	again, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	assertAuditEventsMatch(t, again, want, "AuditEvents re-query after tampering")

	// 较早取得的另一份结果保持原样。
	assertAuditEventsMatch(t, second, want, "earlier AuditEvents result after tampering")

	// Chart 入口重新查询同样不受两个入口的本地改动影响。
	chartAfter, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	assertAuditEventsMatch(t, chartAfter.AuditEvents, want, "Chart re-query after tampering")

	// 事件内容本身可区分：操作人、动作、对象与时间没有被本地值污染。
	actions := auditActions(again)
	if actions[ActionActivated] != 2 || actions[ActionCorrected] != 1 ||
		actions[ActionGranted] != 1 || actions[ActionRevoked] != 1 {
		t.Fatalf("audit action mix wrong after tampering: %v", actions)
	}
	for _, ev := range again {
		if ev.PatientID != pid {
			t.Fatalf("event belongs to wrong patient: %+v", ev)
		}
		if ev.ActorID != doc.ID && ev.ActorID != doc2.ID {
			t.Fatalf("event actor polluted by local value: %+v", ev)
		}
	}
}

// ---- 事件时间相同时，仍保留真实操作的先后（追加）次序 ----

func TestAuditEventsKeepAppendOrderWhenTimestampsEqual(t *testing.T) {
	s, _ := newTestStore(t) // 不推进时钟：所有操作共享同一时间戳。
	pid, eid := setupPatientEncounter(t, s)

	r1 := mustCreateDraft(t, s, pid, eid, Diagnosis, "诊断内容")
	r2 := mustCreateDraft(t, s, pid, eid, Order, "医嘱内容")
	if _, err := s.ActivateRecord(doc, r1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CorrectRecord(doc2, r1, 1, "诊断更正内容", "更正原因"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc2, r2); err != nil {
		t.Fatal(err)
	}

	events, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 audit events, got %d (%v)", len(events), auditIDList(events))
	}
	for _, ev := range events {
		if !ev.OccurredAt.Equal(events[0].OccurredAt) {
			t.Fatalf("test premise broken: timestamps differ: %+v", events)
		}
	}
	// 时间相同也不能打乱真实发生顺序：生效 r1 → 更正 r1 → 生效 r2。
	type step struct{ action, object string }
	wantOrder := []step{
		{ActionActivated, r1},
		{ActionCorrected, r1},
		{ActionActivated, r2},
	}
	for i, w := range wantOrder {
		if events[i].Action != w.action || events[i].ObjectID != w.object {
			t.Fatalf("append order lost at %d: got (%s,%s), want (%s,%s)",
				i, events[i].Action, events[i].ObjectID, w.action, w.object)
		}
	}

	// 本地调换次序后重新查询，正式次序仍保持。
	events[0], events[2] = events[2], events[0]
	again, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range wantOrder {
		if again[i].Action != w.action || again[i].ObjectID != w.object {
			t.Fatalf("append order changed by local swap at %d: got (%s,%s)",
				i, again[i].Action, again[i].ObjectID)
		}
	}
}

// ---- 本地改动不产生审计事件；合法更正只在末尾追加真实事件 ----

func TestAuditEventsLocalMutationDoesNotAppendAndCorrectionAppendsReal(t *testing.T) {
	s, clk := newTestStore(t)
	pid, rid, _ := setupAuditedPatient(t, s, clk)

	// 一份将被本地改乱，一份保持不动作为查询当时的历史。
	tampered, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	pristine, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotAudit(pristine)

	tamperAuditEvents(tampered)

	// 本地整理不是写操作：正式事件数量与内容都不变。
	afterTamper, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	assertAuditEventsMatch(t, afterTamper, want, "local tampering must not generate audit events")

	// 通过正常入口完成一次合法更正。
	clk.t = clk.t.Add(time.Hour)
	v3, err := s.CorrectRecord(doc2, rid, 2, "诊断第 3 版内容", "第二次更正原因")
	if err != nil {
		t.Fatalf("legitimate correction: %v", err)
	}

	// 正式历史保留此前全部事件，并在末尾追加这次更正的真实事件。
	after, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(want)+1 {
		t.Fatalf("correction should append exactly one event: got %d, want %d",
			len(after), len(want)+1)
	}
	assertAuditEventsMatch(t, after[:len(want)], want, "prior history preserved after correction")

	last := after[len(after)-1]
	if last.ID == "" || last.ID == "aud_local_tampered_0" || last.ID == "aud_local_replacement" {
		t.Fatalf("new event id missing or inherited from local tampering: %q", last.ID)
	}
	if last.Action != ActionCorrected || last.ObjectType != "record" || last.ObjectID != rid {
		t.Fatalf("new event must point to the actual record: %+v", last)
	}
	if last.ActorID != doc2.ID {
		t.Fatalf("new event actor = %q, want actual operator %q (not a tampered local value)",
			last.ActorID, doc2.ID)
	}
	if !last.OccurredAt.Equal(clk.t) || !last.OccurredAt.Equal(v3.CreatedAt) {
		t.Fatalf("new event time = %v, want actual operation time %v", last.OccurredAt, clk.t)
	}
	if last.PatientID != pid {
		t.Fatalf("new event patient = %q, want %q", last.PatientID, pid)
	}

	// 原来的查询结果仍代表查询当时的历史，不会自动多出这次新事件。
	assertAuditEventsMatch(t, pristine, want, "earlier result must not gain the new event")
}

// ---- 停用后的档案：内部仍可按现有权限查看历史且隔离要求不变；接收方一律拒绝 ----

func TestAuditEventsDeactivatedPatientIsolationAndReceiverDenied(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, _ := setupAuditedPatient(t, s, clk)

	// 为接收方建立一条当前有效的内容授权（不撤回），再停用档案。
	encs, err := s.ListEncounters(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: encs[0].ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !auth.ActiveAt(clk.t) {
		t.Fatal("test premise broken: receiver grant must be currently valid")
	}
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// 停用后内部使用者仍可查看历史。
	first, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatalf("internal audit query after deactivation: %v", err)
	}
	if got := auditActions(first)[ActionDeactivated]; got != 1 {
		t.Fatalf("deactivation event missing: %v", auditActions(first))
	}
	want := snapshotAudit(first)

	// 隔离要求在停用档案上继续成立。
	tamperAuditEvents(first)
	again, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	assertAuditEventsMatch(t, again, want, "deactivated patient audit re-query after tampering")
	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatalf("internal chart after deactivation: %v", err)
	}
	assertAuditEventsMatch(t, chart.AuditEvents, want, "deactivated patient chart after tampering")

	// 接收方即使持有该患者的有效内容授权，也不能读取审计列表：
	// 返回 ErrAccessDenied，且不携带审计身份或对象信息。
	events, err := s.AuditEvents(rcv, pid)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver audit query err = %v, want ErrAccessDenied", err)
	}
	if events != nil {
		t.Fatalf("denied audit query must carry no events, got %+v", events)
	}
	deniedChart, err := s.Chart(rcv, pid)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver chart err = %v, want ErrAccessDenied", err)
	}
	if deniedChart.AuditEvents != nil || deniedChart.Records != nil ||
		deniedChart.Encounters != nil || deniedChart.Patient != (Patient{}) {
		t.Fatalf("denied chart must carry no audit or record information, got %+v", deniedChart)
	}

	// 接收方的正常读取路径在档案停用后同样被拒绝（既有业务行为保持兼容）。
	if _, err := s.Read(rcv, pid, encs[0].ID, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver read on deactivated patient err = %v, want ErrAccessDenied", err)
	}
}
