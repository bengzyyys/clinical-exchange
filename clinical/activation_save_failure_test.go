package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestActivationSaveFailureKeepsDraftAndAudit 覆盖“身份有权操作、患者未停用、
// 记录仍是草稿，业务条件全部合法，但本地保存失败”的生效场景：操作必须明确
// 报错，且草稿、既有生效记录、审计与接收方读取全部维持提交前的状态；保存
// 条件恢复后，同一条草稿仍能正常生效为第 1 版，失败尝试不留下任何痕迹。
// 诊断与医嘱走同一条生效路径，两种类别都验证。
func TestActivationSaveFailureKeepsDraftAndAudit(t *testing.T) {
	for _, category := range []string{Diagnosis, Order} {
		category := category
		t.Run(category, func(t *testing.T) {
			testActivationSaveFailure(t, category)
		})
	}
}

func testActivationSaveFailure(t *testing.T, category string) {
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
	p, err := s.RegisterPatient(doc, "合成患者丁")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 同一就诊下已有一条同类别的生效记录。
	r1, err := s.CreateDraft(doc, p.ID, e.ID, category, "已生效的既有内容")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, r1.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 目标记录仍是草稿。
	r2, err := s.CreateDraft(doc, p.ID, e.ID, category, "待生效的草稿内容")
	if err != nil {
		t.Fatal(err)
	}
	// 接收方持有该就诊该类别当前有效的整类授权。
	if _, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: category}},
		clk.t.Add(-time.Minute), clk.t.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 提交前的基线：完整档案、审计与接收方读取结果。
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist1Before := findHistory(chartBefore, r1.ID)
	if hist1Before == nil || hist1Before.CurrentVersion == nil ||
		hist1Before.CurrentVersion.ID != v1.ID || len(hist1Before.Versions) != 1 {
		t.Fatalf("unexpected baseline history for existing record: %+v", hist1Before)
	}
	hist2Before := findHistory(chartBefore, r2.ID)
	if hist2Before == nil || !hist2Before.HasDraft || hist2Before.CurrentVersion != nil ||
		len(hist2Before.Versions) != 0 {
		t.Fatalf("unexpected baseline history for target draft: %+v", hist2Before)
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
	if len(readBefore.Records) != 1 || readBefore.Records[0].RecordID != r1.ID ||
		readBefore.Records[0].VersionID != v1.ID {
		t.Fatalf("unexpected baseline read: %+v", readBefore)
	}

	// checkDraftIntact 断言：目标草稿、既有生效记录、审计与接收方读取都与
	// 提交前一致，失败的生效尝试没有留下任何版本或生效事件。
	checkDraftIntact := func(label string, st *Store) {
		t.Helper()

		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		// 目标草稿完整保留：标识、就诊归属、类别与提交前的完整草稿内容；
		// 仍没有当前生效版本，也没有任何历史版本。
		hist := findHistory(chart, r2.ID)
		if hist == nil {
			t.Fatalf("%s: target draft missing: %+v", label, chart.Records)
		}
		if hist.Record.ID != r2.ID || hist.Record.PatientID != p.ID ||
			hist.Record.EncounterID != e.ID || hist.Record.Category != category {
			t.Fatalf("%s: target record identity changed: %+v", label, hist.Record)
		}
		if !hist.HasDraft || hist.DraftContent != "待生效的草稿内容" ||
			hist.Record.DraftContent != "待生效的草稿内容" {
			t.Fatalf("%s: draft content lost: %+v", label, hist)
		}
		if hist.CurrentVersion != nil || hist.Record.CurrentVersionID != "" ||
			len(hist.Versions) != 0 || len(hist.Record.Versions) != 0 {
			t.Fatalf("%s: failed activation left a version: %+v", label, hist)
		}

		// 既有生效记录的历史保持原样。
		hist1 := findHistory(chart, r1.ID)
		if hist1 == nil || hist1.CurrentVersion == nil {
			t.Fatalf("%s: existing record history missing: %+v", label, hist1)
		}
		if !reflect.DeepEqual(*hist1, *hist1Before) {
			t.Fatalf("%s: existing record history changed:\nbefore: %+v\nafter:  %+v",
				label, *hist1Before, *hist1)
		}

		// 该次就诊的记录视图同样保持原状：仍是一生一草两条记录。
		er, err := st.EncounterRecords(doc, p.ID, e.ID)
		if err != nil {
			t.Fatalf("%s: encounter records: %v", label, err)
		}
		if len(er) != 2 {
			t.Fatalf("%s: encounter view changed: %+v", label, er)
		}

		// 既有审计事件保持原样，失败生效没有新增目标记录的生效事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		for _, ev := range audit {
			if ev.Action == ActionActivated && ev.ObjectID == r2.ID {
				t.Fatalf("%s: failed attempt left an activation event: %+v", label, ev)
			}
		}

		// 接收方仍只读到原先已生效的内容：失败尝试的内容、版本标识都不可见，
		// 目标草稿也没有被计为新增生效记录。
		res, err := st.Read(rcv, p.ID, e.ID, category)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}
		for _, rec := range res.Records {
			if rec.RecordID == r2.ID || rec.Content == "待生效的草稿内容" {
				t.Fatalf("%s: failed attempt leaked to receiver: %+v", label, rec)
			}
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、参数校验无关。原数据文件先挪到旁边，事后原样还原。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 身份有权、患者未停用、记录仍是草稿，仅保存失败：明确报错，
	// 且不能伪装成权限拒绝或参数不合法等业务结果。
	failTime := clk.t
	if _, err := s.ActivateRecord(doc, r2.ID); err == nil {
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
	checkDraftIntact("after failed save", s)

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
	// 失败后的未生效状态在重开后完整存在，失败尝试没有变成正式数据。
	checkDraftIntact("after reopen", s2)

	// 保存条件恢复后：先修改草稿，再对同一条草稿生效。成功版本的内容必须
	// 取自这次实际提交时的草稿，版本号从 1 开始，生效时间属于成功的这次
	// 操作，不能沿用失败尝试的时间。
	if _, err := s2.UpdateDraft(doc, r2.ID, "提交时实际的草稿内容"); err != nil {
		t.Fatalf("update draft after save recovered: %v", err)
	}
	clk.t = clk.t.Add(time.Hour)
	v, err := s2.ActivateRecord(doc, r2.ID)
	if err != nil {
		t.Fatalf("activation after save recovered: %v", err)
	}
	if v.Number != 1 || v.RecordID != r2.ID || v.Category != category ||
		v.Content != "提交时实际的草稿内容" || v.PrevID != "" || v.Reason != "" {
		t.Fatalf("bad v1: %+v", v)
	}
	if !v.CreatedAt.Equal(clk.t) || v.CreatedAt.Equal(failTime) {
		t.Fatalf("effective time %v must belong to the successful attempt, not %v", v.CreatedAt, failTime)
	}

	// checkActivated 断言：草稿已清除，当前版本与唯一历史版本一致，既有
	// 记录与授权行为不变，且只多出一条对应本次提交的生效审计。
	checkActivated := func(label string, st *Store) {
		t.Helper()

		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		hist := findHistory(chart, r2.ID)
		if hist == nil {
			t.Fatalf("%s: record missing: %+v", label, chart.Records)
		}
		if hist.HasDraft || hist.DraftContent != "" || hist.Record.DraftContent != "" {
			t.Fatalf("%s: draft not cleared: %+v", label, hist)
		}
		if hist.CurrentVersion == nil || hist.Record.CurrentVersionID != v.ID ||
			!reflect.DeepEqual(*hist.CurrentVersion, v) {
			t.Fatalf("%s: current version mismatch: %+v", label, hist.CurrentVersion)
		}
		if len(hist.Versions) != 1 || !reflect.DeepEqual(hist.Versions[0], v) ||
			!reflect.DeepEqual(hist.Record.Versions, []ID{v.ID}) {
			t.Fatalf("%s: history mismatch: %+v", label, hist)
		}

		// 既有生效记录的历史保持原样。
		hist1 := findHistory(chart, r1.ID)
		if hist1 == nil || !reflect.DeepEqual(*hist1, *hist1Before) {
			t.Fatalf("%s: existing record history changed:\nbefore: %+v\nafter:  %+v",
				label, *hist1Before, hist1)
		}

		// 只新增一条生效审计：既有事件原样保留，事件总数 +1，
		// 身份与对象对应本次提交，时间属于成功的这次操作。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
			t.Fatalf("%s: audit after activation:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		last := audit[len(audit)-1]
		if last.Action != ActionActivated || last.ObjectType != "record" ||
			last.ObjectID != r2.ID || last.ActorID != doc.ID || last.PatientID != p.ID {
			t.Fatalf("%s: unexpected new audit event: %+v", label, last)
		}
		if !last.OccurredAt.Equal(clk.t) || last.OccurredAt.Equal(failTime) {
			t.Fatalf("%s: audit time %v must belong to the successful attempt", label, last.OccurredAt)
		}
		if got := auditActions(audit)[ActionActivated]; got != 2 {
			t.Fatalf("%s: activated events = %d, want 2", label, got)
		}

		// 仍有效的整类授权此时让接收方读到该版本，既有内容保持原样。
		res, err := st.Read(rcv, p.ID, e.ID, category)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if len(res.Records) != 2 {
			t.Fatalf("%s: receiver read after activation: %+v", label, res)
		}
		byID := map[ID]EffectiveRecord{}
		for _, rec := range res.Records {
			byID[rec.RecordID] = rec
		}
		eff, ok := byID[r2.ID]
		if !ok || eff.VersionID != v.ID || eff.Version != 1 ||
			eff.Content != "提交时实际的草稿内容" || !eff.EffectiveAt.Equal(v.CreatedAt) {
			t.Fatalf("%s: receiver cannot read the new version: %+v", label, res)
		}
		if !reflect.DeepEqual(byID[r1.ID], readBefore.Records[0]) {
			t.Fatalf("%s: existing effective record changed: %+v", label, byID[r1.ID])
		}
	}

	checkActivated("after successful activation", s2)

	// 成功后再次要求同一记录生效：仍返回 ErrActive，不能生成第二个版本、
	// 恢复草稿或追加审计。
	if _, err := s2.ActivateRecord(doc, r2.ID); !errors.Is(err, ErrActive) {
		t.Fatalf("re-activate err = %v, want ErrActive", err)
	}
	checkActivated("after redundant activation", s2)

	// 成功后的生效状态在关闭后重新打开同一数据位置时保持一致。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })
	checkActivated("after reopen", s3)
}
