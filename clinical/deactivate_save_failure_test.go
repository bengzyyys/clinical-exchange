package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestDeactivateSaveFailureKeepsStateAndAllowsRetry 覆盖“内部使用者对一份尚未
// 停用的合成患者档案提交停用，身份与患者标识均合法，却因本地保存失败而未能
// 完成”的档案停用场景。
//
// 前置数据：档案已有一次就诊；该就诊下有一条尚未生效的草稿，以及一条已生效、
// 且已有一次更正历史（当前为第 2 版）的诊断记录；接收方持有一条覆盖这次就诊
// 全部诊断的整类授权，时间窗覆盖失败尝试与后续成功提交的全部时刻且从未撤回，
// 因此原本能读到更正后的当前版本。读取变化只能来自档案停用结果本身。
//
// 本地保存失败时：停用必须以普通保存错误返回，不能报告成功，也不能伪装成
// 身份无权、患者不存在或档案已停用；内部查询仍显示患者未停用，就诊归属、
// 草稿完整内容、生效记录的当前版本与旧版本链、更正原因均与提交前相同；
// 既有审计的内容与排列顺序原样保留，不出现停用事件；接收方随后仍能读到
// 原来的当前版本，记录标识、版本标识与内容不变，仍看不到草稿、旧版本或
// 更正原因。
//
// 保存条件恢复后：同一患者可以正常提交停用。内部查询显示停用，且相对提交前
// 只增加一条停用审计，准确记录患者、操作身份与这次成功操作的时间；原有审计
// 不被替换或重新排列。接收方随后读取同一就诊、同一类别时返回 ErrAccessDenied，
// 结果不携带记录标识或内容，原授权仍存在也不能继续读取。内部使用者依旧能
// 查看原有草稿及完整版本历史，停用不会覆盖或删除它们。
func TestDeactivateSaveFailureKeepsStateAndAllowsRetry(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)}
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

	// 一条尚未生效的草稿：停用前后内部使用者都应看到它的完整内容。
	draft, err := s.CreateDraft(doc, p.ID, e.ID, Order, "草稿医嘱：复查血常规")
	if err != nil {
		t.Fatal(err)
	}

	// 一条已生效记录，随后成功更正一次：当前为第 2 版，保留旧版本与更正原因。
	r, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "初诊：高血压 I10")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	v2, err := s.CorrectRecord(doc, r.ID, v1.Number, "复诊：高血压 I10（控制稳定）", "复核更新")
	if err != nil {
		t.Fatal(err)
	}

	// 接收方的整类授权：覆盖这次就诊的全部诊断，时间窗覆盖失败尝试与后续
	// 成功提交的全部时刻，且全程不撤回——读取变化只能来自档案停用结果。
	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(48 * time.Hour)
	auth, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}}, start, end)
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
	if len(chartBefore.Records) != 2 {
		t.Fatalf("baseline chart records = %d, want 2 (draft + effective)", len(chartBefore.Records))
	}
	effHistBefore := findHistory(chartBefore, r.ID)
	draftHistBefore := findHistory(chartBefore, draft.ID)
	if effHistBefore == nil || effHistBefore.CurrentVersion == nil ||
		effHistBefore.CurrentVersion.ID != v2.ID || len(effHistBefore.Versions) != 2 {
		t.Fatalf("baseline effective history wrong: %+v", effHistBefore)
	}
	if draftHistBefore == nil || !draftHistBefore.HasDraft ||
		draftHistBefore.DraftContent != "草稿医嘱：复查血常规" ||
		draftHistBefore.CurrentVersion != nil || len(draftHistBefore.Versions) != 0 {
		t.Fatalf("baseline draft history wrong: %+v", draftHistBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore); got[ActionActivated] != 1 ||
		got[ActionCorrected] != 1 || got[ActionGranted] != 1 || got[ActionDeactivated] != 0 {
		t.Fatalf("unexpected baseline audit: %v", got)
	}
	authBefore, err := s.GetAuthorization(doc, p.ID, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	if authBefore.RevokedAt != nil || !authBefore.ActiveAt(clk.t) {
		t.Fatalf("baseline authorization must be active: %+v", authBefore)
	}
	// 接收方原本能读到更正后的当前版本。
	readBefore, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("baseline receiver read: %v", err)
	}
	if len(readBefore.Records) != 1 {
		t.Fatalf("baseline read = %+v, want exactly 1 effective diagnosis", readBefore)
	}
	beforeRec := readBefore.Records[0]
	if beforeRec.RecordID != r.ID || beforeRec.VersionID != v2.ID ||
		beforeRec.Version != 2 || beforeRec.Content != v2.Content ||
		!beforeRec.EffectiveAt.Equal(v2.CreatedAt) {
		t.Fatalf("baseline read exposes wrong current version: %+v", beforeRec)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），停用状态、就诊归属、
	// 草稿与完整版本历史、审计和接收方可见内容都与提交前逐项一致——停用状态、
	// 停用审计与接收方访问限制必须对应同一次成功操作，失败尝试不能提前阻断
	// 接收方访问，也不能在内部历史里留下看起来已经停用的痕迹。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		// 内部查询仍显示患者未停用。
		patient, err := st.GetPatient(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: get patient: %v", label, err)
		}
		if !reflect.DeepEqual(patient, patientBefore) {
			t.Fatalf("%s: patient changed:\nbefore: %+v\nafter:  %+v", label, patientBefore, patient)
		}
		if patient.Deactivated {
			t.Fatalf("%s: patient must not be deactivated", label)
		}

		// 就诊归属与排列不变。
		encs, err := st.ListEncounters(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list encounters: %v", label, err)
		}
		if !reflect.DeepEqual(encs, encountersBefore) {
			t.Fatalf("%s: encounters changed:\nbefore: %+v\nafter:  %+v", label, encountersBefore, encs)
		}

		// 完整档案：草稿完整内容、生效记录的当前版本、旧版本链与更正原因，
		// 全部与提交前相同。
		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		if !reflect.DeepEqual(chart.Records, chartBefore.Records) {
			t.Fatalf("%s: chart records changed:\nbefore: %+v\nafter:  %+v",
				label, chartBefore.Records, chart.Records)
		}
		if !reflect.DeepEqual(chart.Encounters, chartBefore.Encounters) {
			t.Fatalf("%s: chart encounters changed:\nbefore: %+v\nafter:  %+v",
				label, chartBefore.Encounters, chart.Encounters)
		}
		effHist := findHistory(chart, r.ID)
		if effHist == nil || effHist.CurrentVersion == nil {
			t.Fatalf("%s: effective history missing: %+v", label, effHist)
		}
		cur := effHist.CurrentVersion
		if cur.ID != v2.ID || cur.Number != 2 || cur.Content != v2.Content ||
			cur.PrevID != v1.ID || cur.Reason != v2.Reason ||
			!cur.CreatedAt.Equal(v2.CreatedAt) {
			t.Fatalf("%s: current version changed: %+v", label, cur)
		}
		if !reflect.DeepEqual(effHist.Record.Versions, []ID{v1.ID, v2.ID}) {
			t.Fatalf("%s: version chain changed: %v", label, effHist.Record.Versions)
		}
		if len(effHist.Versions) != 2 ||
			!reflect.DeepEqual(effHist.Versions[0], v1) ||
			!reflect.DeepEqual(effHist.Versions[1], v2) {
			t.Fatalf("%s: old versions or correction reason changed: %+v", label, effHist.Versions)
		}
		draftHist := findHistory(chart, draft.ID)
		if draftHist == nil || !draftHist.HasDraft ||
			draftHist.DraftContent != "草稿医嘱：复查血常规" ||
			draftHist.Record.EncounterID != e.ID || draftHist.Record.PatientID != p.ID {
			t.Fatalf("%s: draft changed: %+v", label, draftHist)
		}

		// 就诊视图与完整档案一致：草稿与完整版本历史都还在。
		er, err := st.EncounterRecords(doc, p.ID, e.ID)
		if err != nil {
			t.Fatalf("%s: encounter records: %v", label, err)
		}
		if len(er) != 2 {
			t.Fatalf("%s: encounter record count changed: %+v", label, er)
		}

		// 既有审计的内容与排列顺序原样保留，不出现停用事件。
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

		// 授权原样保留且仍在有效期内、未撤回。
		gotAuth, err := st.GetAuthorization(doc, p.ID, auth.ID)
		if err != nil {
			t.Fatalf("%s: get authorization: %v", label, err)
		}
		if !reflect.DeepEqual(gotAuth, authBefore) {
			t.Fatalf("%s: authorization changed:\nbefore: %+v\nafter:  %+v",
				label, authBefore, gotAuth)
		}
		if !gotAuth.ActiveAt(clk.t) {
			t.Fatalf("%s: authorization must stay active at %v: %+v", label, clk.t, gotAuth)
		}

		// 接收方仍能读到原来的当前版本：记录标识、版本标识与内容不变。
		res, err := st.Read(rcv, p.ID, e.ID, Diagnosis)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}
		// 接收方仍看不到草稿、旧版本或更正原因：结果中只有当前版本，
		// 且 EffectiveRecord 本身不携带更正原因。
		if len(res.Records) != 1 {
			t.Fatalf("%s: receiver read record count = %d, want 1", label, len(res.Records))
		}
		got := res.Records[0]
		if got.RecordID != r.ID || got.VersionID != v2.ID || got.Version != 2 ||
			got.Content != v2.Content {
			t.Fatalf("%s: receiver visible record changed: %+v", label, got)
		}
		// 同一就诊下的医嘱类别没有任何授权：草稿也不能借停用失败的乱局泄露。
		if orderRes, err := st.Read(rcv, p.ID, e.ID, Order); !errors.Is(err, ErrAccessDenied) ||
			len(orderRes.Records) != 0 || orderRes.EncounterID != "" {
			t.Fatalf("%s: order read = %+v err=%v, want ErrAccessDenied without content",
				label, orderRes, err)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、患者标识或业务规则无关。原数据文件先挪到旁边，事后还原。
	clk.t = clk.t.Add(time.Hour)
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 停用请求的身份和患者标识均合法，失败原因仅是本地保存无法完成：
	// 必须返回保存错误，不能报告成功，也不能误报为 ErrAccessDenied、
	// ErrNotFound 或 ErrDeactivated。
	if err := s.DeactivatePatient(doc, p.ID); err == nil {
		t.Fatal("deactivate must fail when local save fails")
	} else if errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrMismatchedPatient) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：停用状态、就诊、草稿、版本历史、审计与接收方读取全部维持
	// 提交前状态。
	checkIntact("after failed save", s)

	// 关闭后从原数据位置重新打开：先恢复保存条件（还原原数据文件）。
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
	t.Cleanup(func() { _ = s2.Close() })
	// 重开后仍是提交前的正式数据：失败的停用不留任何痕迹。
	checkIntact("after reopen", s2)

	// 保存条件恢复后：同一患者正常提交停用。时刻仍在授权有效期内、授权未
	// 撤回，因此随后的访问变化只能来自这次成功停用。
	successAt := clk.t.Add(time.Hour)
	clk.t = successAt
	if err := s2.DeactivatePatient(doc, p.ID); err != nil {
		t.Fatalf("retry after save recovered: %v", err)
	}

	// 内部查询显示停用。
	patientAfter, err := s2.GetPatient(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !patientAfter.Deactivated {
		t.Fatal("patient must be deactivated after successful retry")
	}
	if patientAfter.ID != patientBefore.ID || patientAfter.Name != patientBefore.Name ||
		patientAfter.Source != patientBefore.Source ||
		!patientAfter.CreatedAt.Equal(patientBefore.CreatedAt) {
		t.Fatalf("deactivation altered unrelated patient fields:\nbefore: %+v\nafter:  %+v",
			patientBefore, patientAfter)
	}

	// 审计相对提交前基线只新增一条停用事件，准确记录患者、操作身份与这次
	// 成功操作的时间；原有审计不被替换或重新排列。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after successful deactivate:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionDeactivated || last.ObjectType != "patient" ||
		last.ObjectID != p.ID || last.PatientID != p.ID ||
		last.ActorID != doc.ID || !last.OccurredAt.Equal(successAt) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if got := auditActions(audit)[ActionDeactivated]; got != 1 {
		t.Fatalf("patient_deactivated events = %d, want 1", got)
	}

	// 原授权仍存在（未撤回、时间窗仍有效），但患者停用后接收方读取同一就诊、
	// 同一类别必须返回 ErrAccessDenied，且结果不携带记录标识或内容。
	authAfter, err := s2.GetAuthorization(doc, p.ID, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	if authAfter.RevokedAt != nil || !authAfter.ActiveAt(clk.t) {
		t.Fatalf("authorization must still exist and be in window: %+v", authAfter)
	}
	denied, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("read after deactivate: result=%+v err=%v, want ErrAccessDenied", denied, err)
	}
	if denied.EncounterID != "" || denied.Category != "" || len(denied.Records) != 0 {
		t.Fatalf("denied read must carry no record ids or content: %+v", denied)
	}

	// 内部使用者依旧能查看原有草稿及完整版本历史：停用不覆盖、不删除它们。
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chart.Records) != 2 {
		t.Fatalf("records after deactivate = %d, want 2 preserved", len(chart.Records))
	}
	if !reflect.DeepEqual(chart.Records, chartBefore.Records) {
		t.Fatalf("internal records changed by deactivation:\nbefore: %+v\nafter:  %+v",
			chartBefore.Records, chart.Records)
	}
	effHist := findHistory(chart, r.ID)
	if effHist == nil || effHist.CurrentVersion == nil ||
		effHist.CurrentVersion.ID != v2.ID || len(effHist.Versions) != 2 ||
		effHist.Versions[0].ID != v1.ID || effHist.Versions[1].Reason != "复核更新" {
		t.Fatalf("effective history not preserved after deactivation: %+v", effHist)
	}
	draftHist := findHistory(chart, draft.ID)
	if draftHist == nil || !draftHist.HasDraft ||
		draftHist.DraftContent != "草稿医嘱：复查血常规" {
		t.Fatalf("draft not preserved after deactivation: %+v", draftHist)
	}

	// 就诊归属保持不变。
	encs, err := s2.ListEncounters(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(encs, encountersBefore) {
		t.Fatalf("encounters changed after deactivation:\nbefore: %+v\nafter:  %+v",
			encountersBefore, encs)
	}

	// 重复停用幂等：不产生额外变化，也不新增审计事件（沿用既有停用规则）。
	if err := s2.DeactivatePatient(doc, p.ID); err != nil {
		t.Fatalf("repeat deactivate must stay successful: %v", err)
	}
	if evs, err := s2.AuditEvents(doc, p.ID); err != nil || !reflect.DeepEqual(evs, audit) {
		t.Fatalf("repeat deactivate changed audit: err=%v", err)
	}

	// 关闭后从同一数据位置重新打开：成功停用与其唯一审计完整保留，失败尝试
	// 不能作为额外的停用状态或事件出现；接收方依旧被拒。
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
		t.Fatal("deactivation not persisted after reopen")
	}
	reloadedAudit, err := s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloadedAudit) != len(auditBefore)+1 {
		t.Fatalf("audit count after reopen = %d, want %d", len(reloadedAudit), len(auditBefore)+1)
	}
	reloadedLast := reloadedAudit[len(reloadedAudit)-1]
	if reloadedLast.Action != ActionDeactivated || reloadedLast.ObjectID != p.ID ||
		reloadedLast.ActorID != doc.ID || !reloadedLast.OccurredAt.Equal(successAt) {
		t.Fatalf("deactivation audit after reopen wrong: %+v", reloadedLast)
	}
	if denied, err := s3.Read(rcv, p.ID, e.ID, Diagnosis); !errors.Is(err, ErrAccessDenied) ||
		len(denied.Records) != 0 || denied.EncounterID != "" {
		t.Fatalf("read after reopen: %+v err=%v, want ErrAccessDenied without content",
			denied, err)
	}
	// 内部历史在重开后依旧完整：草稿与两版历史都在。
	reloadedChart, err := s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloadedChart.Records, chartBefore.Records) {
		t.Fatalf("records not preserved across reopen:\nbefore: %+v\nafter:  %+v",
			chartBefore.Records, reloadedChart.Records)
	}

	// 既有的成功停用后限制继续保留：停用后不能新增就诊、改草稿、生效、更正
	// 或新建授权。
	if _, err := s3.AddEncounter(doc, p.ID, time.Time{}); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("add encounter after deactivate err = %v, want ErrDeactivated", err)
	}
	if _, err := s3.UpdateDraft(doc, draft.ID, "停用后改草稿"); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("update draft after deactivate err = %v, want ErrDeactivated", err)
	}
	if _, err := s3.ActivateRecord(doc, draft.ID); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("activate after deactivate err = %v, want ErrDeactivated", err)
	}
	if _, err := s3.CorrectRecord(doc, r.ID, 2, "停用后更正", "原因"); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("correct after deactivate err = %v, want ErrDeactivated", err)
	}
	if _, err := s3.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t, clk.t.Add(time.Hour)); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("grant after deactivate err = %v, want ErrDeactivated", err)
	}
}
