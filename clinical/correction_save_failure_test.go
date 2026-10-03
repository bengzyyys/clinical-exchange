package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestCorrectionSaveFailureKeepsRecordAndAudit 覆盖“身份有权操作、患者未停用、
// 业务参数合法，但本地保存失败”的更正场景：操作必须明确报错，且正式记录、
// 版本历史、审计与接收方读取全部维持提交前的状态；保存条件恢复后，可以用
// 失败前的当前版本号成功重试，失败尝试不留下任何痕迹。
func TestCorrectionSaveFailureKeepsRecordAndAudit(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "合成患者丙")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "初诊：高血压 I10")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 已生效且已有一次成功更正：当前为第 2 版。
	clk.t = clk.t.Add(time.Hour)
	v2, err := s.CorrectRecord(doc, r.ID, 1, "复诊：高血压 I10（控制稳定）", "复核更新")
	if err != nil {
		t.Fatal(err)
	}
	// 接收方持有当前有效的整类授权。
	if _, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Minute), clk.t.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 提交前的基线：当前版本、完整历史、审计与接收方读取结果。
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	histBefore := findHistory(chartBefore, r.ID)
	if histBefore == nil || histBefore.CurrentVersion == nil ||
		histBefore.CurrentVersion.ID != v2.ID || len(histBefore.Versions) != 2 {
		t.Fatalf("unexpected baseline history: %+v", histBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore); got[ActionActivated] != 1 || got[ActionCorrected] != 1 || got[ActionGranted] != 1 {
		t.Fatalf("unexpected baseline audit: %v", got)
	}
	readBefore, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if len(readBefore.Records) != 1 || readBefore.Records[0].VersionID != v2.ID {
		t.Fatalf("unexpected baseline read: %+v", readBefore)
	}

	// checkIntact 断言：正式记录、完整历史、审计与接收方读取都与提交前一致，
	// 失败更正没有留下任何正式版本或更正事件。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		hist := findHistory(chart, r.ID)
		if hist == nil || hist.CurrentVersion == nil {
			t.Fatalf("%s: history missing: %+v", label, hist)
		}
		// 当前版本的标识、版本号、完整内容与生效时间与提交前一致。
		cur := hist.CurrentVersion
		if cur.ID != v2.ID || cur.Number != 2 || cur.Content != v2.Content ||
			!cur.CreatedAt.Equal(v2.CreatedAt) || cur.PrevID != v1.ID || cur.Reason != v2.Reason {
			t.Fatalf("%s: current version changed: %+v", label, cur)
		}
		if hist.Record.CurrentVersionID != v2.ID {
			t.Fatalf("%s: record current version id changed: %q", label, hist.Record.CurrentVersionID)
		}
		// 原有两版的内容、顺序、前一版本关系与原因全部保留，没有多出版本。
		if len(hist.Versions) != 2 {
			t.Fatalf("%s: version count changed: %+v", label, hist.Versions)
		}
		if !reflect.DeepEqual(hist.Record.Versions, []ID{v1.ID, v2.ID}) {
			t.Fatalf("%s: version order changed: %v", label, hist.Record.Versions)
		}
		if !reflect.DeepEqual(hist.Versions[0], v1) || !reflect.DeepEqual(hist.Versions[1], v2) {
			t.Fatalf("%s: history versions mutated: %+v", label, hist.Versions)
		}

		// 该次就诊的记录视图同样保持原状。
		er, err := st.EncounterRecords(doc, p.ID, e.ID)
		if err != nil {
			t.Fatalf("%s: encounter records: %v", label, err)
		}
		if len(er) != 1 || er[0].CurrentVersion == nil || er[0].CurrentVersion.ID != v2.ID ||
			len(er[0].Versions) != 2 {
			t.Fatalf("%s: encounter view changed: %+v", label, er)
		}

		// 既有审计事件保持原样，失败更正没有新增更正事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}

		// 接收方仍读到提交前的当前生效版本，且只有当前版本。
		res, err := st.Read(rcv, p.ID, e.ID, Diagnosis)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
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

	// 身份有权、患者未停用、版本号/内容/原因均合法，仅保存失败：明确报错，
	// 且不能伪装成参数校验或版本冲突等业务结果。
	if _, err := s.CorrectRecord(doc, r.ID, 2, "未保存的更正内容", "未保存的更正原因"); err == nil {
		t.Fatal("correction must fail when local save fails")
	} else if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：档案、就诊记录、审计与接收方读取全部维持提交前状态。
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
	// 失败前的当前版本、完整历史与审计在重开后完整存在，
	// 失败更正没有变成正式数据。
	checkIntact("after reopen", s2)

	// 保存条件恢复后：用失败前的当前版本号再次提交合法更正，成功生成
	// 紧接原当前版本的新版本——失败尝试既没有推进版本号，也没有让
	// 原本有效的当前版本号变成冲突。
	clk.t = clk.t.Add(time.Hour)
	v3, err := s2.CorrectRecord(doc, r.ID, 2, "重试成功的更正内容", "保存恢复后重试")
	if err != nil {
		t.Fatalf("retry after save recovered: %v", err)
	}
	if v3.Number != 3 || v3.PrevID != v2.ID || v3.Reason != "保存恢复后重试" ||
		v3.Content != "重试成功的更正内容" || !v3.CreatedAt.Equal(clk.t) {
		t.Fatalf("bad v3: %+v", v3)
	}

	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, r.ID)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != v3.ID || len(hist.Versions) != 3 {
		t.Fatalf("history after retry: %+v", hist)
	}
	// 此前历史未被丢失或覆盖。
	if !reflect.DeepEqual(hist.Versions[0], v1) || !reflect.DeepEqual(hist.Versions[1], v2) {
		t.Fatalf("prior history mutated by retry: %+v", hist.Versions)
	}

	// 只新增一次对应的更正审计：既有事件原样保留，事件总数 +1。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after retry:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionCorrected || last.ObjectID != r.ID || last.ActorID != doc.ID {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if got := auditActions(audit)[ActionCorrected]; got != 2 {
		t.Fatalf("corrected events = %d, want 2", got)
	}

	// 成功更正后接收方读到新的当前版本（既有授权行为保持不变）。
	res, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 1 || res.Records[0].VersionID != v3.ID ||
		res.Records[0].Version != 3 || res.Records[0].Content != v3.Content {
		t.Fatalf("receiver read after retry: %+v", res)
	}
}
