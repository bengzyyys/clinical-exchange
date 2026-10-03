package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件为“同一数据目录同一时刻只允许一个 Store”的约定提供回归保障。
// 锁本身是平台相关的（Unix flock / Windows LockFileEx），但这些用例不依赖
// 平台：既覆盖同进程内的第二次 Open，也覆盖句柄与标记文件的生命周期。

// TestSecondOpenSameProcessReturnsNilHandleAndInUseError：
// 第一次打开成功后、关闭之前，同进程内第二次打开同一目录必须立即失败，
// 返回非空错误与空句柄，错误需明确说明目录正在使用。
func TestSecondOpenSameProcessReturnsNilHandleAndInUseError(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	t.Cleanup(func() { _ = s1.Close() })

	s2, err := Open(dir)
	if err == nil {
		_ = s2.Close()
		t.Fatal("second concurrent Open must fail")
	}
	if s2 != nil {
		t.Fatalf("rejected Open must return a nil handle, got %+v", s2)
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("error must state the directory is in use, got: %v", err)
	}
}

// TestRejectedOpenDoesNotDisturbHolder：被拒绝的调用不能改变原使用者的占用
// 状态；原 Store 继续登记患者、管理记录、查询历史，已保存内容保持原样，
// 关闭后重开仍能读到完整数据。
func TestRejectedOpenDoesNotDisturbHolder(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)}
	s1, err := Open(dir, WithClock(func() time.Time { return clk.t }))
	if err != nil {
		t.Fatalf("first open: %v", err)
	}

	p, err := s1.RegisterPatient(doc, "占用方患者")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s1.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := s1.CreateDraft(doc, p.ID, enc.ID, Diagnosis, "占用方诊断")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s1.ActivateRecord(doc, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	auditsBefore := len(mustAudit(t, s1, p.ID))

	// 第二次打开被拒绝。
	if s2, err := Open(dir); err == nil {
		_ = s2.Close()
		t.Fatal("second concurrent Open must fail")
	}

	// 原 Store 仍可正常写入新版本与审计。
	if _, err := s1.CorrectRecord(doc, draft.ID, v1.Number, "占用方诊断（更正）", "复核"); err != nil {
		t.Fatalf("holder cannot mutate after rejected open: %v", err)
	}
	// 查询历史照常。
	if chart, err := s1.Chart(doc, p.ID); err != nil || len(chart.Records) != 1 {
		t.Fatalf("holder chart broken: %+v %v", chart, err)
	}
	if got := len(mustAudit(t, s1, p.ID)); got != auditsBefore+1 {
		t.Fatalf("audit count after correction = %d, want %d", got, auditsBefore+1)
	}

	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// 新 Store 重开读到关闭前提交的完整数据。
	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after holder close: %v", err)
	}
	t.Cleanup(func() { _ = s3.Close() })
	chart, err := s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatalf("chart after reopen: %v", err)
	}
	if len(chart.Records) != 1 || chart.Records[0].CurrentVersion == nil {
		t.Fatalf("record missing after reopen: %+v", chart.Records)
	}
	cur := chart.Records[0].CurrentVersion
	if cur.Number != 2 || cur.Content != "占用方诊断（更正）" {
		t.Fatalf("persisted current version wrong after reopen: %+v", cur)
	}
	if len(chart.Records[0].Versions) != 2 {
		t.Fatalf("version chain after reopen = %d versions, want 2", len(chart.Records[0].Versions))
	}
}

// TestDifferentDirectoriesDoNotInterfere：不同数据目录各自可正常打开使用。
func TestDifferentDirectoriesDoNotInterfere(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	sA, err := Open(dirA)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	t.Cleanup(func() { _ = sA.Close() })
	sB, err := Open(dirB)
	if err != nil {
		t.Fatalf("open B while A is held: %v", err)
	}
	t.Cleanup(func() { _ = sB.Close() })

	pA, err := sA.RegisterPatient(doc, "甲目录患者")
	if err != nil {
		t.Fatal(err)
	}
	pB, err := sB.RegisterPatient(doc, "乙目录患者")
	if err != nil {
		t.Fatal(err)
	}
	if pA.ID == pB.ID {
		t.Fatal("patients in distinct directories unexpectedly share an id")
	}
}

// TestStaleLockMarkerAloneDoesNotBlockOpen：目录中残留上一次留下的占用标记
// 文件，但没有任何活动句柄持锁时，不能单凭标记断定仍被占用，应当能打开。
func TestStaleLockMarkerAloneDoesNotBlockOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".clinical.lock"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open with only a stale marker must succeed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenUnreadableDataReleasesDirectory：数据内容无法解析时，Open 返回错误
// 与空句柄，且不能把目录一直占住——调用方修正数据后无需删除任何标记即可
// 再次打开成功。
func TestOpenUnreadableDataReleasesDirectory(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, dataName)
	if err := os.WriteFile(dataPath, []byte("{this is not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open on corrupt data must fail")
	}
	if s != nil {
		t.Fatalf("failed Open must return a nil handle, got %+v", s)
	}

	// 修正数据问题（删除坏文件即得到空库）后立即重开：若失败的 Open 泄漏了
	// 锁，这里会以“already in use”被拒。
	if err := os.Remove(dataPath); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after fixing data must succeed: %v", err)
	}
	if _, err := s2.RegisterPatient(doc, "修复后患者"); err != nil {
		t.Fatalf("holder after recovery cannot write: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestClosedHandleStaysClosedAfterReopen：关闭后的原句柄即使在另一 Store 已
// 重新打开同一目录后，仍返回 ErrClosed，不会重新变得可用。
func TestClosedHandleStaysClosedAfterReopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	if _, err := s1.RegisterPatient(doc, "旧句柄"); !errors.Is(err, ErrClosed) {
		t.Fatalf("old handle err = %v, want ErrClosed", err)
	}
	if err := s1.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("double close err = %v, want ErrClosed", err)
	}
}
