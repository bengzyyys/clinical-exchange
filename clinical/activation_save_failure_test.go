package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestActivationSaveFailureKeepsDraftAndAllowsRetry 覆盖“内部使用者提交、患者未
// 停用、目标记录仍是草稿，同一次就诊已有同类生效记录，但本地保存失败”的生效场景：
// 生效必须把草稿固化为第 1 版、清除草稿并留下生效审计，三者作为同一次成功操作
// 出现——保存失败时一项都不能单独成立。失败后目标草稿的标识、就诊归属、类别与
// 完整草稿内容原样保留，没有当前版本与历史版本，也没有新增生效审计；持有该就诊
// 该类别有效整类授权的接收方仍只能看到原先已生效的内容。保存条件恢复后同一条
// 草稿可以正常生效：版本号从 1 开始，内容与时间取自成功的这次提交；失败与成功
// 两种状态在关闭后重新打开同一数据位置时都保持一致。诊断与医嘱同样适用。
func TestActivationSaveFailureKeepsDraftAndAllowsRetry(t *testing.T) {
	for _, category := range []string{Diagnosis, Order} {
		t.Run(category, func(t *testing.T) {
			testActivationSaveFailure(t, category)
		})
	}
}

func testActivationSaveFailure(t *testing.T, category string) {
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
	p, err := s.RegisterPatient(doc, "生效保存失败患者")
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
	// 目标记录仍是草稿。
	draftContent := "目标草稿内容-" + category
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
	// 草稿——标识、就诊归属、类别与完整草稿内容都在，仍没有当前生效版本与任何
	// 历史版本；既有记录、审计与接收方可见结果全部维持提交前状态，没有新增
	// 目标记录的生效事件，目标草稿也没有被计为新增生效记录。
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
			hist.Record.DraftContent != draftContent || !hist.Record.HasDraft {
			t.Fatalf("%s: draft content lost or changed: %+v", label, hist)
		}
		if hist.CurrentVersion != nil || hist.Record.CurrentVersionID != "" ||
			len(hist.Versions) != 0 || len(hist.Record.Versions) != 0 {
			t.Fatalf("%s: failed activation left a version: %+v", label, hist)
		}
		// 既有生效记录的历史原样保留。
		if got := findHistory(chart, other.ID); !reflect.DeepEqual(got, otherHistBefore) {
			t.Fatalf("%s: other record history changed:\nbefore: %+v\nafter:  %+v", label, otherHistBefore, got)
		}

		// 就诊视图中的目标记录同样仍是草稿。
		er, err := st.EncounterRecords(doc, p.ID, e.ID)
		if err != nil {
			t.Fatalf("%s: encounter records: %v", label, err)
		}
		if len(er) != 2 {
			t.Fatalf("%s: encounter record count changed: %+v", label, er)
		}
		for _, rh := range er {
			if rh.Record.ID == target.ID && (!rh.HasDraft || rh.CurrentVersion != nil) {
				t.Fatalf("%s: target no longer a plain draft in encounter view: %+v", label, rh)
			}
		}

		// 既有审计事件保持原样，失败生效没有新增目标记录的生效事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}

		// 接收方仍只读到原先已生效的内容：失败尝试的内容、版本标识都不出现，
		// 目标草稿也没有被计为新增生效记录。
		res, err := st.Read(rcv, p.ID, e.ID, category)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}
		if readRecordIDs(res)[target.ID] {
			t.Fatalf("%s: failed activation leaked the target draft to the receiver", label)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、患者状态或参数合法性无关。原数据文件先挪到旁边，事后原样还原。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 身份有权、患者未停用、目标记录仍是草稿，仅保存失败：明确报错，
	// 且不能伪装成参数不合法、已生效或权限拒绝等业务结果。
	failTime := clk.t.Add(time.Hour)
	clk.t = failTime
	if _, err := s.ActivateRecord(doc, target.ID); err == nil {
		t.Fatal("activation must fail when local save fails")
	} else if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrActive) ||
		errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) ||
		errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：草稿、既有记录、审计与接收方读取全部维持提交前状态。
	checkIntact("after failed save", s)

	// 关闭后从原数据位置重新打开：先恢复保存条件（还原原数据文件），
	// 失败后的未生效状态在重开后保持一致。
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

	// 保存条件恢复后：同一条草稿仍可修改并正常生效。版本内容必须取自这次
	// 实际提交时的草稿，先生效时间属于成功的这次操作。
	retryContent := "重试时提交的内容-" + category
	if _, err := s2.UpdateDraft(doc, target.ID, retryContent); err != nil {
		t.Fatalf("update draft after save recovered: %v", err)
	}
	successTime := failTime.Add(time.Hour)
	clk.t = successTime
	v1, err := s2.ActivateRecord(doc, target.ID)
	if err != nil {
		t.Fatalf("activate after save recovered: %v", err)
	}
	if v1.Number != 1 || v1.RecordID != target.ID || v1.Category != category ||
		v1.Content != retryContent || v1.PrevID != "" || v1.Reason != "" {
		t.Fatalf("bad v1: %+v", v1)
	}
	// 生效时间属于成功的这次操作，不能沿用失败尝试的时间。
	if !v1.CreatedAt.Equal(successTime) || v1.CreatedAt.Equal(failTime) {
		t.Fatalf("effective time %v does not belong to the successful attempt (failed at %v)", v1.CreatedAt, failTime)
	}

	// 内部档案：草稿已清除，当前版本与唯一历史版本一致。
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, target.ID)
	if hist == nil || hist.CurrentVersion == nil {
		t.Fatalf("history missing after retry: %+v", hist)
	}
	if hist.HasDraft || hist.DraftContent != "" || hist.Record.HasDraft || hist.Record.DraftContent != "" {
		t.Fatalf("draft not cleared after activation: %+v", hist)
	}
	if !reflect.DeepEqual(*hist.CurrentVersion, v1) || hist.Record.CurrentVersionID != v1.ID {
		t.Fatalf("current version mismatch: %+v vs %+v", hist.CurrentVersion, v1)
	}
	if len(hist.Versions) != 1 || !reflect.DeepEqual(hist.Versions[0], v1) ||
		!reflect.DeepEqual(hist.Record.Versions, []ID{v1.ID}) {
		t.Fatalf("history versions wrong after retry: %+v", hist)
	}
	// 既有记录的历史不受目标记录生效影响。
	if got := findHistory(chart, other.ID); !reflect.DeepEqual(got, otherHistBefore) {
		t.Fatalf("other record history changed by retry:\nbefore: %+v\nafter:  %+v", otherHistBefore, got)
	}

	// 只多出一条生效审计：身份与对象对应本次提交，时间属于成功操作。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after retry:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionActivated || last.ObjectType != "record" || last.ObjectID != target.ID ||
		last.ActorID != doc.ID || last.PatientID != p.ID || !last.OccurredAt.Equal(successTime) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if got := auditActions(audit)[ActionActivated]; got != 2 {
		t.Fatalf("activated events = %d, want 2", got)
	}

	// 仍有效的整类授权此时让接收方读到该版本；既有内容原样保留。
	res, err := s2.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 2 {
		t.Fatalf("receiver read after retry = %+v, want 2 records", res)
	}
	byID := map[ID]EffectiveRecord{}
	for _, rec := range res.Records {
		byID[rec.RecordID] = rec
	}
	if got := byID[target.ID]; got.VersionID != v1.ID || got.Version != 1 ||
		got.Content != retryContent || !got.EffectiveAt.Equal(successTime) {
		t.Fatalf("receiver sees wrong target version: %+v", got)
	}
	if got := byID[other.ID]; got.VersionID != otherV1.ID || got.Content != otherContent {
		t.Fatalf("receiver view of existing record changed: %+v", got)
	}

	// 成功后的生效状态在关闭后重新打开同一数据位置时保持一致。
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
	if hist == nil || hist.CurrentVersion == nil || !reflect.DeepEqual(*hist.CurrentVersion, v1) ||
		len(hist.Versions) != 1 || hist.HasDraft {
		t.Fatalf("activated state not preserved after reopen: %+v", hist)
	}
	audit, err = s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(audit)[ActionActivated]; got != 2 {
		t.Fatalf("activated events after reopen = %d, want 2", got)
	}
	res, err = s3.Read(rcv, p.ID, e.ID, category)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 2 || !readRecordIDs(res)[target.ID] {
		t.Fatalf("receiver read after reopen = %+v, want activated target included", res)
	}

	// 成功后再次要求同一记录生效：返回 ErrActive，不能生成第二个版本、
	// 恢复草稿或追加审计。
	if _, err := s3.ActivateRecord(doc, target.ID); !errors.Is(err, ErrActive) {
		t.Fatalf("re-activate err = %v, want ErrActive", err)
	}
	chart, err = s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist = findHistory(chart, target.ID)
	if hist == nil || len(hist.Versions) != 1 || hist.CurrentVersion == nil ||
		hist.CurrentVersion.ID != v1.ID || hist.HasDraft || hist.DraftContent != "" {
		t.Fatalf("re-activate changed the record: %+v", hist)
	}
	audit, err = s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(audit)[ActionActivated]; got != 2 {
		t.Fatalf("re-activate appended an audit event: activated = %d, want 2", got)
	}
}
