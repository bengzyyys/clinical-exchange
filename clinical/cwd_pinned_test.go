package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withWorkingDir 把进程工作目录临时切到 dir，测试结束后恢复原目录。
// 本包测试均不并行，切换进程级工作目录不会与其他用例互相干扰。
func withWorkingDir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %q: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

// dataRaw 读取数据目录中的快照原文；文件不存在时返回 nil。
func dataRaw(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, dataName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read data file in %q: %v", dir, err)
	}
	return raw
}

// TestOpenRelativePinnedAfterChdirNewLocationMissing 覆盖核心场景：先在工作位置 A
// 以相对路径 "./data" 打开档案，随后调用进程切换到另一个工作位置 B（B 下完全没有
// 同名目录）。已有句柄的保存必须照常成功并写回 A 的原数据目录：不能失败，也不能
// 在 B 下创建另一份临床档案。关闭旧句柄后从原数据目录重新打开，应能查到变更后的
// 完整内容，患者、就诊与记录标识与原档案一致。使用旧句柄期间进程工作目录不能被
// 句柄改动。
func TestOpenRelativePinnedAfterChdirNewLocationMissing(t *testing.T) {
	workA := t.TempDir()
	workB := t.TempDir()

	withWorkingDir(t, workA)
	s, err := Open("./data")
	if err != nil {
		t.Fatalf("open relative: %v", err)
	}
	// 句柄内部固定的目录是打开时解析出的绝对目录。
	wantDir := filepath.Join(workA, "data")
	if s.dir != wantDir {
		t.Fatalf("pinned dir = %q, want %q", s.dir, wantDir)
	}

	p, err := s.RegisterPatient(doc, "相对路径原档案患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	// 切换到另一个没有同名数据目录的工作位置。
	withWorkingDir(t, workB)

	// 既有句柄查询到的仍是原档案。
	if got, err := s.GetPatient(doc, p.ID); err != nil || got.ID != p.ID {
		t.Fatalf("query after chdir: %+v, %v", got, err)
	}

	// 通过原句柄新增就诊、保存草稿：必须成功并落回 A 的原目录。
	e2, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatalf("add encounter after chdir: %v", err)
	}
	r, err := s.CreateDraft(doc, p.ID, e2.ID, Diagnosis, "切换工作目录后保存的诊断草稿")
	if err != nil {
		t.Fatalf("save draft after chdir: %v", err)
	}

	// 新工作位置下不能出现任何临床档案痕迹。
	if _, err := os.Stat(filepath.Join(workB, "data")); !os.IsNotExist(err) {
		t.Fatalf("archive created in new working location: stat err = %v", err)
	}
	// 原数据目录确实拿到了本次写入。
	if raw := dataRaw(t, wantDir); len(raw) == 0 {
		t.Fatal("original data directory did not receive the save")
	}

	// 旧句柄不能改变调用进程已选定的工作目录。
	if wd, err := os.Getwd(); err != nil || wd != workB {
		t.Fatalf("working directory changed by handle ops: %q, err = %v", wd, err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterPatient(doc, "closed"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed handle err = %v, want ErrClosed", err)
	}

	// 从原数据目录（绝对路径）重新打开：刚才的变更与标识完整保留。
	s2, err := Open(wantDir)
	if err != nil {
		t.Fatalf("reopen original dir: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if chart.Patient.ID != p.ID {
		t.Fatalf("patient identity changed: %+v", chart.Patient)
	}
	if len(chart.Encounters) != 2 {
		var ids []ID
		for _, en := range chart.Encounters {
			ids = append(ids, en.ID)
		}
		t.Fatalf("encounters after reopen = %v, want both %q and %q", ids, e.ID, e2.ID)
	}
	hist := findHistory(chart, r.ID)
	if hist == nil || !hist.HasDraft ||
		hist.DraftContent != "切换工作目录后保存的诊断草稿" ||
		hist.Record.PatientID != p.ID || hist.Record.EncounterID != e2.ID {
		t.Fatalf("saved draft not preserved in original archive: %+v", hist)
	}
}

// TestOpenRelativePinnedDoesNotTouchOtherArchive 覆盖“新工作位置下恰好存在同名
// 数据目录且存有另一组患者资料”的情形：原句柄既不能读取那组合成患者，也不能
// 覆盖、追加或把它当作原目录的替代位置。切换工作目录后新发起的 Open 仍按本次
// 打开时的工作位置解释相对路径，两份档案各自独立可用；同一数据目录的句柄互斥、
// 关闭后释放原目录等既有行为保持不变。
func TestOpenRelativePinnedDoesNotTouchOtherArchive(t *testing.T) {
	workA := t.TempDir()
	dirA := filepath.Join(workA, "data")
	workB := t.TempDir()
	dirB := filepath.Join(workB, "data")

	// 先在工作位置 B 建立另一组独立的合成患者档案。
	clk := &fakeClock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	func() {
		withWorkingDir(t, workB)
		seed, err := Open("./data", WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("seed archive B: %v", err)
		}
		bp, err := seed.RegisterPatient(doc, "新位置档案患者乙")
		if err != nil {
			t.Fatal(err)
		}
		be, err := seed.AddEncounter(doc, bp.ID, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		br, err := seed.CreateDraft(doc, bp.ID, be.ID, Order, "乙档案原有的医嘱草稿")
		if err != nil {
			t.Fatal(err)
		}
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}
		t.Logf("archive B patient=%s encounter=%s record=%s", bp.ID, be.ID, br.ID)
	}()
	beforeB := dataRaw(t, dirB)
	if beforeB == nil {
		t.Fatal("archive B data file missing")
	}

	// 在工作位置 A 以相对路径打开原档案。
	withWorkingDir(t, workA)
	s, err := Open("./data")
	if err != nil {
		t.Fatalf("open archive A: %v", err)
	}
	ap, err := s.RegisterPatient(doc, "原档案患者甲")
	if err != nil {
		t.Fatal(err)
	}
	ae, err := s.AddEncounter(doc, ap.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	// 切换到已存在同名数据目录的工作位置 B。
	withWorkingDir(t, workB)

	// 重新解析 B 档案，取出其中的患者标识，用于验证隔离（只读打开，不改数据）。
	var bpID ID
	{
		probe, err := Open(dirB)
		if err != nil {
			t.Fatalf("probe open B: %v", err)
		}
		err = probe.view(func(snap *snapshot) error {
			for id := range snap.Patients {
				bpID = id
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if bpID == "" {
			t.Fatal("archive B has no patient")
		}
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// 原句柄只能看到原档案：查得到甲，查不到乙。
	if got, err := s.GetPatient(doc, ap.ID); err != nil || got.Name != "原档案患者甲" {
		t.Fatalf("original handle lost its patient: %+v, %v", got, err)
	}
	if _, err := s.GetPatient(doc, bpID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("original handle read patient from other archive: %v", err)
	}
	if encs, err := s.ListEncounters(doc, ap.ID); err != nil || len(encs) != 1 || encs[0].ID != ae.ID {
		t.Fatalf("original handle encounters drifted to other archive: %+v, %v", encs, err)
	}

	// 经原句柄保存（更正路径：草稿生效后更正），必须写入 A，不能追加到 B。
	ar, err := s.CreateDraft(doc, ap.ID, ae.ID, Diagnosis, "甲档案的诊断")
	if err != nil {
		t.Fatal(err)
	}
	av1, err := s.ActivateRecord(doc, ar.ID)
	if err != nil {
		t.Fatal(err)
	}
	av2, err := s.CorrectRecord(doc, ar.ID, av1.Number, "甲档案的诊断-更正版", "复核补录")
	if err != nil {
		t.Fatalf("correct after chdir must save into original dir: %v", err)
	}

	if raw := dataRaw(t, dirB); string(raw) != string(beforeB) {
		t.Fatal("other archive in new working location was modified by original handle")
	}
	if raw := dataRaw(t, dirA); len(raw) == 0 {
		t.Fatal("correction was not saved to the original data directory")
	}

	// 切换工作目录后另行 Open：相对路径按本次工作位置解释，独立打开 B 档案。
	other, err := Open("./data")
	if err != nil {
		t.Fatalf("relative open after chdir should resolve against new cwd: %v", err)
	}
	if other.dir != dirB {
		t.Fatalf("new open pinned dir = %q, want %q", other.dir, dirB)
	}
	if _, err := other.GetPatient(doc, bpID); err != nil {
		t.Fatalf("new handle should see archive B patient: %v", err)
	}
	if _, err := other.GetPatient(doc, ap.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("new archive B handle leaked patient from archive A: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	// 同一数据目录仍只能被一个句柄占用：回到工作位置 A 再次相对打开应被拒绝。
	withWorkingDir(t, workA)
	if _, err := Open("./data"); err == nil {
		t.Fatal("second handle on the same original directory was allowed")
	}

	// 关闭旧句柄释放的是原目录 A（此刻工作目录在 A，可直接相对重开验证）；
	// 关闭后句柄继续返回 ErrClosed。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPatient(doc, ap.ID); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed handle err = %v, want ErrClosed", err)
	}
	reopened, err := Open("./data")
	if err != nil {
		t.Fatalf("reopen after old handle closed: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	chart, err := reopened.Chart(doc, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, ar.ID)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != av2.ID ||
		len(hist.Versions) != 2 || hist.Versions[1].PrevID != av1.ID {
		t.Fatalf("correction chain not preserved in original archive: %+v", hist)
	}

	// B 档案从头到尾没有被原句柄读取之外的任何操作改动。
	if raw := dataRaw(t, dirB); string(raw) != string(beforeB) {
		t.Fatal("other archive changed despite pinned original handle")
	}
}

// TestPinnedHandleSaveFailureDoesNotFallBackToOtherLocation 覆盖原目录确实无法
// 保存的情形：必须返回保存错误（不能伪装成业务错误），已有记录与审计保持原样，
// 即使新工作位置存在可写的同名数据目录，也不能转而写入那里。保存条件恢复后
// 重试成功，变更仍只落入原目录。
func TestPinnedHandleSaveFailureDoesNotFallBackToOtherLocation(t *testing.T) {
	workA := t.TempDir()
	dirA := filepath.Join(workA, "data")
	workB := t.TempDir()
	dirB := filepath.Join(workB, "data")

	// B 下准备一份可写的同名独立档案。
	func() {
		withWorkingDir(t, workB)
		seed, err := Open("./data")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := seed.RegisterPatient(doc, "可写同名目录里的患者乙"); err != nil {
			t.Fatal(err)
		}
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	beforeB := dataRaw(t, dirB)

	withWorkingDir(t, workA)
	s, err := Open("./data")
	if err != nil {
		t.Fatal(err)
	}
	ap, err := s.RegisterPatient(doc, "原目录无法保存时的患者甲")
	if err != nil {
		t.Fatal(err)
	}
	ae, err := s.AddEncounter(doc, ap.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ar, err := s.CreateDraft(doc, ap.ID, ae.ID, Diagnosis, "原始草稿内容")
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := s.AuditEvents(doc, ap.ID)
	if err != nil {
		t.Fatal(err)
	}

	withWorkingDir(t, workB)

	// 让 A 的原数据目录无法整体落盘：数据文件路径被同名目录占据，原子改名失败。
	dataPath := filepath.Join(dirA, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	_, err = s.UpdateDraft(doc, ar.ID, "试图保存的新草稿内容")
	if err == nil {
		t.Fatal("save must fail when original directory is unwritable")
	} else if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrActive) || errors.Is(err, ErrAccessDenied) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrNotFound) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}

	// 已有记录与审计保持原样。
	chart, err := s.Chart(doc, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, ar.ID)
	if hist == nil || !hist.HasDraft || hist.DraftContent != "原始草稿内容" ||
		hist.Record.ID != ar.ID || hist.CurrentVersion != nil {
		t.Fatalf("state changed after failed save: %+v", hist)
	}
	auditAfter, err := s.AuditEvents(doc, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditAfter) != len(auditBefore) {
		t.Fatalf("audit changed after failed save: before %d after %d", len(auditBefore), len(auditAfter))
	}

	// 可写的同名目录 B 绝不能成为替代写入位置。
	if raw := dataRaw(t, dirB); string(raw) != string(beforeB) {
		t.Fatal("fallback write went into the other same-named data directory")
	}
	if _, statErr := os.Stat(filepath.Join(dirB, dataName+".tmp")); !os.IsNotExist(statErr) {
		t.Fatalf("temp file leaked into other archive: %v", statErr)
	}

	// 恢复原目录的保存条件后重试：成功，且变更只落在 A。
	if err := os.Remove(dataPath); err != nil {
		t.Fatalf("unblock data path: %v", err)
	}
	if err := os.Rename(backupPath, dataPath); err != nil {
		t.Fatalf("restore data file: %v", err)
	}
	updated, err := s.UpdateDraft(doc, ar.ID, "恢复后成功保存的草稿内容")
	if err != nil {
		t.Fatalf("retry after save recovered: %v", err)
	}
	if updated.ID != ar.ID || updated.PatientID != ap.ID || updated.EncounterID != ae.ID {
		t.Fatalf("retry changed record identity: %+v", updated)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dirA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	chart, err = reopened.Chart(doc, ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist = findHistory(chart, ar.ID)
	if hist == nil || !hist.HasDraft || hist.DraftContent != "恢复后成功保存的草稿内容" {
		t.Fatalf("recovered save not preserved in original archive: %+v", hist)
	}
	if raw := dataRaw(t, dirB); string(raw) != string(beforeB) {
		t.Fatal("other archive was modified during save failure/recovery")
	}
}
