package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestDeactivationSaveFailureKeepsAccessAndHistory 覆盖“内部使用者对一份尚未停用
// 的合成患者档案提交停用，身份与患者标识均合法，仅因本地保存失败而未能完成”的
// 档案停用场景。
//
// 前置数据：档案已有一次就诊；该就诊下有一条仍可修改的草稿，以及一条已生效且
// 有更正历史的诊断（第 1 版经更正得到当前第 2 版，旧版本、PrevID 版本链与更正
// 原因都在）。接收方持有覆盖这次就诊全部诊断的有效整类授权，原本能读到更正后的
// 当前版本，看不到草稿、旧版本或更正原因。授权时间窗覆盖失败尝试与后续成功提交
// 的全部时刻且始终未撤回，使读取变化只能来自档案停用结果本身。
//
// 本地保存失败时：停用必须以普通保存错误返回，不能报告成功，也不能误报为
// ErrAccessDenied、ErrNotFound 或 ErrDeactivated；内部查询仍显示患者未停用，
// 就诊归属、草稿完整内容、生效记录的当前版本与旧版本链、更正原因均与提交前
// 相同；审计内容及排列顺序原样保留，不出现停用事件；接收方随后仍能读取原来的
// 当前版本（记录标识、版本标识与内容不变），仍看不到草稿、旧版本或更正原因。
//
// 保存条件恢复后：同一患者可以正常提交停用。内部查询显示停用，并且相对提交前
// 只增加一条停用审计，准确记录患者、操作身份与这次成功操作的时间；原有审计不
// 被替换或重新排列。接收方随后读取同一就诊、同一类别时返回 ErrAccessDenied，
// 结果不携带记录标识或内容，原授权仍存在也不能继续读取。内部使用者依旧能查看
// 原有草稿及完整版本历史，停用不会覆盖或删除它们。
func TestDeactivationSaveFailureKeepsAccessAndHistory(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "停用保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 同一次就诊下一条仍是草稿的诊断。
	draftContent := "尚未生效的草稿内容"
	draft, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, draftContent)
	if err != nil {
		t.Fatal(err)
	}
	// 同一次就诊下一条已生效的诊断，随后更正一次：当前为第 2 版。
	r, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "初诊：高血压 I10")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	v2, err := s.CorrectRecord(doc, r.ID, 1, "复诊：高血压 I10（控制稳定）", "复核更新")
	if err != nil {
		t.Fatal(err)
	}
	// 接收方持有覆盖这次就诊全部诊断的整类授权。时间窗覆盖失败尝试与后续
	// 成功提交的全部时刻，且全程不撤回：读取变化只能来自停用结果本身。
	auth, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-2*time.Hour), clk.t.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// ---- 提交前的正式基线 ----
	patientBefore, err := s.GetPatient(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if patientBefore.Deactivated {
		t.Fatal("baseline patient must not be deactivated")
	}
	encountersBefore, err := s.ListEncounters(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	draftHistBefore := findHistory(chartBefore, draft.ID)
	if draftHistBefore == nil || !draftHistBefore.HasDraft ||
		draftHistBefore.DraftContent != draftContent || draftHistBefore.CurrentVersion != nil ||
		len(draftHistBefore.Versions) != 0 {
		t.Fatalf("unexpected baseline draft history: %+v", draftHistBefore)
	}
	activeHistBefore := findHistory(chartBefore, r.ID)
	if activeHistBefore == nil || activeHistBefore.CurrentVersion == nil ||
		activeHistBefore.CurrentVersion.ID != v2.ID || len(activeHistBefore.Versions) != 2 {
		t.Fatalf("unexpected baseline active history: %+v", activeHistBefore)
	}
	authBefore, err := s.GetAuthorization(doc, p.ID, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore); got[ActionActivated] != 1 ||
		got[ActionCorrected] != 1 || got[ActionGranted] != 1 || got[ActionDeactivated] != 0 {
		t.Fatalf("unexpected baseline audit: %v", got)
	}
	// 接收方原本能读到更正后的当前版本：记录标识、版本标识与内容均为第 2 版，
	// 结果里没有草稿、旧版本或更正原因（EffectiveRecord 本身不含这些）。
	readBefore, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if len(readBefore.Records) != 1 {
		t.Fatalf("baseline read = %+v, want exactly the current version", readBefore)
	}
	cur := readBefore.Records[0]
	if cur.RecordID != r.ID || cur.VersionID != v2.ID || cur.Version != 2 ||
		cur.Content != v2.Content || !cur.EffectiveAt.Equal(v2.CreatedAt) {
		t.Fatalf("baseline read does not expose the corrected current version: %+v", cur)
	}
	if readRecordIDs(readBefore)[draft.ID] {
		t.Fatal("baseline read must not expose the draft")
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），档案停用状态、就诊归属、
	// 草稿完整内容、生效记录的当前版本与旧版本链、更正原因、审计与接收方可见
	// 结果全部与提交前逐项一致——失败尝试不能提前改变内部历史或接收方访问。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		// 内部查询仍显示患者未停用，患者档案本身其他字段不变。
		patient, err := st.GetPatient(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: get patient: %v", label, err)
		}
		if patient.Deactivated {
			t.Fatalf("%s: patient must not be deactivated after failed save", label)
		}
		wantPatient := patientBefore
		wantPatient.Deactivated = false
		if !reflect.DeepEqual(patient, wantPatient) {
			t.Fatalf("%s: patient changed:\nbefore: %+v\nafter:  %+v", label, wantPatient, patient)
		}

		// 就诊归属与完整档案（草稿、当前版本、旧版本链、更正原因）原样保留。
		encs, err := st.ListEncounters(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list encounters: %v", label, err)
		}
		if !reflect.DeepEqual(encs, encountersBefore) {
			t.Fatalf("%s: encounters changed:\nbefore: %+v\nafter:  %+v", label, encountersBefore, encs)
		}
		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		if !reflect.DeepEqual(chart.Encounters, chartBefore.Encounters) ||
			!reflect.DeepEqual(chart.Records, chartBefore.Records) {
			t.Fatalf("%s: internal history changed:\nbefore: %+v\nafter:  %+v",
				label, chartBefore, chart)
		}
		dh := findHistory(chart, draft.ID)
		if dh == nil || dh.Record.EncounterID != e.ID || dh.Record.PatientID != p.ID ||
			dh.Record.Category != Diagnosis || !dh.HasDraft || dh.DraftContent != draftContent ||
			dh.CurrentVersion != nil || len(dh.Versions) != 0 {
			t.Fatalf("%s: draft no longer intact: %+v", label, dh)
		}
		ah := findHistory(chart, r.ID)
		if ah == nil || ah.Record.EncounterID != e.ID || ah.Record.PatientID != p.ID ||
			ah.CurrentVersion == nil || ah.CurrentVersion.ID != v2.ID ||
			!reflect.DeepEqual(ah.Record.Versions, []ID{v1.ID, v2.ID}) || len(ah.Versions) != 2 {
			t.Fatalf("%s: effective record history no longer intact: %+v", label, ah)
		}
		// 旧版本内容、PrevID 版本链与两版的更正原因均与提交前相同。
		if !reflect.DeepEqual(ah.Versions[0], v1) || !reflect.DeepEqual(ah.Versions[1], v2) ||
			ah.Versions[1].PrevID != v1.ID || ah.Versions[1].Reason != v2.Reason {
			t.Fatalf("%s: version chain or reasons changed: %+v", label, ah.Versions)
		}

		// 审计内容及排列顺序原样保留，不出现停用事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		if got := auditActions(audit)[ActionDeactivated]; got != 0 {
			t.Fatalf("%s: patient_deactivated events = %d, want 0", label, got)
		}

		// 原授权仍存在、未撤回且在当前时刻有效：读取变化不能由授权状态解释。
		gotAuth, err := st.GetAuthorization(doc, p.ID, auth.ID)
		if err != nil {
			t.Fatalf("%s: get authorization: %v", label, err)
		}
		if !reflect.DeepEqual(gotAuth, authBefore) || gotAuth.RevokedAt != nil {
			t.Fatalf("%s: authorization changed:\nbefore: %+v\nafter:  %+v", label, authBefore, gotAuth)
		}
		if !gotAuth.ActiveAt(clk.t) {
			t.Fatalf("%s: authorization must stay active at %v: %+v", label, clk.t, gotAuth)
		}

		// 接收方随后仍能读取原来的当前版本：记录标识、版本标识与内容不变；
		// 仍看不到草稿、旧版本或更正原因。
		res, err := st.Read(rcv, p.ID, e.ID, Diagnosis)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}
		if len(res.Records) != 1 || res.Records[0].RecordID != r.ID ||
			res.Records[0].VersionID != v2.ID || res.Records[0].Content != v2.Content {
			t.Fatalf("%s: receiver no longer sees exactly the corrected current version: %+v", label, res)
		}
		if readRecordIDs(res)[draft.ID] || res.Records[0].Version != 2 {
			t.Fatalf("%s: receiver read leaks draft or old version: %+v", label, res)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、患者标识合法性或患者状态无关。原数据文件先挪到旁边，事后还原。
	failTime := clk.t.Add(time.Hour)
	clk.t = failTime
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 身份有权、患者存在且未停用，仅本地保存失败：必须返回保存错误，不能报告
	// 成功，也不能误报为 ErrAccessDenied、ErrNotFound 或 ErrDeactivated。
	if err := s.DeactivatePatient(doc, p.ID); err == nil {
		t.Fatal("deactivation must fail when local save fails")
	} else if errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrMismatchedPatient) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：停用状态、内部历史、审计与接收方访问全部维持提交前状态。
	checkIntact("after failed save", s)

	// 关闭后从原数据位置重新打开：先恢复保存条件（还原原数据文件），
	// 失败后的未停用状态在重开后保持一致。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dataPath); err != nil {
		t.Fatalf("unblock data path: %v", err)
	}
	if err := os.Rename(backupPath, dataPath); err != nil {
		t.Fatalf("restore data file: %v", err)
	}
	s2 := open()
	checkIntact("after reopen", s2)

	// 保存条件恢复后：同一患者可以正常提交停用。停用时间属于这次成功操作，
	// 不能沿用失败尝试的时间。
	successTime := failTime.Add(time.Hour)
	clk.t = successTime
	if err := s2.DeactivatePatient(doc, p.ID); err != nil {
		t.Fatalf("deactivate after save recovered: %v", err)
	}

	// 内部查询显示停用。
	patient, err := s2.GetPatient(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !patient.Deactivated {
		t.Fatal("patient must be deactivated after successful submit")
	}

	// 相对提交前只增加一条停用审计：准确记录患者、操作身份与成功操作的时间；
	// 原有审计不被替换或重新排列。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after successful deactivation:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionDeactivated || last.ObjectType != "patient" || last.ObjectID != p.ID ||
		last.ActorID != doc.ID || last.PatientID != p.ID || !last.OccurredAt.Equal(successTime) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if last.OccurredAt.Equal(failTime) {
		t.Fatalf("deactivation audit time %v must not belong to the failed attempt %v",
			last.OccurredAt, failTime)
	}
	if got := auditActions(audit)[ActionDeactivated]; got != 1 {
		t.Fatalf("patient_deactivated events = %d, want 1", got)
	}

	// 接收方随后读取同一就诊、同一类别：ErrAccessDenied，结果不携带记录标识
	// 或内容；原授权仍存在（未撤回、仍在有效期）也不能继续读取。
	res, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver read after deactivation: res=%+v err=%v, want ErrAccessDenied", res, err)
	}
	if res.EncounterID != "" || res.Category != "" || len(res.Records) != 0 {
		t.Fatalf("denied read must carry no record ids or content: %+v", res)
	}
	gotAuth, err := s2.GetAuthorization(doc, p.ID, auth.ID)
	if err != nil {
		t.Fatalf("authorization must still exist after deactivation: %v", err)
	}
	if gotAuth.RevokedAt != nil || !gotAuth.ActiveAt(successTime) {
		t.Fatalf("existing authorization must remain intact and active, yet reads stay denied: %+v", gotAuth)
	}
	if !reflect.DeepEqual(gotAuth.Scopes, authBefore.Scopes) ||
		!gotAuth.StartsAt.Equal(authBefore.StartsAt) || !gotAuth.ExpiresAt.Equal(authBefore.ExpiresAt) {
		t.Fatalf("authorization mutated by deactivation:\nbefore: %+v\nafter:  %+v",
			authBefore, gotAuth)
	}

	// 内部使用者依旧能查看原有草稿及完整版本历史：停用不覆盖、不删除它们。
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !chart.Patient.Deactivated {
		t.Fatal("chart should show deactivated")
	}
	if !reflect.DeepEqual(chart.Encounters, chartBefore.Encounters) ||
		!reflect.DeepEqual(chart.Records, chartBefore.Records) {
		t.Fatalf("internal history changed by deactivation:\nbefore: %+v\nafter:  %+v",
			chartBefore, chart)
	}
	dh := findHistory(chart, draft.ID)
	if dh == nil || !dh.HasDraft || dh.DraftContent != draftContent ||
		dh.Record.EncounterID != e.ID || dh.CurrentVersion != nil {
		t.Fatalf("draft lost after deactivation: %+v", dh)
	}
	ah := findHistory(chart, r.ID)
	if ah == nil || ah.CurrentVersion == nil || ah.CurrentVersion.ID != v2.ID ||
		len(ah.Versions) != 2 || !reflect.DeepEqual(ah.Versions[0], v1) ||
		!reflect.DeepEqual(ah.Versions[1], v2) || ah.Versions[1].PrevID != v1.ID {
		t.Fatalf("full version history lost after deactivation: %+v", ah)
	}

	// 成功停用后的既有规则保留：重复停用幂等，不产生额外停用事件。
	if err := s2.DeactivatePatient(doc, p.ID); err != nil {
		t.Fatalf("repeat deactivation must stay successful: %v", err)
	}
	if evs, err := s2.AuditEvents(doc, p.ID); err != nil || len(evs) != len(audit) {
		t.Fatalf("repeat deactivation added audit events: before=%d after=%d err=%v",
			len(audit), len(evs), err)
	}

	// 关闭后从同一数据位置重新打开：停用状态与唯一停用审计完整保留，接收方
	// 依旧被拒绝且不携带内容，内部草稿与完整版本历史仍在；失败尝试不作为额外
	// 的停用状态或事件出现。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })

	reloaded, err := s3.GetPatient(doc, p.ID)
	if err != nil {
		t.Fatalf("get patient after reopen: %v", err)
	}
	if !reloaded.Deactivated {
		t.Fatalf("deactivated state not preserved after reopen: %+v", reloaded)
	}
	if denied, err := s3.Read(rcv, p.ID, e.ID, Diagnosis); !errors.Is(err, ErrAccessDenied) ||
		len(denied.Records) != 0 || denied.EncounterID != "" || denied.Category != "" {
		t.Fatalf("read after reopen: res=%+v err=%v, want ErrAccessDenied without content",
			denied, err)
	}
	reloadedChart, err := s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloadedChart.Records, chartBefore.Records) {
		t.Fatalf("internal history not preserved after reopen:\nbefore: %+v\nafter:  %+v",
			chartBefore.Records, reloadedChart.Records)
	}
	var deactivateTimes []time.Time
	for _, ev := range mustAudit(t, s3, p.ID) {
		if ev.Action == ActionDeactivated {
			deactivateTimes = append(deactivateTimes, ev.OccurredAt)
		}
	}
	if len(deactivateTimes) != 1 || !deactivateTimes[0].Equal(successTime) {
		t.Fatalf("deactivation events after reopen = %v, want only [%v]",
			deactivateTimes, successTime)
	}
}
