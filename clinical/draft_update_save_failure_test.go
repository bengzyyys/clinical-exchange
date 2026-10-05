package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestDraftUpdateSaveFailureKeepsDraftAndAllowsRetry 覆盖“内部使用者提交合法的
// 草稿修改、患者未停用、目标记录仍是尚未生效的草稿，同一次就诊已有同类生效记录，
// 但本地保存失败”的改草稿场景：修改必须明确向调用方报错，不能报告成功，也不能
// 把保存问题伪装成参数错误、版本冲突、已生效或权限问题。失败后正式草稿的完整
// 内容与提交前一致——记录标识、患者、就诊、类别原样保留，仍是草稿，没有当前
// 生效版本，也没有新增历史版本；既有生效记录及其历史不受影响，持有该就诊该
// 类别有效整类授权的接收方仍只读到原先的生效内容，看不到旧草稿或此次未保存的
// 新内容。改草稿本就不产生审计事件，失败尝试也不能增加或改动审计。
//
// 保存条件恢复后可以继续修改同一条草稿：成功结果与内部档案、就诊记录视图中的
// 完整内容都与提交一致（含换行与首尾空格，按原样保留），记录标识与归属不变，
// 不生成生效版本，接收方仍拿不到草稿内容。这条草稿之后生效时，第 1 版必须固化
// 最后一次成功保存的内容，而不是之前失败的修改。空白内容被拒绝且原草稿保留；
// 已生效记录不能再通过修改草稿覆盖。诊断与医嘱同样适用。
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
	// 同一次就诊下已有一条同类别的生效记录及其历史。
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
	draftContent := "目标草稿原内容-第二行也行-" + category
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

	// checkIntact 断言：失败尝试之后（以及重开之后），目标记录仍是提交前的那条
	// 草稿——标识、患者、就诊、类别与完整草稿内容全部原样，仍没有当前生效版本
	// 与任何历史版本；既有记录历史、审计与接收方可见结果全部维持提交前状态，
	// 未保存的新内容不出现在任何正式视图里。
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
		// 既有生效记录的历史原样保留。
		if got := findHistory(chart, other.ID); !reflect.DeepEqual(got, otherHistBefore) {
			t.Fatalf("%s: other record history changed:\nbefore: %+v\nafter:  %+v", label, otherHistBefore, got)
		}

		// 就诊视图中的目标记录同样仍是提交前的草稿。
		er, err := st.EncounterRecords(doc, p.ID, e.ID)
		if err != nil {
			t.Fatalf("%s: encounter records: %v", label, err)
		}
		if len(er) != 2 {
			t.Fatalf("%s: encounter record count changed: %+v", label, er)
		}
		for _, rh := range er {
			if rh.Record.ID == target.ID {
				if !rh.HasDraft || rh.CurrentVersion != nil || rh.DraftContent != draftContent ||
					rh.Record.PatientID != p.ID || rh.Record.EncounterID != e.ID ||
					rh.Record.Category != category {
					t.Fatalf("%s: target no longer the same plain draft in encounter view: %+v", label, rh)
				}
			}
		}

		// 改草稿本就不产生审计事件；失败尝试不能增加或改动任何既有事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}

		// 接收方仍只读到原先已生效的内容：旧草稿与此次未保存的新内容都不出现，
		// 目标草稿也没有被计为新增生效记录。
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

	// 身份有权、患者未停用、目标记录是草稿，提交内容本身合法（非空白，
	// 含换行与首尾空格），仅保存失败：明确报错，不能伪装成参数不合法、
	// 版本冲突、已生效、未找到或权限拒绝等业务结果，也不能报告成功。
	failedContent := "  未保存的新内容-" + category + "\n第二行 "
	if _, err := s.UpdateDraft(doc, target.ID, failedContent); err == nil {
		t.Fatal("draft update must fail when local save fails")
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

	// 保存条件恢复后：同一条草稿可以继续修改。提交内容同样带换行与首尾空格，
	// 必须按原样保留；修改只改变草稿内容，不更换记录标识或归属，不产生版本。
	retryContent := "  重试提交的新内容-" + category + "\n第二行仍在  "
	updated, err := s2.UpdateDraft(doc, target.ID, retryContent)
	if err != nil {
		t.Fatalf("update draft after save recovered: %v", err)
	}
	if updated.ID != target.ID || updated.PatientID != p.ID || updated.EncounterID != e.ID ||
		updated.Category != category || !updated.HasDraft || updated.DraftContent != retryContent ||
		updated.CurrentVersionID != "" || len(updated.Versions) != 0 {
		t.Fatalf("successful update returned an unexpected record: %+v", updated)
	}

	// 内部档案视图与返回结果一致：仍是同一条草稿，完整内容即本次提交。
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, target.ID)
	if hist == nil || !hist.HasDraft || hist.DraftContent != retryContent ||
		!hist.Record.HasDraft || hist.Record.DraftContent != retryContent {
		t.Fatalf("chart draft content mismatch after retry: %+v", hist)
	}
	if hist.Record.ID != target.ID || hist.Record.PatientID != p.ID ||
		hist.Record.EncounterID != e.ID || hist.Record.Category != category {
		t.Fatalf("record identity changed after retry: %+v", hist.Record)
	}
	if hist.CurrentVersion != nil || hist.Record.CurrentVersionID != "" ||
		len(hist.Versions) != 0 || len(hist.Record.Versions) != 0 {
		t.Fatalf("draft update produced a version: %+v", hist)
	}
	// 既有记录的历史不受改草稿影响。
	if got := findHistory(chart, other.ID); !reflect.DeepEqual(got, otherHistBefore) {
		t.Fatalf("other record history changed by retry:\nbefore: %+v\nafter:  %+v", otherHistBefore, got)
	}

	// 就诊记录视图显示相同内容，仍是草稿、同一标识与归属。
	er, err := s2.EncounterRecords(doc, p.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(er) != 2 {
		t.Fatalf("encounter record count changed after retry: %+v", er)
	}
	var targetInEncounter *RecordHistory
	for i := range er {
		if er[i].Record.ID == target.ID {
			targetInEncounter = &er[i]
		}
	}
	if targetInEncounter == nil || !targetInEncounter.HasDraft ||
		targetInEncounter.DraftContent != retryContent || targetInEncounter.CurrentVersion != nil ||
		targetInEncounter.Record.PatientID != p.ID || targetInEncounter.Record.EncounterID != e.ID ||
		targetInEncounter.Record.Category != category {
		t.Fatalf("encounter view of target after retry unexpected: %+v", targetInEncounter)
	}

	// 成功的草稿修改同样不产生审计事件：事件集合与提交前完全一致。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit events changed after successful draft update:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}

	// 接收方仍只能看到原先的生效记录：草稿内容（无论新旧）都不暴露。
	res, err := s2.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, readBefore) || readRecordIDs(res)[target.ID] {
		t.Fatalf("receiver read after successful draft update = %+v, want unchanged effective-only view", res)
	}

	// 成功修改后的草稿状态在关闭后重新打开同一数据位置时保持一致。
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
	if hist == nil || !hist.HasDraft || hist.DraftContent != retryContent ||
		hist.Record.ID != target.ID || hist.CurrentVersion != nil || len(hist.Versions) != 0 {
		t.Fatalf("updated draft not preserved after reopen: %+v", hist)
	}

	// 边界：空白内容（含纯换行/纯空白）被拒绝并保留当前草稿。
	if _, err := s3.UpdateDraft(doc, target.ID, "  \n\t "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank content err = %v, want ErrInvalidArgument", err)
	}
	chart, err = s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hist = findHistory(chart, target.ID); hist == nil || !hist.HasDraft ||
		hist.DraftContent != retryContent {
		t.Fatalf("rejected blank update changed the draft: %+v", hist)
	}

	// 草稿之后通过现有操作生效：第 1 版必须固化最后一次成功保存的内容，
	// 不能采用之前失败的那次修改。
	activateTime := clk.t.Add(2 * time.Hour)
	clk.t = activateTime
	v1, err := s3.ActivateRecord(doc, target.ID)
	if err != nil {
		t.Fatalf("activate updated draft: %v", err)
	}
	if v1.Number != 1 || v1.RecordID != target.ID || v1.Category != category ||
		v1.PrevID != "" || v1.Reason != "" {
		t.Fatalf("bad v1 identity: %+v", v1)
	}
	if v1.Content != retryContent {
		t.Fatalf("v1 content = %q, want last successfully saved content %q", v1.Content, retryContent)
	}
	if v1.Content == failedContent {
		t.Fatalf("v1固化了保存失败那次提交的内容: %q", v1.Content)
	}
	if !v1.CreatedAt.Equal(activateTime) {
		t.Fatalf("v1 time %v != activation time %v", v1.CreatedAt, activateTime)
	}

	// 生效后草稿清除，版本链上只有第 1 版。
	chart, err = s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist = findHistory(chart, target.ID)
	if hist == nil || hist.CurrentVersion == nil || hist.HasDraft || hist.DraftContent != "" ||
		!reflect.DeepEqual(*hist.CurrentVersion, v1) || hist.Record.CurrentVersionID != v1.ID ||
		len(hist.Versions) != 1 || !reflect.DeepEqual(hist.Versions[0], v1) {
		t.Fatalf("history after activation unexpected: %+v", hist)
	}
	// 既有生效记录及其历史不受影响。
	if got := findHistory(chart, other.ID); !reflect.DeepEqual(got, otherHistBefore) {
		t.Fatalf("other record history changed after target activation:\nbefore: %+v\nafter:  %+v", otherHistBefore, got)
	}

	// 只多出一条生效审计：失败的改草稿尝试始终没有留下事件。
	audit, err = s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after activation:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionActivated || last.ObjectType != "record" || last.ObjectID != target.ID ||
		last.ActorID != doc.ID || last.PatientID != p.ID {
		t.Fatalf("unexpected new audit event: %+v", last)
	}

	// 生效后整类授权让接收方读到第 1 版（成功保存的内容），看不到任何草稿痕迹；
	// 既有内容原样保留。
	res, err = s3.Read(rcv, p.ID, e.ID, category)
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
	if got := byID[target.ID]; got.VersionID != v1.ID || got.Version != 1 ||
		got.Content != retryContent {
		t.Fatalf("receiver sees wrong target version: %+v", got)
	}
	if got := byID[other.ID]; got.VersionID != otherV1.ID || got.Content != otherContent {
		t.Fatalf("receiver view of existing record changed: %+v", got)
	}

	// 边界：已生效的记录不能再通过修改草稿覆盖；状态与审计均不变。
	if _, err := s3.UpdateDraft(doc, target.ID, "生效后试图覆盖"); !errors.Is(err, ErrActive) {
		t.Fatalf("update active record err = %v, want ErrActive", err)
	}
	chart, err = s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hist = findHistory(chart, target.ID); hist == nil || hist.HasDraft ||
		hist.CurrentVersion == nil || hist.CurrentVersion.ID != v1.ID ||
		hist.CurrentVersion.Content != retryContent || len(hist.Versions) != 1 {
		t.Fatalf("ErrActive update changed the effective record: %+v", hist)
	}
	audit, err = s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(audit); got[ActionActivated] != 2 {
		t.Fatalf("audit changed after rejected update on active record: %v", got)
	}
}
