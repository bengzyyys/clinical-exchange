package clinical

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---- 本地保存失败的注入与恢复 ----

// blockPersist 让后续落盘失败：persist 总是先写 dataName+".tmp" 再原子改名，
// 在该路径上放一个目录即可让写入稳定失败（不依赖文件权限，root 下同样有效）。
func blockPersist(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, dataName+".tmp"), 0o755); err != nil {
		t.Fatalf("block persist: %v", err)
	}
}

// restorePersist 移除占位目录，恢复本地保存能力。
func restorePersist(t *testing.T, dir string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, dataName+".tmp")); err != nil {
		t.Fatalf("restore persist: %v", err)
	}
}

func dataFileBytes(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, dataName))
	if err != nil {
		t.Fatalf("read data file: %v", err)
	}
	return raw
}

// mustHistory 取出指定记录的完整历史视图。
func mustHistory(t *testing.T, s *Store, pid, recordID ID) RecordHistory {
	t.Helper()
	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	h := findHistory(chart, recordID)
	if h == nil {
		t.Fatalf("record %q missing from chart", recordID)
	}
	return *h
}

// assertTwoVersionHistory 断言记录仍保持失败前的两版历史：
// 当前版本标识/版本号/内容/生效时间与两版的内容、顺序、版本链、原因全部一致。
func assertTwoVersionHistory(t *testing.T, h RecordHistory, v1, v2 Version) {
	t.Helper()
	if h.CurrentVersion == nil {
		t.Fatal("current version missing")
	}
	cv := h.CurrentVersion
	if cv.ID != v2.ID || cv.Number != 2 || cv.Content != v2.Content || !cv.CreatedAt.Equal(v2.CreatedAt) {
		t.Fatalf("current version changed: %+v, want v2 %+v", cv, v2)
	}
	if h.Record.CurrentVersionID != v2.ID {
		t.Fatalf("record current version id = %q, want %q", h.Record.CurrentVersionID, v2.ID)
	}
	if len(h.Versions) != 2 {
		t.Fatalf("versions len = %d, want 2: %+v", len(h.Versions), h.Versions)
	}
	got1, got2 := h.Versions[0], h.Versions[1]
	if got1.ID != v1.ID || got1.Number != 1 || got1.Content != v1.Content || got1.PrevID != "" || got1.Reason != "" {
		t.Fatalf("v1 mutated: %+v", got1)
	}
	if got2.ID != v2.ID || got2.Number != 2 || got2.Content != v2.Content ||
		got2.PrevID != v1.ID || got2.Reason != v2.Reason || !got2.CreatedAt.Equal(v2.CreatedAt) {
		t.Fatalf("v2 mutated: %+v", got2)
	}
	if len(h.Record.Versions) != 2 || h.Record.Versions[0] != v1.ID || h.Record.Versions[1] != v2.ID {
		t.Fatalf("version id order changed: %v", h.Record.Versions)
	}
}

// TestCorrectionPersistFailureKeepsRecordAndAudit 保护“更正保存失败时正式记录与
// 审计都维持原状”的已有行为：身份有权操作、患者未停用、业务参数全部合法，
// 仅本地保存失败。参数校验失败（空原因、过期版本号等）由其他测试覆盖，
// 不能代替本场景。
func TestCorrectionPersistFailureKeepsRecordAndAudit(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	openStore := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := openStore()

	p, err := s.RegisterPatient(doc, "合成患者甲")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateDraft(doc, p.ID, enc.ID, Diagnosis, "初诊：高血压 I10")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 一次成功更正，形成两版历史。
	clk.t = clk.t.Add(time.Hour)
	v2, err := s.CorrectRecord(doc, rec.ID, 1, "更正：高血压 I10（复核）", "补充检验依据")
	if err != nil {
		t.Fatal(err)
	}
	// 接收方持有覆盖该就诊诊断类别的有效授权。
	if _, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: enc.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 失败前的正式状态：磁盘快照、完整历史与审计。
	diskBefore := dataFileBytes(t, dir)
	auditBefore := mustAudit(t, s, p.ID)
	if got := auditActions(auditBefore)[ActionCorrected]; got != 1 {
		t.Fatalf("correction audits before = %d, want 1", got)
	}

	// 本地保存失败期间，内部使用者提交参数完全合法的更正。
	clk.t = clk.t.Add(time.Hour)
	blockPersist(t, dir)
	_, err = s.CorrectRecord(doc, rec.ID, 2, "失败更正：不应生效的内容", "保存失败的原因")
	if err == nil {
		t.Fatal("correction with broken local save must return an error")
	}
	restorePersist(t, dir)

	// 磁盘上的正式数据一个字节都不能变。
	if got := dataFileBytes(t, dir); !bytes.Equal(got, diskBefore) {
		t.Fatal("failed correction modified the on-disk snapshot")
	}

	// 患者完整档案：当前版本与两版历史保持提交前原状。
	assertTwoVersionHistory(t, mustHistory(t, s, p.ID, rec.ID), v1, v2)

	// 该次就诊的记录视图同样保持原状。
	hist, err := s.EncounterRecords(doc, p.ID, enc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("encounter records len = %d, want 1", len(hist))
	}
	assertTwoVersionHistory(t, hist[0], v1, v2)

	// 审计保持原样：失败更正不得新增更正事件。
	auditAfter := mustAudit(t, s, p.ID)
	if len(auditAfter) != len(auditBefore) {
		t.Fatalf("audit count changed: before=%d after=%d", len(auditBefore), len(auditAfter))
	}
	for i := range auditBefore {
		if auditAfter[i] != auditBefore[i] {
			t.Fatalf("audit event %d changed: %+v -> %+v", i, auditBefore[i], auditAfter[i])
		}
	}

	// 接收方仍读到提交前的当前生效版本，而不是失败操作尝试写入的内容。
	res, err := s.Read(rcv, p.ID, enc.ID, Diagnosis)
	if err != nil {
		t.Fatalf("receiver read: %v", err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("receiver records len = %d, want 1", len(res.Records))
	}
	eff := res.Records[0]
	if eff.RecordID != rec.ID || eff.VersionID != v2.ID || eff.Version != 2 ||
		eff.Content != v2.Content || !eff.EffectiveAt.Equal(v2.CreatedAt) {
		t.Fatalf("receiver sees wrong version: %+v, want v2 %+v", eff, v2)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 关闭后从原数据位置重新打开：失败前的当前版本、完整历史与审计仍完整，
	// 失败更正不能变成正式数据。
	s2 := openStore()
	assertTwoVersionHistory(t, mustHistory(t, s2, p.ID, rec.ID), v1, v2)
	auditReopen := mustAudit(t, s2, p.ID)
	if len(auditReopen) != len(auditBefore) {
		t.Fatalf("audit count after reopen = %d, want %d", len(auditReopen), len(auditBefore))
	}
	for i := range auditBefore {
		if auditReopen[i] != auditBefore[i] {
			t.Fatalf("audit event %d changed after reopen: %+v -> %+v", i, auditBefore[i], auditReopen[i])
		}
	}
	res2, err := s2.Read(rcv, p.ID, enc.ID, Diagnosis)
	if err != nil {
		t.Fatalf("receiver read after reopen: %v", err)
	}
	if len(res2.Records) != 1 || res2.Records[0].VersionID != v2.ID || res2.Records[0].Content != v2.Content {
		t.Fatalf("receiver read after reopen = %+v, want v2", res2.Records)
	}

	// 本地保存恢复后，用失败前的当前版本号再次提交合法更正：
	// 失败尝试既没有提前推进版本号，也没有让原本有效的版本号变成冲突。
	clk.t = clk.t.Add(time.Hour)
	v3, err := s2.CorrectRecord(doc, rec.ID, 2, "更正：高血压 I10（再次复核）", "保存恢复后的更正")
	if err != nil {
		t.Fatalf("correction after save recovered: %v", err)
	}
	if v3.Number != 3 || v3.PrevID != v2.ID || v3.Content != "更正：高血压 I10（再次复核）" ||
		v3.Reason != "保存恢复后的更正" {
		t.Fatalf("bad v3: %+v", v3)
	}

	// 只新增一次对应的更正审计；历史延伸为三版且旧内容原样保留。
	if got := auditActions(mustAudit(t, s2, p.ID))[ActionCorrected]; got != 2 {
		t.Fatalf("correction audits after recovery = %d, want 2", got)
	}
	h3 := mustHistory(t, s2, p.ID, rec.ID)
	if len(h3.Versions) != 3 || h3.CurrentVersion == nil || h3.CurrentVersion.ID != v3.ID {
		t.Fatalf("history after recovery = %+v", h3)
	}
	if h3.Versions[0].ID != v1.ID || h3.Versions[1].ID != v2.ID || h3.Versions[2].ID != v3.ID {
		t.Fatalf("version order after recovery = %v", h3.Record.Versions)
	}
	if h3.Versions[0].Content != v1.Content || h3.Versions[1].Content != v2.Content || h3.Versions[1].Reason != v2.Reason {
		t.Fatalf("old versions mutated after recovery: %+v", h3.Versions)
	}

	// 接收方随后读到的是这次实际成功的新版本。
	res3, err := s2.Read(rcv, p.ID, enc.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if len(res3.Records) != 1 || res3.Records[0].VersionID != v3.ID || res3.Records[0].Content != v3.Content {
		t.Fatalf("receiver read after recovery = %+v, want v3", res3.Records)
	}

	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCorrectionPersistFailureErrorIsNotBusinessConflict 确认保存失败返回的
// 是明确的错误，而不是被误报为版本冲突等业务结果。
func TestCorrectionPersistFailureErrorIsNotBusinessConflict(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	p, err := s.RegisterPatient(doc, "合成患者乙")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.CreateDraft(doc, p.ID, enc.ID, Diagnosis, "诊断")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, rec.ID); err != nil {
		t.Fatal(err)
	}

	blockPersist(t, dir)
	_, err = s.CorrectRecord(doc, rec.ID, 1, "新内容", "原因")
	restorePersist(t, dir)
	if err == nil {
		t.Fatal("expected error when local save fails")
	}
	for _, business := range []error{ErrConflict, ErrInvalidArgument, ErrNotFound, ErrAccessDenied, ErrDeactivated, ErrClosed} {
		if errors.Is(err, business) {
			t.Fatalf("save failure reported as business error %v", business)
		}
	}
}
