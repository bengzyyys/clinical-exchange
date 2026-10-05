package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件为“相对路径打开后调用进程切换工作目录”补回归保障：
// 已成功打开的句柄必须始终绑定 Open 成功时确定的（绝对）数据目录，
// 工作目录变化既不能把保存重定向到新位置下的同名目录，也不能因新位置
// 缺少同名目录而让保存失败；切换工作目录后的新 Open 仍按新工作位置解释
// 相对路径，两份数据各自独立、互不读写覆盖。

// pinWorkingDir 切换进程工作目录，并在测试结束时切回原位置。
// 工作目录是进程级全局状态，本包测试均不使用 t.Parallel。
func pinWorkingDir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

// TestOpenRelativePathPinnedAcrossChdir 是主场景：在甲工作位置以相对路径
// ./data 打开档案，切换到（没有同名目录的）乙工作位置后，原句柄的查询与
// 新增草稿保存仍落在甲位置；乙位置不会被创建出另一份档案。关闭后从甲位置
// 重新打开，刚保存的草稿及其患者、就诊、记录标识完整可查；已关闭句柄
// 继续返回 ErrClosed。
func TestOpenRelativePathPinnedAcrossChdir(t *testing.T) {
	root := t.TempDir()
	locA := filepath.Join(root, "loc-a")
	locB := filepath.Join(root, "loc-b")
	if err := os.MkdirAll(locA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(locB, 0o755); err != nil {
		t.Fatal(err)
	}

	// 在甲工作位置按 README 示例的方式打开相对数据目录。
	pinWorkingDir(t, locA)
	s, err := Open("./data")
	if err != nil {
		t.Fatalf("open relative: %v", err)
	}
	wantDataDir := filepath.Join(locA, "data")
	if got := s.dir; !filepath.IsAbs(got) || got != wantDataDir {
		t.Fatalf("data dir not pinned to absolute open-time path: got %q, want %q", got, wantDataDir)
	}

	patient, err := s.RegisterPatient(doc, "切换工作目录患者甲")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s.AddEncounter(doc, patient.ID, time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	// 切换到乙工作位置：该位置完全没有同名 data 目录。
	pinWorkingDir(t, locB)
	if wd, _ := os.Getwd(); wd != locB {
		t.Fatalf("working dir not changed: %s", wd)
	}

	// 通过原句柄查询到的仍是甲位置的患者资料。
	if got, err := s.GetPatient(doc, patient.ID); err != nil || got.ID != patient.ID {
		t.Fatalf("old handle lost patient after chdir: %+v %v", got, err)
	}

	// 通过原句柄为已有患者保存一条诊断草稿，必须成功。
	draftContent := "在乙工作位置经原句柄保存的草稿"
	draft, err := s.CreateDraft(doc, patient.ID, enc.ID, Diagnosis, draftContent)
	if err != nil {
		t.Fatalf("save through old handle must not depend on cwd: %v", err)
	}

	// 句柄的任何操作都不改变调用进程已选定的工作目录。
	if wd, _ := os.Getwd(); wd != locB {
		t.Fatalf("using old handle changed process working dir: %s", wd)
	}

	// 变更落在甲位置的数据文件中，乙位置下绝不能出现同名目录或数据文件。
	dataA := filepath.Join(wantDataDir, dataName)
	if raw, err := os.ReadFile(dataA); err != nil {
		t.Fatalf("change not saved to original data dir: %v", err)
	} else if !strings.Contains(string(raw), draft.ID) || !strings.Contains(string(raw), draftContent) {
		t.Fatalf("original data file missing saved draft:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(locB, "data")); !os.IsNotExist(err) {
		t.Fatalf("new working location must not get a data directory, stat: %v", err)
	}

	// 关闭旧句柄后，释放的是甲位置的目录；已关闭句柄继续返回 ErrClosed。
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := s.GetPatient(doc, patient.ID); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed handle: got %v, want ErrClosed", err)
	}
	if err := s.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("repeat close: got %v, want ErrClosed", err)
	}

	// 从原数据目录重新打开：刚保存的草稿与患者、就诊、记录标识全部保持一致。
	s2, err := Open(wantDataDir)
	if err != nil {
		t.Fatalf("reopen original dir: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	chart, err := s2.Chart(doc, patient.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, draft.ID)
	if hist == nil {
		t.Fatalf("saved draft missing after reopen")
	}
	if hist.Record.ID != draft.ID || hist.Record.PatientID != patient.ID ||
		hist.Record.EncounterID != enc.ID || !hist.HasDraft || hist.DraftContent != draftContent {
		t.Fatalf("reopened draft identity/content changed: %+v", hist.Record)
	}
}

// TestOldHandleIgnoresSameNamedDirAtNewWorkingDir：乙工作位置下恰好已有同名
// data 目录且其中存放另一组合成患者资料时，原句柄不能读取、覆盖或追加那里
// 的任何数据，也不能把它当作原目录的替代位置。
func TestOldHandleIgnoresSameNamedDirAtNewWorkingDir(t *testing.T) {
	root := t.TempDir()
	locA := filepath.Join(root, "loc-a")
	locB := filepath.Join(root, "loc-b")
	if err := os.MkdirAll(locA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(locB, 0o755); err != nil {
		t.Fatal(err)
	}

	// 先在乙位置建立一份独立档案（含乙患者），随后关闭释放占用。
	pinWorkingDir(t, locB)
	sBSeed, err := Open("./data")
	if err != nil {
		t.Fatal(err)
	}
	patientB, err := sBSeed.RegisterPatient(doc, "乙位置自有患者")
	if err != nil {
		t.Fatal(err)
	}
	dataB := filepath.Join(locB, "data", dataName)
	bBefore, err := os.ReadFile(dataB)
	if err != nil {
		t.Fatalf("read B snapshot: %v", err)
	}
	if err := sBSeed.Close(); err != nil {
		t.Fatal(err)
	}

	// 在甲位置以相对路径打开原句柄并登记甲患者。
	pinWorkingDir(t, locA)
	sA, err := Open("./data")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sA.Close() })
	patientA, err := sA.RegisterPatient(doc, "甲位置原档案患者")
	if err != nil {
		t.Fatal(err)
	}
	encA, err := sA.AddEncounter(doc, patientA.ID, time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	// 切换到乙位置（同名目录中有另一组患者）。
	pinWorkingDir(t, locB)

	// 原句柄读不到乙位置的患者，甲位置的患者照常可见。
	if _, err := sA.GetPatient(doc, patientB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old handle read patient from same-named dir at new cwd: %v", err)
	}
	if _, err := sA.GetPatient(doc, patientA.ID); err != nil {
		t.Fatalf("old handle lost its own patient: %v", err)
	}

	// 原句柄的新增就诊/草稿保存必须只落在甲位置；乙位置快照逐字节不变。
	if _, err := sA.CreateDraft(doc, patientA.ID, encA.ID, Diagnosis, "只属于甲位置的草稿"); err != nil {
		t.Fatalf("save through old handle: %v", err)
	}
	if bAfter, err := os.ReadFile(dataB); err != nil {
		t.Fatalf("B snapshot unreadable: %v", err)
	} else if string(bAfter) != string(bBefore) {
		t.Fatalf("old handle overwrote/appended to same-named dir at new cwd")
	}
	// 乙位置档案中依旧只有乙患者：重开乙位置档案，甲患者不可见。
	sB, err := Open(filepath.Join(locB, "data"))
	if err != nil {
		t.Fatalf("reopen independent B store: %v", err)
	}
	if _, err := sB.GetPatient(doc, patientA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B store leaked patient A: %v", err)
	}
	if _, err := sB.GetPatient(doc, patientB.ID); err != nil {
		t.Fatalf("B store lost its own patient: %v", err)
	}
	if err := sB.Close(); err != nil {
		t.Fatal(err)
	}

	// 切回甲位置后再次相对打开：解析出的仍是甲位置，目录被原句柄占用，
	// 不能因为工作目录变化而把乙位置误判为占用或替代位置。
	pinWorkingDir(t, locA)
	if _, err := Open("./data"); err == nil {
		t.Fatal("relative reopen of occupied original dir must fail")
	}
}

// TestRelativeOpenAfterChdirUsesNewWorkingDir：固定关系只针对已打开句柄。
// 切换工作目录后另行 Open，相对路径按新调用时刻的工作位置解释，得到的是
// 另一份独立数据。
func TestRelativeOpenAfterChdirUsesNewWorkingDir(t *testing.T) {
	root := t.TempDir()
	locA := filepath.Join(root, "loc-a")
	locB := filepath.Join(root, "loc-b")
	if err := os.MkdirAll(locA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(locB, 0o755); err != nil {
		t.Fatal(err)
	}

	pinWorkingDir(t, locA)
	sA, err := Open("./data")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sA.Close() })
	patientA, err := sA.RegisterPatient(doc, "相对重解析患者甲")
	if err != nil {
		t.Fatal(err)
	}
	if got := sA.dir; got != filepath.Join(locA, "data") {
		t.Fatalf("sA dir = %q", got)
	}

	// 切换到乙位置后重新相对打开：是一份全新、独立的档案，与甲句柄共存。
	pinWorkingDir(t, locB)
	sB, err := Open("./data")
	if err != nil {
		t.Fatalf("relative open at new cwd: %v", err)
	}
	t.Cleanup(func() { _ = sB.Close() })
	if got := sB.dir; got != filepath.Join(locB, "data") {
		t.Fatalf("sB dir = %q, want %q", got, filepath.Join(locB, "data"))
	}
	if _, err := sB.GetPatient(doc, patientA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("new relative store must not see A's patient: %v", err)
	}
	patientB, err := sB.RegisterPatient(doc, "相对重解析患者乙")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sA.GetPatient(doc, patientB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A store must not see B's patient: %v", err)
	}
}

// TestOldHandleSaveFailureNotRedirectedToNewWorkingDir：原目录确实无法保存时
// 返回保存错误，已有记录与审计保持原样；即使乙位置存在可写的同名目录，
// 也不能转而写入那里。
func TestOldHandleSaveFailureNotRedirectedToNewWorkingDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}
	root := t.TempDir()
	locA := filepath.Join(root, "loc-a")
	locB := filepath.Join(root, "loc-b")
	if err := os.MkdirAll(locA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(locB, 0o755); err != nil {
		t.Fatal(err)
	}

	// 乙位置先备好可写的同名目录与另一组患者。
	pinWorkingDir(t, locB)
	sBSeed, err := Open("./data")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sBSeed.RegisterPatient(doc, "保存失败时乙位置患者"); err != nil {
		t.Fatal(err)
	}
	dataB := filepath.Join(locB, "data", dataName)
	bBefore, err := os.ReadFile(dataB)
	if err != nil {
		t.Fatal(err)
	}
	if err := sBSeed.Close(); err != nil {
		t.Fatal(err)
	}

	// 甲位置以相对路径打开原句柄并建立基线状态（相对路径才能体现
	// “原目录不可写时不得转而写入新工作位置同名目录”的固定关系）。
	pinWorkingDir(t, locA)
	dirA := filepath.Join(locA, "data")
	sA, err := Open("./data")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sA.Close() })
	patientA, err := sA.RegisterPatient(doc, "保存失败时甲位置患者")
	if err != nil {
		t.Fatal(err)
	}
	encA, err := sA.AddEncounter(doc, patientA.ID, time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := sA.AuditEvents(doc, patientA.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 切换到乙位置后让甲位置原目录变得不可写（目录本身只读，无法新建/改名
	// 数据文件）；乙位置同名目录仍然可写。
	pinWorkingDir(t, locB)
	if err := os.Chmod(dirA, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dirA, 0o755) })

	if _, err := sA.CreateDraft(doc, patientA.ID, encA.ID, Diagnosis, "不应落盘的草稿"); err == nil {
		t.Fatal("save must fail when the original dir is unwritable")
	} else if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrActive) || errors.Is(err, ErrAccessDenied) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrNotFound) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}

	// 原句柄的既有状态保持原样。
	auditAfter, err := sA.AuditEvents(doc, patientA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditAfter) != len(auditBefore) {
		t.Fatalf("audit changed after failed save: before %d, after %d", len(auditBefore), len(auditAfter))
	}
	chart, err := sA.Chart(doc, patientA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chart.Records) != 0 {
		t.Fatalf("failed save left a record: %+v", chart.Records)
	}
	// 失败不能留下半截临时文件。
	if _, statErr := os.Stat(filepath.Join(dirA, dataName+".tmp")); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}
	// 乙位置可写的同名目录不能成为替代写入位置。
	if bAfter, err := os.ReadFile(dataB); err != nil {
		t.Fatalf("B snapshot unreadable: %v", err)
	} else if string(bAfter) != string(bBefore) {
		t.Fatalf("failed save redirected data to writable same-named dir at new cwd")
	}

	// 恢复原目录可写后，同一句柄的保存立即正常成功，且仍落在甲位置。
	if err := os.Chmod(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	draft, err := sA.CreateDraft(doc, patientA.ID, encA.ID, Diagnosis, "恢复后保存的草稿")
	if err != nil {
		t.Fatalf("save after restoring original dir: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(dirA, dataName)); err != nil {
		t.Fatalf("read A snapshot: %v", err)
	} else if !strings.Contains(string(raw), draft.ID) {
		t.Fatal("recovered save did not land in original data dir")
	}
	if bAfter, _ := os.ReadFile(dataB); string(bAfter) != string(bBefore) {
		t.Fatal("B snapshot changed despite pinning")
	}
}
