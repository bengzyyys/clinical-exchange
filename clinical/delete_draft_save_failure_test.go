package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestDeleteDraftSaveFailureKeepsDraftAndAllowsRetry 覆盖“内部使用者删除一条
// 尚未生效的草稿、患者未停用、同一次就诊下还有另一条同类草稿与一条已生效并有
// 更正历史的同类记录，接收方持有该就诊该类别的有效整类授权，但本地保存失败”的
// 删草稿场景：删除必须明确向调用方报保存错误，不能报告成功，也不能把保存问题
// 伪装成记录不存在、已生效、没有权限或患者已停用。失败后目标草稿在完整档案与
// 就诊记录视图中仍能找到——标识、患者、就诊、类别与完整内容（含换行与首尾
// 空格）保持提交前的值，仍没有当前生效版本或历史版本；另一条草稿与既有生效
// 记录的当前版本、旧版本链和更正原因全部原样；既有审计事件的内容与顺序不变；
// 接收方仍只读到原先的当前生效内容，看不到任何草稿。
//
// 保存条件恢复后，对同一目标草稿再次提交删除正常成功：档案与就诊记录中都不再
// 出现它，按原标识修改该草稿返回 ErrNotFound；删除只移除目标草稿，另一条草稿
// 与生效记录的完整历史继续保留；删除本就不产生审计事件，成功后审计与删除前
// 一致，接收方可读内容不受影响。边界保留：对已有更正历史的生效记录执行草稿
// 删除返回 ErrActive，其当前内容、全部旧版本与更正原因完整保留。
// 诊断与医嘱同样适用。
func TestDeleteDraftSaveFailureKeepsDraftAndAllowsRetry(t *testing.T) {
	for _, category := range []string{Diagnosis, Order} {
		t.Run(category, func(t *testing.T) {
			testDeleteDraftSaveFailure(t, category)
		})
	}
}

func testDeleteDraftSaveFailure(t *testing.T, category string) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "删草稿保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 目标草稿：此前已经保存并修改过，最终内容含换行与首尾空格。
	target, err := s.CreateDraft(doc, p.ID, e.ID, category, "目标草稿初始内容-"+category)
	if err != nil {
		t.Fatal(err)
	}
	targetContent := "  目标草稿修改后内容-" + category + "\n第二行也保留  "
	target, err = s.UpdateDraft(doc, target.ID, targetContent)
	if err != nil {
		t.Fatal(err)
	}
	// 同一次就诊下的另一条同类草稿，始终保持草稿状态。
	otherDraftContent := "另一条草稿内容-" + category
	otherDraft, err := s.CreateDraft(doc, p.ID, e.ID, category, otherDraftContent)
	if err != nil {
		t.Fatal(err)
	}
	// 同一次就诊下已有一条同类别的生效记录，并有一次更正历史（当前第 2 版）。
	active, err := s.CreateDraft(doc, p.ID, e.ID, category, "生效记录原始内容-"+category)
	if err != nil {
		t.Fatal(err)
	}
	activeV1, err := s.ActivateRecord(doc, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	activeV2, err := s.CorrectRecord(doc, active.ID, 1, "生效记录更正后内容-"+category, "复核更正原因-"+category)
	if err != nil {
		t.Fatal(err)
	}
	// 接收方持有该就诊该类别当前有效的整类授权，整个操作期间有效且未撤回。
	if _, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: category}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 提交前的基线：目标草稿、另一条草稿、生效记录历史、审计与接收方读取结果。
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetBefore := findHistory(chartBefore, target.ID)
	if targetBefore == nil || !targetBefore.HasDraft || targetBefore.DraftContent != targetContent ||
		targetBefore.CurrentVersion != nil || len(targetBefore.Versions) != 0 {
		t.Fatalf("unexpected baseline target draft: %+v", targetBefore)
	}
	otherDraftBefore := findHistory(chartBefore, otherDraft.ID)
	if otherDraftBefore == nil || !otherDraftBefore.HasDraft ||
		otherDraftBefore.DraftContent != otherDraftContent {
		t.Fatalf("unexpected baseline other draft: %+v", otherDraftBefore)
	}
	activeBefore := findHistory(chartBefore, active.ID)
	if activeBefore == nil || activeBefore.CurrentVersion == nil ||
		activeBefore.CurrentVersion.ID != activeV2.ID || len(activeBefore.Versions) != 2 {
		t.Fatalf("unexpected baseline active history: %+v", activeBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore); got[ActionActivated] != 1 || got[ActionCorrected] != 1 ||
		got[ActionGranted] != 1 {
		t.Fatalf("unexpected baseline audit: %v", got)
	}
	readBefore, err := s.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if len(readBefore.Records) != 1 || readBefore.Records[0].RecordID != active.ID ||
		readBefore.Records[0].VersionID != activeV2.ID || readBefore.Records[0].Version != 2 ||
		readBefore.Records[0].Content != activeV2.Content {
		t.Fatalf("unexpected baseline read: %+v", readBefore)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），目标草稿仍是提交前的那条
	// 草稿——标识、患者、就诊、类别与完整内容（含换行与首尾空格）全部原样，
	// 仍没有当前生效版本与任何历史版本；另一条草稿、生效记录历史、审计与
	// 接收方可见结果全部维持提交前状态。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		hist := findHistory(chart, target.ID)
		if hist == nil {
			t.Fatalf("%s: target draft missing from chart", label)
		}
		if hist.Record.ID != target.ID || hist.Record.PatientID != p.ID ||
			hist.Record.EncounterID != e.ID || hist.Record.Category != category {
			t.Fatalf("%s: target record identity changed: %+v", label, hist.Record)
		}
		if !hist.HasDraft || hist.DraftContent != targetContent ||
			!hist.Record.HasDraft || hist.Record.DraftContent != targetContent {
			t.Fatalf("%s: target draft content lost or changed: %+v", label, hist)
		}
		if hist.CurrentVersion != nil || hist.Record.CurrentVersionID != "" ||
			len(hist.Versions) != 0 || len(hist.Record.Versions) != 0 {
			t.Fatalf("%s: failed delete left a version: %+v", label, hist)
		}
		// 另一条草稿与既有生效记录的历史原样保留。
		if got := findHistory(chart, otherDraft.ID); !reflect.DeepEqual(got, otherDraftBefore) {
			t.Fatalf("%s: other draft changed:\nbefore: %+v\nafter:  %+v", label, otherDraftBefore, got)
		}
		if got := findHistory(chart, active.ID); !reflect.DeepEqual(got, activeBefore) {
			t.Fatalf("%s: active record history changed:\nbefore: %+v\nafter:  %+v", label, activeBefore, got)
		}

		// 就诊记录视图中的目标记录同样仍是提交前的草稿。
		er, err := st.EncounterRecords(doc, p.ID, e.ID)
		if err != nil {
			t.Fatalf("%s: encounter records: %v", label, err)
		}
		if len(er) != 3 {
			t.Fatalf("%s: encounter record count changed: %+v", label, er)
		}
		var targetInEncounter *RecordHistory
		for i := range er {
			if er[i].Record.ID == target.ID {
				targetInEncounter = &er[i]
			}
		}
		if targetInEncounter == nil || !targetInEncounter.HasDraft ||
			targetInEncounter.DraftContent != targetContent || targetInEncounter.CurrentVersion != nil ||
			targetInEncounter.Record.PatientID != p.ID || targetInEncounter.Record.EncounterID != e.ID ||
			targetInEncounter.Record.Category != category {
			t.Fatalf("%s: target no longer the same plain draft in encounter view: %+v", label, targetInEncounter)
		}

		// 删草稿本就不产生审计事件；失败尝试不能增加或改动任何既有事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}

		// 接收方仍只读到原先的当前生效内容：任何草稿都不出现。
		res, err := st.Read(rcv, p.ID, e.ID, category)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}
		if readRecordIDs(res)[target.ID] || readRecordIDs(res)[otherDraft.ID] {
			t.Fatalf("%s: failed delete leaked a draft to the receiver", label)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、患者状态、记录状态无关。原数据文件先挪到旁边，事后原样还原。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 身份有权、患者未停用、目标记录是尚未生效的草稿，仅保存失败：明确报错，
	// 不能伪装成记录不存在、已生效、参数不合法、版本冲突、权限拒绝或患者
	// 已停用等业务结果，也不能报告成功。
	if err := s.DeleteDraft(doc, target.ID); err == nil {
		t.Fatal("draft delete must fail when local save fails")
	} else if errors.Is(err, ErrNotFound) || errors.Is(err, ErrActive) ||
		errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) {
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

	// 保存条件恢复后：对同一目标草稿再次提交删除，正常成功。
	if err := s2.DeleteDraft(doc, target.ID); err != nil {
		t.Fatalf("delete draft after save recovered: %v", err)
	}

	// 患者档案与就诊记录中都不再出现目标草稿。
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hist := findHistory(chart, target.ID); hist != nil {
		t.Fatalf("deleted draft still in chart: %+v", hist)
	}
	// 删除只移除目标草稿：另一条草稿与生效记录的完整历史原样保留。
	if got := findHistory(chart, otherDraft.ID); !reflect.DeepEqual(got, otherDraftBefore) {
		t.Fatalf("other draft changed by delete:\nbefore: %+v\nafter:  %+v", otherDraftBefore, got)
	}
	if got := findHistory(chart, active.ID); !reflect.DeepEqual(got, activeBefore) {
		t.Fatalf("active record history changed by delete:\nbefore: %+v\nafter:  %+v", activeBefore, got)
	}
	er, err := s2.EncounterRecords(doc, p.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(er) != 2 {
		t.Fatalf("encounter record count after delete: %+v", er)
	}
	for _, rh := range er {
		if rh.Record.ID == target.ID {
			t.Fatalf("deleted draft still in encounter view: %+v", rh)
		}
	}

	// 删除本就不产生审计事件：成功后审计与删除前完全一致。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit events changed by delete:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}

	// 接收方仍只读到原先的当前生效内容，原有授权与可读内容不受影响。
	res, err := s2.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, readBefore) {
		t.Fatalf("receiver read changed by delete:\nbefore: %+v\nafter:  %+v", readBefore, res)
	}

	// 按原标识修改已删除的草稿：返回 ErrNotFound。
	if _, err := s2.UpdateDraft(doc, target.ID, "删除后试图修改"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update deleted draft err = %v, want ErrNotFound", err)
	}
	// 重复删除同一标识同样返回 ErrNotFound，且不改变任何状态。
	if err := s2.DeleteDraft(doc, target.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-delete err = %v, want ErrNotFound", err)
	}

	// 删除结果在关闭后重新打开同一数据位置时保持：目标草稿仍然不存在，
	// 另一条草稿与生效记录历史仍在。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })
	chart, err = s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hist := findHistory(chart, target.ID); hist != nil {
		t.Fatalf("deleted draft reappeared after reopen: %+v", hist)
	}
	if got := findHistory(chart, otherDraft.ID); !reflect.DeepEqual(got, otherDraftBefore) {
		t.Fatalf("other draft changed after reopen:\nbefore: %+v\nafter:  %+v", otherDraftBefore, got)
	}
	if got := findHistory(chart, active.ID); !reflect.DeepEqual(got, activeBefore) {
		t.Fatalf("active record history changed after reopen:\nbefore: %+v\nafter:  %+v", activeBefore, got)
	}

	// 边界：已有更正历史的生效记录不能走草稿删除，返回 ErrActive；
	// 当前内容、全部旧版本与更正原因完整保留，审计不变。
	if err := s3.DeleteDraft(doc, active.ID); !errors.Is(err, ErrActive) {
		t.Fatalf("delete active record err = %v, want ErrActive", err)
	}
	chart, err = s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, active.ID)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != activeV2.ID ||
		hist.CurrentVersion.Content != activeV2.Content || hist.CurrentVersion.Reason != activeV2.Reason ||
		len(hist.Versions) != 2 {
		t.Fatalf("ErrActive delete changed the effective record: %+v", hist)
	}
	if !reflect.DeepEqual(hist.Versions[0], activeV1) || !reflect.DeepEqual(hist.Versions[1], activeV2) {
		t.Fatalf("ErrActive delete mutated version history: %+v", hist.Versions)
	}
	if got := findHistory(chart, otherDraft.ID); !reflect.DeepEqual(got, otherDraftBefore) {
		t.Fatalf("ErrActive delete changed the other draft:\nbefore: %+v\nafter:  %+v", otherDraftBefore, got)
	}
	audit, err = s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit events changed by ErrActive delete:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	res, err = s3.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, readBefore) {
		t.Fatalf("receiver read changed by ErrActive delete:\nbefore: %+v\nafter:  %+v", readBefore, res)
	}
}
