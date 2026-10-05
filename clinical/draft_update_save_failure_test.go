package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestDraftUpdateSaveFailureKeepsDraftAndAllowsRetry 覆盖“内部使用者提交合法
// 修改、患者未停用、目标记录仍是尚未生效的草稿，但本地保存失败”的改草稿场景：
// 修改必须明确报错且不能报告成功，正式草稿仍保留提交前的完整内容——记录标识、
// 患者、就诊与类别保持原样，仍没有当前生效版本与任何历史版本；同一次就诊已有
// 的同类生效记录及其历史不受影响，持有该就诊该类别有效整类授权的接收方仍只读到
// 原先的生效内容，看不到旧草稿或未保存的新内容。改草稿本就不产生审计事件，
// 失败尝试也不能增加事件或改动已有审计。保存条件恢复后可继续修改同一条草稿，
// 成功结果与内部档案、就诊记录视图一致；换行与首尾空格按原样保留，标识归属
// 不变、不生成版本、不向接收方泄露草稿。之后生效时第 1 版固化最后一次成功
// 保存的内容，而不是失败的那次提交。诊断与医嘱同样适用。
func TestDraftUpdateSaveFailureKeepsDraftAndAllowsRetry(t *testing.T) {
	for _, category := range []string{Diagnosis, Order} {
		t.Run(category, func(t *testing.T) {
			testDraftUpdateSaveFailure(t, category)
		})
	}
}

func testDraftUpdateSaveFailure(t *testing.T, category string) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "改草稿保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 同一次就诊下已有一条同类别的生效记录。
	otherContent := "既有生效内容-" + category
	other, err := s.CreateDraft(doc, p.ID, e.ID, category, otherContent)
	if err != nil {
		t.Fatal(err)
	}
	otherV1, err := s.ActivateRecord(doc, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 目标记录仍是尚未生效的草稿。
	draftContent := "目标草稿原内容-\n带换行-" + category
	target, err := s.CreateDraft(doc, p.ID, e.ID, category, draftContent)
	if err != nil {
		t.Fatal(err)
	}
	// 接收方持有该就诊该类别当前有效的整类授权。
	if _, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: category}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 提交前的基线：目标草稿、既有记录历史、审计与接收方读取结果。
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetBefore := findHistory(chartBefore, target.ID)
	if targetBefore == nil || !targetBefore.HasDraft || targetBefore.DraftContent != draftContent ||
		targetBefore.CurrentVersion != nil || len(targetBefore.Versions) != 0 {
		t.Fatalf("unexpected baseline target draft: %+v", targetBefore)
	}
	otherHistBefore := findHistory(chartBefore, other.ID)
	if otherHistBefore == nil || otherHistBefore.CurrentVersion == nil ||
		otherHistBefore.CurrentVersion.ID != otherV1.ID || len(otherHistBefore.Versions) != 1 {
		t.Fatalf("unexpected baseline other history: %+v", otherHistBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore); got[ActionActivated] != 1 || got[ActionGranted] != 1 {
		t.Fatalf("unexpected baseline audit: %v", got)
	}
	readBefore, err := s.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if len(readBefore.Records) != 1 || readBefore.Records[0].RecordID != other.ID ||
		readBefore.Records[0].VersionID != otherV1.ID || readBefore.Records[0].Content != otherContent {
		t.Fatalf("unexpected baseline read: %+v", readBefore)
	}

	// 失败尝试提交的合法新内容：含换行与首尾空格，必须原样可保留，
	// 但保存失败时绝不能进入正式草稿。
	failedContent := "  未保存的新内容-\n第二行  -" + category

	// checkIntact 断言：失败尝试之后（以及重开之后），目标记录仍是提交前的那条
	// 草稿——标识、患者、就诊归属、类别与完整草稿内容都在，仍没有当前生效版本
	// 与任何历史版本；既有记录、审计与接收方可见结果全部维持提交前状态。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		hist := findHistory(chart, target.ID)
		if hist == nil {
			t.Fatalf("%s: target record missing", label)
		}
		if hist.Record.ID != target.ID || hist.Record.PatientID != p.ID ||
			hist.Record.EncounterID != e.ID || hist.Record.Category != category {
			t.Fatalf("%s: target record identity changed: %+v", label, hist.Record)
		}
		if !hist.HasDraft || hist.DraftContent != draftContent ||
			!hist.Record.HasDraft || hist.Record.DraftContent != draftContent {
			t.Fatalf("%s: draft content lost or changed: %+v", label, hist)
		}
		if hist.CurrentVersion != nil || hist.Record.CurrentVersionID != "" ||
			len(hist.Versions) != 0 || len(hist.Record.Versions) != 0 {
			t.Fatalf("%s: failed update left a version: %+v", label, hist)
		}
		// 未保存的新内容绝不能出现在任何视图里。
		if hist.DraftContent == failedContent || hist.Record.DraftContent == failedContent {
			t.Fatalf("%s: unsaved content leaked into the formal draft", label)
		}
		// 既有生效记录的历史原样保留。
		if got := findHistory(chart, other.ID); !reflect.DeepEqual(got, otherHistBefore) {
			t.Fatalf("%s: other record history changed:\nbefore: %+v\nafter:  %+v", label, otherHistBefore, got)
		}

		// 就诊视图中的目标记录同样仍是原草稿，且没有任何版本。
		er, err := st.EncounterRecords(doc, p.ID, e.ID)
		if err != nil {
			t.Fatalf("%s: encounter records: %v", label, err)
		}
		if len(er) != 2 {
			t.Fatalf("%s: encounter record count changed: %+v", label, er)
		}
		for _, rh := range er {
			if rh.Record.ID == target.ID {
				if !rh.HasDraft || rh.DraftContent != draftContent ||
					rh.CurrentVersion != nil || len(rh.Versions) != 0 {
					t.Fatalf("%s: target no longer the original plain draft in encounter view: %+v", label, rh)
				}
			}
		}

		// 既有审计事件保持原样；改草稿本不产生事件，失败尝试也不增加。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}

		// 接收方仍只读到原先已生效的内容：旧草稿与未保存的新内容都不出现。
		res, err := st.Read(rcv, p.ID, e.ID, category)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}
		if readRecordIDs(res)[target.ID] {
			t.Fatalf("%s: failed update leaked the target draft to the receiver", label)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、患者状态、记录状态或参数合法性无关。原数据文件先挪到旁边，
	// 事后原样还原。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 身份有权、患者未停用、记录仍是草稿、新内容合法（含换行与首尾空格），
	// 仅保存失败：明确报错，不能报告成功，也不能伪装成参数不合法、版本冲突、
	// 已生效、记录不存在、权限拒绝或患者停用等业务结果。
	if got, err := s.UpdateDraft(doc, target.ID, failedContent); err == nil {
		t.Fatalf("update must fail when local save fails, but returned %+v", got)
	} else if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrActive) || errors.Is(err, ErrAccessDenied) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrNotFound) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：档案、就诊记录、审计与接收方读取全部维持提交前状态。
	checkIntact("after failed save", s)

	// 关闭后从原数据位置重新打开：先恢复保存条件（还原原数据文件），
	// 失败后的草稿状态在重开后保持一致。
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

	// 保存条件恢复后：继续修改同一条草稿。合法内容（含换行与首尾空格）按原样
	// 保留，返回结果中的完整内容与提交一致，记录标识与归属不变，仍是无版本草稿。
	retryContent := "  重试提交的内容-\n换行第二行  -" + category
	updated, err := s2.UpdateDraft(doc, target.ID, retryContent)
	if err != nil {
		t.Fatalf("update draft after save recovered: %v", err)
	}
	if updated.ID != target.ID || updated.PatientID != p.ID || updated.EncounterID != e.ID ||
		updated.Category != category || !updated.HasDraft || updated.DraftContent != retryContent ||
		updated.CurrentVersionID != "" || len(updated.Versions) != 0 {
		t.Fatalf("successful update returned an unexpected record: %+v", updated)
	}

	// 内部完整档案与就诊记录视图都显示同一份新内容，且仍是无版本草稿。
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, target.ID)
	if hist == nil || !hist.HasDraft || hist.DraftContent != retryContent ||
		hist.Record.DraftContent != retryContent || hist.CurrentVersion != nil ||
		len(hist.Versions) != 0 || hist.Record.CurrentVersionID != "" ||
		hist.Record.ID != target.ID || hist.Record.PatientID != p.ID ||
		hist.Record.EncounterID != e.ID || hist.Record.Category != category {
		t.Fatalf("draft after successful update: %+v", hist)
	}
	er, err := s2.EncounterRecords(doc, p.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	var encTarget *RecordHistory
	for i := range er {
		if er[i].Record.ID == target.ID {
			encTarget = &er[i]
		}
	}
	if encTarget == nil || !encTarget.HasDraft || encTarget.DraftContent != retryContent ||
		encTarget.CurrentVersion != nil || len(encTarget.Versions) != 0 {
		t.Fatalf("encounter view after successful update: %+v", encTarget)
	}
	// 既有生效记录的历史不受成功修改影响。
	if got := findHistory(chart, other.ID); !reflect.DeepEqual(got, otherHistBefore) {
		t.Fatalf("other record history changed by retry:\nbefore: %+v\nafter:  %+v", otherHistBefore, got)
	}

	// 改草稿不产生审计事件：失败与成功两次尝试后审计仍与提交前一致。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit after successful update:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}

	// 接收方仍只读到原先的生效内容，不获得草稿内容（无论失败的还是成功的）。
	res, err := s2.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, readBefore) {
		t.Fatalf("receiver read after successful update:\nbefore: %+v\nafter:  %+v", readBefore, res)
	}

	// 这条草稿之后通过现有操作生效：第 1 版必须固化最后一次成功保存的内容，
	// 不能采用之前失败的修改或提交前的旧内容；版本号从 1 开始，草稿被清除。
	successTime := clk.t.Add(2 * time.Hour)
	clk.t = successTime
	v1, err := s2.ActivateRecord(doc, target.ID)
	if err != nil {
		t.Fatalf("activate after recovered update: %v", err)
	}
	if v1.Number != 1 || v1.RecordID != target.ID || v1.Category != category ||
		v1.Content != retryContent || v1.PrevID != "" || v1.Reason != "" ||
		!v1.CreatedAt.Equal(successTime) {
		t.Fatalf("activation did not freeze the last successfully saved content: %+v", v1)
	}
	if v1.Content == failedContent || v1.Content == draftContent {
		t.Fatalf("activation froze the wrong content: %q", v1.Content)
	}
	chart, err = s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist = findHistory(chart, target.ID)
	if hist == nil || hist.HasDraft || hist.DraftContent != "" ||
		hist.CurrentVersion == nil || hist.CurrentVersion.ID != v1.ID || len(hist.Versions) != 1 {
		t.Fatalf("record state after activation: %+v", hist)
	}

	// 生效后接收方可读到第 1 版；既有生效记录原样保留。
	res, err = s2.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 2 {
		t.Fatalf("receiver read after activation = %+v, want 2 records", res)
	}
	byID := map[ID]EffectiveRecord{}
	for _, rec := range res.Records {
		byID[rec.RecordID] = rec
	}
	if got := byID[target.ID]; got.VersionID != v1.ID || got.Version != 1 || got.Content != retryContent {
		t.Fatalf("receiver sees wrong target version: %+v", got)
	}
	if got := byID[other.ID]; got.VersionID != otherV1.ID || got.Content != otherContent {
		t.Fatalf("receiver view of existing record changed: %+v", got)
	}

	// 生效只新增一条目标记录的生效审计，既有事件原样保留。
	audit, err = s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after activation:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionActivated || last.ObjectID != target.ID || last.ActorID != doc.ID {
		t.Fatalf("unexpected new audit event: %+v", last)
	}

	// 已生效记录不能再通过修改草稿覆盖：返回 ErrActive，内容与版本保持不变。
	if _, err := s2.UpdateDraft(doc, target.ID, "生效后再覆盖"); !errors.Is(err, ErrActive) {
		t.Fatalf("update active record err = %v, want ErrActive", err)
	}
	chart, err = s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist = findHistory(chart, target.ID)
	if hist == nil || hist.HasDraft || hist.CurrentVersion == nil ||
		hist.CurrentVersion.ID != v1.ID || hist.CurrentVersion.Content != retryContent ||
		len(hist.Versions) != 1 {
		t.Fatalf("update on active record changed state: %+v", hist)
	}

	// 最终状态在关闭后重新打开同一数据位置时保持一致。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })
	chart, err = s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist = findHistory(chart, target.ID)
	if hist == nil || hist.HasDraft || hist.CurrentVersion == nil ||
		!reflect.DeepEqual(*hist.CurrentVersion, v1) || len(hist.Versions) != 1 {
		t.Fatalf("final state not preserved after reopen: %+v", hist)
	}
	res, err = s3.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 2 || !readRecordIDs(res)[target.ID] {
		t.Fatalf("receiver read after reopen = %+v, want activated target included", res)
	}
}

// TestDraftUpdateRejectsBlankAndKeepsOriginal 保留边界：提交空白内容在进入写入
// 之前即被拒绝（ErrInvalidArgument），正式草稿完整保留，不产生版本或审计。
func TestDraftUpdateRejectsBlankAndKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	pid, eid := setupPatientEncounter(t, s)
	original := "保留原样的草稿内容"
	r, err := s.CreateDraft(doc, pid, eid, Diagnosis, original)
	if err != nil {
		t.Fatal(err)
	}

	for _, blank := range []string{"", "   ", "\n\t  \n"} {
		if _, err := s.UpdateDraft(doc, r.ID, blank); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("blank content %q err = %v, want ErrInvalidArgument", blank, err)
		}
	}

	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, r.ID)
	if hist == nil || !hist.HasDraft || hist.DraftContent != original ||
		hist.CurrentVersion != nil || len(hist.Versions) != 0 {
		t.Fatalf("blank update changed the draft: %+v", hist)
	}
	er, err := s.EncounterRecords(doc, pid, eid)
	if err != nil {
		t.Fatal(err)
	}
	if len(er) != 1 || !er[0].HasDraft || er[0].DraftContent != original {
		t.Fatalf("blank update changed the encounter view: %+v", er)
	}
	audit, err := s.AuditEvents(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 0 {
		t.Fatalf("blank update produced audit events: %+v", audit)
	}
}
