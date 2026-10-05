package clinical

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“按患者查看审计历史”的返回结果隔离约定补回归保障：
// 内部使用者通过 AuditEvents 或 Chart 拿到审计列表后，可以在自己的程序里
// 整理这些结果；这种本地整理不能成为修改正式审计历史的另一条入口。
//
// 覆盖的约定：
//   - 调用方改写返回事件的操作人、动作、对象、所属患者或时间，替换、调换、
//     删除列表元素后，再次查询仍得到原来的事件标识、内容、数量与追加次序；
//   - 较早取得的另一份结果保持原样（同一入口多份，以及 AuditEvents 与
//     Chart 两个入口之间都一样）；
//   - 事件时间相同时仍保留真实操作的先后关系；
//   - 本地改动不产生审计事件；随后的合法更正在正式历史末尾追加真实事件，
//     不继承被改过的本地值，旧查询结果仍停留在查询当时；
//   - 停用后内部使用者仍可查看历史且隔离要求不变；持有效内容授权的接收方
//     读取审计列表一律 ErrAccessDenied，且不携带审计身份或对象信息。
//
// 预期基线由测试自行拷贝（刻意不调用生产代码的任何拷贝逻辑）：即便生产侧
// 退化为返回内部切片或指针，基线仍保持查询时内容，隔离断言才能真正抓住泄漏。

// ---- 测试自用的独立拷贝与比对（不经过生产代码） ----

// snapshotAudit 独立拷贝整份审计列表作为预期基线。AuditEvent 为纯值类型，
// 逐元素复制即与存储侧完全脱离。
func snapshotAudit(evs []AuditEvent) []AuditEvent {
	out := make([]AuditEvent, len(evs))
	copy(out, evs)
	return out
}

// tamperAuditEvent 在调用方手中的一条返回事件上改写全部可写字段：
// 事件标识、所属患者、操作人、动作、对象类别与标识、发生时间。
func tamperAuditEvent(ev *AuditEvent) {
	ev.ID = "aud_local_tampered"
	ev.PatientID = "pat_local_tampered"
	ev.ActorID = "actor_local_tampered"
	ev.Action = "local_forged_action"
	ev.ObjectType = "local_forged_object"
	ev.ObjectID = "obj_local_tampered"
	ev.OccurredAt = time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)
}

// forgedAuditEvent 构造一条完全本地伪造的事件，用于替换列表元素。
func forgedAuditEvent() AuditEvent {
	return AuditEvent{
		ID:         "aud_local_forged",
		PatientID:  "pat_local_forged",
		ActorID:    "actor_local_forged",
		Action:     "forged_action",
		ObjectType: "forged_object",
		ObjectID:   "obj_local_forged",
		OccurredAt: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func assertAuditMatches(t *testing.T, got, want []AuditEvent, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: event count changed: got %d (%v), want %d (%v)",
			label, len(got), auditIDList(got), len(want), auditIDList(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: event %d changed:\n got=%+v\nwant=%+v", label, i, got[i], want[i])
		}
	}
}

func auditIDList(evs []AuditEvent) []ID {
	ids := make([]ID, len(evs))
	for i, ev := range evs {
		ids[i] = ev.ID
	}
	return ids
}

// scrambleAuditResult 在调用方手中的一份结果上做尽可能广泛的本地整理：
// 逐事件改写全部字段、用伪造事件替换一个元素、首尾调换、删除末尾元素。
func scrambleAuditResult(evs []AuditEvent) []AuditEvent {
	for i := range evs {
		tamperAuditEvent(&evs[i])
	}
	if len(evs) >= 2 {
		evs[1] = forgedAuditEvent()
		evs[0], evs[len(evs)-1] = evs[len(evs)-1], evs[0]
	}
	return evs[:len(evs)-1]
}

// ---- 形态：多次生效/更正/授权/撤回留下可区分的事件；时间相同也保持追加次序 ----

func TestAuditEventsShapeAndAppendOrder(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	// 两名内部使用者交替操作，动作、对象、时间交错，足以区分每条事件。
	recA := mustCreateDraft(t, s, pid, eid, Diagnosis, "诊断第 1 版内容")
	recB := mustCreateDraft(t, s, pid, eid, Order, "医嘱第 1 版内容")

	t0 := clk.t
	if _, err := s.ActivateRecord(doc, recA); err != nil {
		t.Fatal(err)
	}
	// 与上一事件同一时刻：时间相同也必须保留真实操作的先后关系。
	if _, err := s.ActivateRecord(doc2, recB); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	t1 := clk.t
	if _, err := s.CorrectRecord(doc2, recA, 1, "诊断第 2 版内容", "诊断更正原因"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CorrectRecord(doc, recB, 1, "医嘱第 2 版内容", "医嘱更正原因"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	t2 := clk.t
	auth, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	t3 := clk.t
	if err := s.Revoke(doc2, pid, auth.ID); err != nil {
		t.Fatal(err)
	}

	events := mustAudit(t, s, pid)
	want := []struct {
		action, objectType, objectID, actorID string
		at                                    time.Time
	}{
		{ActionActivated, "record", recA, doc.ID, t0},
		{ActionActivated, "record", recB, doc2.ID, t0},
		{ActionCorrected, "record", recA, doc2.ID, t1},
		{ActionCorrected, "record", recB, doc.ID, t1},
		{ActionGranted, "authorization", auth.ID, doc.ID, t2},
		{ActionRevoked, "authorization", auth.ID, doc2.ID, t3},
	}
	if len(events) != len(want) {
		t.Fatalf("event count = %d (%v), want %d", len(events), auditIDList(events), len(want))
	}
	seenIDs := map[ID]bool{}
	for i, w := range want {
		ev := events[i]
		if ev.Action != w.action || ev.ObjectType != w.objectType || ev.ObjectID != w.objectID ||
			ev.ActorID != w.actorID || ev.PatientID != pid {
			t.Fatalf("event %d = %+v, want action %s on %s by %s", i, ev, w.action, w.objectID, w.actorID)
		}
		if !ev.OccurredAt.Equal(w.at) {
			t.Fatalf("event %d time = %v, want %v", i, ev.OccurredAt, w.at)
		}
		if ev.ID == "" || seenIDs[ev.ID] {
			t.Fatalf("event %d id missing or duplicated: %q", i, ev.ID)
		}
		seenIDs[ev.ID] = true
	}
	// 时间相同的两对事件必须按真实操作顺序相邻出现（上面已按位置断言，
	// 这里再显式确认它们的时间戳确实相同，保证该场景被覆盖）。
	if !events[0].OccurredAt.Equal(events[1].OccurredAt) || !events[2].OccurredAt.Equal(events[3].OccurredAt) {
		t.Fatal("test setup should produce same-timestamp event pairs")
	}

	// Chart 入口看到同一份历史。
	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	assertAuditMatches(t, chart.AuditEvents, events, "chart audit view")
}

// ---- 隔离：本地整理改不到正式历史，也改不到另一份结果（两个入口都适用） ----

func TestAuditEventsResultIsDetachedFromStore(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	recA := mustCreateDraft(t, s, pid, eid, Diagnosis, "诊断第 1 版内容")
	recB := mustCreateDraft(t, s, pid, eid, Order, "医嘱第 1 版内容")
	mustActivate(t, s, recA)
	if _, err := s.ActivateRecord(doc2, recB); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	mustCorrect(t, s, recA, 1, "诊断第 2 版内容", "诊断更正原因")
	if _, err := s.CorrectRecord(doc2, recB, 1, "医嘱第 2 版内容", "医嘱更正原因"); err != nil {
		t.Fatal(err)
	}

	// 同一入口取得两份结果；另一入口取得一份。基线独立拷贝。
	first := mustAudit(t, s, pid)
	second := mustAudit(t, s, pid)
	chartBefore, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	viaChart := chartBefore.AuditEvents
	want := snapshotAudit(first)
	if len(want) != 4 {
		t.Fatalf("setup should produce 4 events, got %d", len(want))
	}

	// 在第一份结果上广泛本地整理：改写字段、替换元素、调换、删除。
	first = scrambleAuditResult(first)

	// 同一入口再次查询：正式历史保持原标识、内容、数量与追加次序。
	again := mustAudit(t, s, pid)
	assertAuditMatches(t, again, want, "AuditEvents re-query after local tampering")

	// 同一入口较早取得的另一份结果保持原样。
	assertAuditMatches(t, second, want, "earlier AuditEvents result after local tampering")

	// 另一入口较早取得的结果保持原样；重新查询同样不受影响。
	assertAuditMatches(t, viaChart, want, "earlier Chart result after local tampering")
	chartAfter, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	assertAuditMatches(t, chartAfter.AuditEvents, want, "Chart re-query after local tampering")

	// 反过来也成立：篡改从 Chart 取得的结果，AuditEvents 入口不受波及。
	tamperedChart := mustAudit(t, s, pid) // 先记录当前正式历史
	chartView, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	scrambleAuditResult(chartView.AuditEvents)
	assertAuditMatches(t, mustAudit(t, s, pid), tamperedChart, "AuditEvents after tampering a Chart result")

	// 本地整理既不是查询也不是写操作：不能增加、更换或删除正式审计事件。
	assertAuditMatches(t, mustAudit(t, s, pid), want, "official audit history unchanged by local edits")
}

// ---- 合法更正：正式历史末尾追加真实事件；旧结果停留在查询当时 ----

func TestAuditCorrectionAppendsRealEventAfterLocalTampering(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	rid := mustCreateDraft(t, s, pid, eid, Diagnosis, "诊断第 1 版内容")
	mustActivate(t, s, rid)
	clk.t = clk.t.Add(time.Hour)
	mustCorrect(t, s, rid, 1, "诊断第 2 版内容", "第一次更正原因")

	// 更正前取得、此后不在本地改动的结果：应一直代表查询当时的历史。
	frozen := mustAudit(t, s, pid)
	if len(frozen) != 2 {
		t.Fatalf("setup should produce 2 events, got %d", len(frozen))
	}

	// 另一份结果被调用方任意改写（操作人、对象、患者、时间全部换成本地值）。
	local := mustAudit(t, s, pid)
	tamperAuditEvent(&local[0])

	// 通过正常入口完成一次合法更正。
	clk.t = clk.t.Add(time.Hour)
	correctionTime := clk.t
	v3, err := s.CorrectRecord(doc2, rid, 2, "诊断第 3 版内容", "第二次更正原因")
	if err != nil {
		t.Fatalf("legitimate correction: %v", err)
	}

	// 正式历史：此前事件原样保留，末尾追加本次更正的真实事件。
	after := mustAudit(t, s, pid)
	if len(after) != 3 {
		t.Fatalf("correction should append exactly one event, got %d (%v)", len(after), auditIDList(after))
	}
	if after[0] != frozen[0] || after[1] != frozen[1] {
		t.Fatalf("prior official history changed:\n got=%+v\nwant=%+v", after[:2], frozen)
	}
	ev := after[2]
	if ev.Action != ActionCorrected || ev.ObjectType != "record" || ev.ObjectID != rid {
		t.Fatalf("new event must point at the actual record: %+v", ev)
	}
	if ev.ActorID != doc2.ID || ev.PatientID != pid {
		t.Fatalf("new event must name the actual actor and patient: %+v", ev)
	}
	if !ev.OccurredAt.Equal(correctionTime) {
		t.Fatalf("new event time = %v, want the real operation time %v", ev.OccurredAt, correctionTime)
	}
	if ev.ID == "" || ev.ID == frozen[0].ID || ev.ID == frozen[1].ID {
		t.Fatalf("new event id missing or reused: %q", ev.ID)
	}
	// 新事件的任何字段都不能继承被改过的本地值。
	forged := local[0]
	if ev.ID == forged.ID || ev.ActorID == forged.ActorID || ev.Action == forged.Action ||
		ev.ObjectType == forged.ObjectType || ev.ObjectID == forged.ObjectID ||
		ev.PatientID == forged.PatientID || ev.OccurredAt.Equal(forged.OccurredAt) {
		t.Fatalf("new event inherited tampered local values:\n got=%+v\nlocal=%+v", ev, forged)
	}
	_ = v3

	// 原来的查询结果仍代表查询当时的历史，不会自动多出新事件。
	if len(frozen) != 2 {
		t.Fatalf("earlier result gained the new event: %d", len(frozen))
	}
	// 被本地改过的那份结果保持调用方留下的样子，不被正式状态回写。
	if local[0].ActorID != "actor_local_tampered" || local[0].ObjectID != "obj_local_tampered" {
		t.Fatalf("locally edited result changed unexpectedly: %+v", local[0])
	}
}

// ---- 停用后：内部查看与隔离要求不变；接收方持有效授权也不得读取审计列表 ----

func TestAuditIsolationAfterDeactivation(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	rid := mustCreateDraft(t, s, pid, eid, Diagnosis, "诊断第 1 版内容")
	mustActivate(t, s, rid)
	mustCorrect(t, s, rid, 1, "诊断第 2 版内容", "更正原因")

	// 接收方持有覆盖该患者内容的有效授权（停用不撤回授权）。
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(time.Hour)); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// 停用后内部使用者仍可按现有权限查看完整历史。
	baseline := mustAudit(t, s, pid)
	if got := auditActions(baseline); got[ActionActivated] != 1 || got[ActionCorrected] != 1 ||
		got[ActionGranted] != 1 || got[ActionDeactivated] != 1 {
		t.Fatalf("deactivated chart history incomplete: %v", got)
	}
	want := snapshotAudit(baseline)

	// 隔离要求继续成立：本地整理改不到正式历史，也改不到另一份结果。
	first := mustAudit(t, s, pid)
	second := mustAudit(t, s, pid)
	scrambleAuditResult(first)
	assertAuditMatches(t, mustAudit(t, s, pid), want, "AuditEvents after tampering (deactivated)")
	assertAuditMatches(t, second, want, "earlier result after tampering (deactivated)")
	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	assertAuditMatches(t, chart.AuditEvents, want, "Chart audit view (deactivated)")

	// 接收方即使持有该患者的有效内容授权，也不能读取审计列表：
	// 返回 ErrAccessDenied，且不携带审计身份或对象信息。
	evs, err := s.AuditEvents(rcv, pid)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver AuditEvents err = %v, want ErrAccessDenied", err)
	}
	if evs != nil {
		t.Fatalf("denied audit query must carry no events, got %+v", evs)
	}
	ch, err := s.Chart(rcv, pid)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver Chart err = %v, want ErrAccessDenied", err)
	}
	if ch.Patient != (Patient{}) || ch.Encounters != nil || ch.Records != nil || ch.AuditEvents != nil {
		t.Fatalf("denied chart query must carry no audit identity or object info, got %+v", ch)
	}
}
