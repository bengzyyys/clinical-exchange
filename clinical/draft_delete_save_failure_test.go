package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestDraftDeleteSaveFailureKeepsDraftAndAllowsRetry 覆盖“内部使用者删除一条
// 尚未生效的诊断或医嘱、患者未停用、身份与记录状态都符合删除条件，同一次就诊
// 下还留有另一条同类草稿以及一条已生效且有更正历史的同类记录，但本地保存失败”
// 的删草稿场景：删除必须明确向调用方报保存错误，不能报告成功，也不能把保存
// 问题伪装成记录不存在、已生效或没有权限。失败后原目标草稿仍在完整档案与
// 该次就诊记录中——标识、患者、就诊、类别与提交前的完整草稿内容（含换行与
// 首尾空格，按原样保留）全部不变，仍没有当前生效版本与任何历史版本；另一条
// 草稿、既有生效记录的当前版本与旧版本链（含更正原因）、既有审计事件的内容
// 与顺序全部原样保留，持有该就诊该类别有效整类授权的接收方仍只读到原来的
// 当前生效内容，看不到任何草稿。
//
// 保存条件恢复后，对同一目标草稿再次提交删除应正常成功：档案与就诊记录中都
// 不再出现它，按原标识修改该草稿返回 ErrNotFound；删除只移除目标草稿，另一条
// 草稿与生效记录的完整历史继续保留。删草稿本就不产生审计事件，失败与成功都
// 不改变审计，原有授权与接收方可读内容不受影响。
//
// 同时保留边界：已有生效版本和更正历史的记录不能走草稿删除，DeleteDraft
// 返回 ErrActive，当前内容、全部旧版本与更正原因完整保留。诊断与医嘱同样适用。
func TestDraftDeleteSaveFailureKeepsDraftAndAllowsRetry(t *testing.T) {
	for _, category := range []string{Diagnosis, Order} {
		t.Run(category, func(t *testing.T) {
			testDraftDeleteSaveFailure(t, category)
		})
	}
}

func testDraftDeleteSaveFailure(t *testing.T, category string) {
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

	// 同一次就诊下另一条同类草稿：删除目标草稿的任何结果都不能波及它。
	siblingContent := "另一条草稿内容-" + category
	sibling, err := s.CreateDraft(doc, p.ID, e.ID, category, siblingContent)
	if err != nil {
		t.Fatal(err)
	}

	// 同一次就诊下一条已生效且已有更正历史的同类记录：当前为第 2 版，
	// 旧版本链与更正原因都必须在删除尝试前后保持原样。
	activeV1Content := "既有生效初版内容-" + category
	active, err := s.CreateDraft(doc, p.ID, e.ID, category, activeV1Content)
	if err != nil {
		t.Fatal(err)
	}
	activeV1, err := s.ActivateRecord(doc, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	activeV2Content := "  既有生效更正后内容-" + category + "\n带换行 "
	activeReason := "首次更正原因-" + category
	activeV2, err := s.CorrectRecord(doc, active.ID, 1, activeV2Content, activeReason)
	if err != nil {
		t.Fatal(err)
	}

	// 目标草稿：此前已经保存并修改过，当前草稿内容包含换行与首尾空格。
	target, err := s.CreateDraft(doc, p.ID, e.ID, category, "目标草稿初次保存内容-"+category)
	if err != nil {
		t.Fatal(err)
	}
	draftContent := "  目标草稿修改后内容-" + category + "\n第二行仍在  "
	if _, err := s.UpdateDraft(doc, target.ID, draftContent); err != nil {
		t.Fatal(err)
	}

	// 接收方持有覆盖该就诊该类别的有效整类授权，整个操作期间有效且不撤回；
	// 原本只能读到生效记录的当前版本。
	if _, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: category}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 提交前的基线：目标草稿、另一条草稿、既有生效记录历史、审计与接收方
	// 读取结果。
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetBefore := findHistory(chartBefore, target.ID)
	if targetBefore == nil || !targetBefore.HasDraft || targetBefore.DraftContent != draftContent ||
		targetBefore.CurrentVersion != nil || len(targetBefore.Versions) != 0 {
		t.Fatalf("unexpected baseline target draft: %+v", targetBefore)
	}
	siblingHistBefore := findHistory(chartBefore, sibling.ID)
	if siblingHistBefore == nil || !siblingHistBefore.HasDraft ||
		siblingHistBefore.DraftContent != siblingContent ||
		siblingHistBefore.CurrentVersion != nil || len(siblingHistBefore.Versions) != 0 {
		t.Fatalf("unexpected baseline sibling draft: %+v", siblingHistBefore)
	}
	activeHistBefore := findHistory(chartBefore, active.ID)
	if activeHistBefore == nil || activeHistBefore.CurrentVersion == nil ||
		activeHistBefore.CurrentVersion.ID != activeV2.ID || len(activeHistBefore.Versions) != 2 {
		t.Fatalf("unexpected baseline active history: %+v", activeHistBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore); got[ActionActivated] != 1 ||
		got[ActionCorrected] != 1 || got[ActionGranted] != 1 {
		t.Fatalf("unexpected baseline audit: %v", got)
	}
	readBefore, err := s.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if len(readBefore.Records) != 1 || readBefore.Records[0].RecordID != active.ID ||
		readBefore.Records[0].VersionID != activeV2.ID ||
		readBefore.Records[0].Content != activeV2Content {
		t.Fatalf("unexpected baseline read: %+v", readBefore)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），目标记录仍是提交前的
	// 那条草稿——标识、患者、就诊、类别与完整草稿内容（含换行与首尾空格）
	// 全部原样，仍没有当前生效版本与任何历史版本；另一条草稿、既有生效记录
	// 的当前版本/旧版本链/更正原因、审计事件的内容与顺序、接收方可见结果全部
	// 维持提交前状态，失败的删除没有在任何正式视图里让目标提前消失。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		hist := findHistory(chart, target.ID)
		if hist == nil {
			t.Fatalf("%s: target draft disappeared from chart after unfinished delete", label)
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
			t.Fatalf("%s: failed delete left a version: %+v", label, hist)
		}
		// 另一条草稿原样保留。
		if got := findHistory(chart, sibling.ID); !reflect.DeepEqual(got, siblingHistBefore) {
			t.Fatalf("%s: sibling draft changed:\nbefore: %+v\nafter:  %+v", label, siblingHistBefore, got)
		}
		// 既有生效记录的当前版本、旧版本链与更正原因原样保留。
		if got := findHistory(chart, active.ID); !reflect.DeepEqual(got, activeHistBefore) {
			t.Fatalf("%s: active record history changed:\nbefore: %+v\nafter:  %+v", label, activeHistBefore, got)
		}

		// 就诊视图中目标草稿同样仍在，仍是提交前的同一条草稿。
		er, err := st.EncounterRecords(doc, p.ID, e.ID)
		if err != nil {
			t.Fatalf("%s: encounter records: %v", label, err)
		}
		if len(er) != 3 {
			t.Fatalf("%s: encounter record count changed: %+v", label, er)
		}
		var found bool
		for _, rh := range er {
			if rh.Record.ID == target.ID {
				found = true
				if !rh.HasDraft || rh.CurrentVersion != nil || rh.DraftContent != draftContent ||
					rh.Record.PatientID != p.ID || rh.Record.EncounterID != e.ID ||
					rh.Record.Category != category || len(rh.Versions) != 0 {
					t.Fatalf("%s: target no longer the same plain draft in encounter view: %+v", label, rh)
				}
			}
		}
		if !found {
			t.Fatalf("%s: target draft missing from encounter view", label)
		}

		// 删草稿本就不产生审计事件；失败尝试不能增加、删除或重排任何事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}

		// 接收方仍只读到原先的当前生效内容：目标草稿与另一条草稿都不出现。
		res, err := st.Read(rcv, p.ID, e.ID, category)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}
		if readRecordIDs(res)[target.ID] || readRecordIDs(res)[sibling.ID] {
			t.Fatalf("%s: failed delete leaked a draft to the receiver: %+v", label, res)
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

	// 身份有权、患者未停用、目标记录是草稿，删除条件全部满足，仅本地保存
	// 失败：调用方必须收到保存错误，不能得到成功结果，也不能被误报为记录
	// 不存在、已生效或没有权限。
	if err := s.DeleteDraft(doc, target.ID); err == nil {
		t.Fatal("draft delete must fail when local save fails")
	} else if errors.Is(err, ErrNotFound) || errors.Is(err, ErrActive) ||
		errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) ||
		errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：档案、就诊记录、审计与接收方读取全部维持提交前状态。
	checkIntact("after failed save", s)

	// 关闭后从原数据位置重新打开：先恢复保存条件（还原原数据文件），
	// 未完成的删除在重开后同样没有生效。
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

	// 保存条件恢复后：对同一目标草稿再次提交删除应正常成功。
	if err := s2.DeleteDraft(doc, target.ID); err != nil {
		t.Fatalf("retry delete after save recovered: %v", err)
	}

	// 患者完整档案中不再出现目标草稿。
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if findHistory(chart, target.ID) != nil {
		t.Fatalf("target draft still present after successful delete: %+v", findHistory(chart, target.ID))
	}
	// 另一条草稿继续保留。
	if got := findHistory(chart, sibling.ID); !reflect.DeepEqual(got, siblingHistBefore) {
		t.Fatalf("sibling draft changed by delete:\nbefore: %+v\nafter:  %+v", siblingHistBefore, got)
	}
	// 生效记录的当前版本、旧版本链与更正原因完整保留。
	if got := findHistory(chart, active.ID); !reflect.DeepEqual(got, activeHistBefore) {
		t.Fatalf("active record history changed by delete:\nbefore: %+v\nafter:  %+v", activeHistBefore, got)
	}

	// 该次就诊记录中同样不再出现目标草稿，只剩另一条草稿与生效记录。
	er, err := s2.EncounterRecords(doc, p.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(er) != 2 {
		t.Fatalf("encounter records after delete = %+v, want 2 entries", er)
	}
	for _, rh := range er {
		if rh.Record.ID == target.ID {
			t.Fatalf("deleted target still listed in encounter view: %+v", rh)
		}
	}

	// 按原标识修改该草稿应返回 ErrNotFound（删除已经完成，记录不再存在）。
	if _, err := s2.UpdateDraft(doc, target.ID, "删除后试图修改"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update deleted draft err = %v, want ErrNotFound", err)
	}

	// 删除不产生审计事件：成功后事件内容与顺序仍与删除前完全一致。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit events changed after successful delete:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}

	// 原有授权不受影响：接收方仍只读到生效记录的当前版本，数量不增不减。
	res, err := s2.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, readBefore) || readRecordIDs(res)[target.ID] {
		t.Fatalf("receiver read after delete = %+v, want unchanged effective-only view", res)
	}

	// 成功删除的结果在关闭后重新打开同一数据位置时保持一致。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })
	chart, err = s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if findHistory(chart, target.ID) != nil {
		t.Fatalf("deleted draft reappeared after reopen: %+v", findHistory(chart, target.ID))
	}
	if got := findHistory(chart, sibling.ID); !reflect.DeepEqual(got, siblingHistBefore) {
		t.Fatalf("sibling draft not preserved after reopen: %+v", got)
	}
	if got := findHistory(chart, active.ID); !reflect.DeepEqual(got, activeHistBefore) {
		t.Fatalf("active history not preserved after reopen: %+v", got)
	}
	if _, err := s3.UpdateDraft(doc, target.ID, "重开后试图修改"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update deleted draft after reopen err = %v, want ErrNotFound", err)
	}

	// 边界：已有生效版本和更正历史的记录不能走草稿删除。当前内容、全部旧
	// 版本（版本关系与更正原因）必须完整保留，审计也不发生变化。
	if err := s3.DeleteDraft(doc, active.ID); !errors.Is(err, ErrActive) {
		t.Fatalf("delete active record err = %v, want ErrActive", err)
	}
	chart, err = s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotActive := findHistory(chart, active.ID)
	if gotActive == nil || gotActive.CurrentVersion == nil {
		t.Fatalf("active record missing after rejected delete: %+v", gotActive)
	}
	cur := gotActive.CurrentVersion
	if cur.ID != activeV2.ID || cur.Number != 2 || cur.Content != activeV2Content ||
		cur.PrevID != activeV1.ID || cur.Reason != activeReason {
		t.Fatalf("current version changed after rejected delete: %+v", cur)
	}
	if len(gotActive.Versions) != 2 ||
		!reflect.DeepEqual(gotActive.Versions[0], activeV1) ||
		!reflect.DeepEqual(gotActive.Versions[1], activeV2) ||
		!reflect.DeepEqual(gotActive.Record.Versions, []ID{activeV1.ID, activeV2.ID}) {
		t.Fatalf("version chain changed after rejected delete: %+v", gotActive)
	}
	if gotActive.HasDraft || gotActive.DraftContent != "" {
		t.Fatalf("rejected delete resurfaced a draft on active record: %+v", gotActive)
	}
	audit, err = s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit events changed after rejected delete on active record:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	// 另一条草稿仍在，接收方可见内容仍是生效记录的当前版本。
	if got := findHistory(chart, sibling.ID); !reflect.DeepEqual(got, siblingHistBefore) {
		t.Fatalf("sibling draft changed by rejected delete of active record: %+v", got)
	}
	res, err = s3.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, readBefore) {
		t.Fatalf("receiver read changed after rejected delete:\nbefore: %+v\nafter:  %+v", readBefore, res)
	}
}
