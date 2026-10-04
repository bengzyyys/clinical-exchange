package clinical

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件为数据目录独占使用补回归保障，Unix（flock）与 Windows
//（LockFileEx）下同样适用：
//
//   - 同一进程内第二次打开同一目录立即失败，返回非空错误与空句柄；
//   - 另一个独立进程持有同一目录时，本进程打开同样立即失败，而不是等待
//     对方关闭；
//   - 被拒绝的调用不影响原使用者：登记患者、草稿/生效/更正、查询历史与
//     审计全部照常，已提交内容保持原样，占用状态也不改变；
//   - 不同目录互不影响；
//   - Close 后同位置可重新打开并读到完整数据；目录里残留的锁标记文件
//     本身不代表仍有人占用；
//   - 数据无法读取或格式不受支持时，Open 返回错误和空句柄并释放占用，
//     调用方修正数据后可再次打开。

const (
	holderModeEnv = "CLINICAL_LOCK_TEST_HOLDER"
	holderDirEnv  = "CLINICAL_LOCK_TEST_DIR"
)

// alreadyInUseMsg 是两种平台实现共用的占用错误片段。
const alreadyInUseMsg = "already in use"

// TestMain 让测试二进制可以作为“独立持锁进程”运行：父进程启动自身副本，
// 子进程 Open 指定目录并持有，直到父进程关闭其标准输入才 Close 退出。
func TestMain(m *testing.M) {
	if os.Getenv(holderModeEnv) == "1" {
		os.Exit(runHolderProcess())
	}
	os.Exit(m.Run())
}

func runHolderProcess() int {
	dir := os.Getenv(holderDirEnv)
	out := bufio.NewWriter(os.Stdout)
	s, err := Open(dir)
	if err != nil {
		fmt.Fprintf(out, "ERROR: %v\n", err)
		_ = out.Flush()
		return 1
	}
	fmt.Fprintln(out, "READY")
	if err := out.Flush(); err != nil {
		_ = s.Close()
		return 1
	}
	// 阻塞到父进程关闭标准输入，期间一直持有目录占用。
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := s.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "holder close: %v\n", err)
		return 1
	}
	return 0
}

// holder 是一个持有数据目录占用的独立进程。
type holder struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	readyCh chan string
}

func spawnHolder(t *testing.T, dir string) *holder {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	cmd := exec.Command(exe, "-test.run", "^TestMain$")
	cmd.Env = append(os.Environ(), holderModeEnv+"=1", holderDirEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("holder stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("holder stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holder process: %v", err)
	}
	h := &holder{cmd: cmd, stdin: stdin, readyCh: make(chan string, 1)}
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			h.readyCh <- scanner.Text()
		} else {
			h.readyCh <- ""
		}
	}()
	t.Cleanup(func() { h.release(t) })
	return h
}

// waitReady 等待子进程完成 Open；返回其上报的第一行（READY 或 ERROR: ...）。
func (h *holder) waitReady(t *testing.T) string {
	t.Helper()
	select {
	case line := <-h.readyCh:
		return line
	case <-time.After(15 * time.Second):
		t.Fatal("holder process did not become ready in time")
		return ""
	}
}

// release 让子进程关闭 Store 并等待其退出。
func (h *holder) release(t *testing.T) {
	t.Helper()
	if h.stdin != nil {
		_ = h.stdin.Close()
	}
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case err := <-done:
		h.stdin = nil
		// 子进程可能已被前一次 release 回收；只对仍在运行时的非零退出报错。
		if err != nil && h.cmd.ProcessState != nil && h.cmd.ProcessState.ExitCode() != 0 {
			t.Errorf("holder process exited with error: %v", err)
		}
	case <-time.After(15 * time.Second):
		_ = h.cmd.Process.Kill()
		t.Fatal("holder process did not exit after release")
	}
}

// ---- 同进程内：第二次打开立即失败、返回空句柄，且不影响原使用者 ----

func TestSecondOpenSameProcessRejectedAndHolderUnaffected(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	t.Cleanup(func() { _ = s1.Close() })

	reject := func(label string) {
		t.Helper()
		s2, err := Open(dir)
		if err == nil {
			_ = s2.Close()
			t.Fatalf("%s: second Open must fail", label)
		}
		if s2 != nil {
			t.Fatalf("%s: rejected Open must return nil handle, got %#v", label, s2)
		}
		if !strings.Contains(err.Error(), alreadyInUseMsg) {
			t.Fatalf("%s: error does not say directory is in use: %v", label, err)
		}
	}

	reject("first rejection")

	// 原 Store 继续登记患者、管理记录，且数据真正落盘。
	p, err := s1.RegisterPatient(doc, "锁互斥患者")
	if err != nil {
		t.Fatalf("register after rejected open: %v", err)
	}
	enc, err := s1.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatalf("add encounter after rejected open: %v", err)
	}
	draft, err := s1.CreateDraft(doc, p.ID, enc.ID, Diagnosis, "初版诊断")
	if err != nil {
		t.Fatalf("create draft after rejected open: %v", err)
	}
	v1, err := s1.ActivateRecord(doc, draft.ID)
	if err != nil {
		t.Fatalf("activate after rejected open: %v", err)
	}
	v2, err := s1.CorrectRecord(doc, draft.ID, v1.Number, "复核后的诊断", "复核补录")
	if err != nil {
		t.Fatalf("correct after rejected open: %v", err)
	}

	// 被拒绝的调用不能改变占用状态：第三次打开仍应立即失败。
	reject("second rejection")

	// 查询历史、版本链与审计在原 Store 上完整可用。
	hist, err := s1.EncounterRecords(doc, p.ID, enc.ID)
	if err != nil {
		t.Fatalf("query history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history len = %d, want 1", len(hist))
	}
	rh := hist[0]
	if rh.CurrentVersion == nil || rh.CurrentVersion.ID != v2.ID || len(rh.Versions) != 2 {
		t.Fatalf("version chain broken after rejected open: %+v", rh)
	}
	acts := auditActions(mustAudit(t, s1, p.ID))
	if acts[ActionActivated] != 1 || acts[ActionCorrected] != 1 {
		t.Fatalf("audit events changed by rejected open: %+v", acts)
	}

	// 关闭后再开，已提交的患者、版本与审计原样保留。
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	t.Cleanup(func() { _ = s3.Close() })
	got, err := s3.GetPatient(doc, p.ID)
	if err != nil {
		t.Fatalf("patient lost across reopen: %v", err)
	}
	if got.Name != "锁互斥患者" || got.Source != SyntheticSource {
		t.Fatalf("patient content changed across reopen: %+v", got)
	}
	reopened, err := s3.EncounterRecords(doc, p.ID, enc.ID)
	if err != nil {
		t.Fatalf("history after reopen: %v", err)
	}
	if len(reopened) != 1 || reopened[0].CurrentVersion == nil ||
		reopened[0].CurrentVersion.ID != v2.ID || len(reopened[0].Versions) != 2 {
		t.Fatalf("version history not preserved across reopen: %+v", reopened)
	}
	acts2 := auditActions(mustAudit(t, s3, p.ID))
	if acts2[ActionActivated] != 1 || acts2[ActionCorrected] != 1 {
		t.Fatalf("audit not preserved across reopen: %+v", acts2)
	}
}

// ---- 跨进程：另一个独立进程持有时，Open 必须立即失败而非等待其关闭 ----

func TestOpenRejectedByIndependentProcess(t *testing.T) {
	dir := t.TempDir()

	// 先由本进程写入一批已提交数据，再交给独立进程重新打开持有。
	s0, err := Open(dir)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	p, err := s0.RegisterPatient(doc, "跨进程患者")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s0.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := s0.CreateDraft(doc, p.ID, enc.ID, Order, "跨进程医嘱")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s0.ActivateRecord(doc, draft.ID); err != nil {
		t.Fatal(err)
	}
	if err := s0.Close(); err != nil {
		t.Fatal(err)
	}

	h := spawnHolder(t, dir)
	if line := h.waitReady(t); line != "READY" {
		t.Fatalf("holder did not open directory, reported: %q", line)
	}

	// 子进程持有期间：Open 必须立即失败（子进程只有在本测试后面的
	// release 时才会关闭），错误明确说明目录正在使用，句柄为空。
	start := time.Now()
	s1, err := Open(dir)
	waited := time.Since(start)
	if err == nil {
		_ = s1.Close()
		t.Fatal("Open against directory held by another process must fail")
	}
	if s1 != nil {
		t.Fatalf("rejected cross-process Open must return nil handle, got %#v", s1)
	}
	if !strings.Contains(err.Error(), alreadyInUseMsg) {
		t.Fatalf("cross-process error does not say directory is in use: %v", err)
	}
	if waited > 5*time.Second {
		t.Fatalf("Open waited %s for holder to close; must fail immediately", waited)
	}

	// 再次尝试仍立即失败，证明被拒调用没有干扰任何一方的占用。
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), alreadyInUseMsg) {
		t.Fatalf("second cross-process Open not rejected: %v", err)
	}

	// 持有者关闭后，同一目录可正常重开，先前提交的数据完整保留。
	h.release(t)
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after holder exit: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if got, err := s2.GetPatient(doc, p.ID); err != nil || got.Name != "跨进程患者" {
		t.Fatalf("committed patient not preserved while another process held dir: %+v, %v", got, err)
	}
	hist, err := s2.EncounterRecords(doc, p.ID, enc.ID)
	if err != nil {
		t.Fatalf("history after holder exit: %v", err)
	}
	if len(hist) != 1 || hist[0].CurrentVersion == nil || len(hist[0].Versions) != 1 {
		t.Fatalf("committed version history not preserved: %+v", hist)
	}
	if acts := auditActions(mustAudit(t, s2, p.ID)); acts[ActionActivated] != 1 {
		t.Fatalf("committed audit not preserved: %+v", acts)
	}
}

// ---- 不同目录互不影响（含与持锁进程并存） ----

func TestDifferentDirectoriesDoNotInterfere(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	sA, err := Open(dirA)
	if err != nil {
		t.Fatalf("open dir A: %v", err)
	}
	t.Cleanup(func() { _ = sA.Close() })
	sB, err := Open(dirB)
	if err != nil {
		t.Fatalf("open dir B: %v", err)
	}
	t.Cleanup(func() { _ = sB.Close() })

	pA, err := sA.RegisterPatient(doc, "目录甲患者")
	if err != nil {
		t.Fatal(err)
	}
	pB, err := sB.RegisterPatient(doc, "目录乙患者")
	if err != nil {
		t.Fatal(err)
	}

	// 独立进程持有第三个目录，不影响 A、B 的使用。
	h := spawnHolder(t, t.TempDir())
	if line := h.waitReady(t); line != "READY" {
		t.Fatalf("holder reported: %q", line)
	}

	if got, err := sA.GetPatient(doc, pA.ID); err != nil || got.Name != "目录甲患者" {
		t.Fatalf("dir A affected by other users: %+v, %v", got, err)
	}
	if got, err := sB.GetPatient(doc, pB.ID); err != nil || got.Name != "目录乙患者" {
		t.Fatalf("dir B affected by other users: %+v, %v", got, err)
	}
	if _, err := Open(dirA); err == nil || !strings.Contains(err.Error(), alreadyInUseMsg) {
		t.Fatalf("dir A lock lost: %v", err)
	}
	if _, err := Open(dirB); err == nil || !strings.Contains(err.Error(), alreadyInUseMsg) {
		t.Fatalf("dir B lock lost: %v", err)
	}
}

// ---- 残留的锁标记文件不代表占用：无需手动删除即可重开 ----

func TestStaleLockMarkerDoesNotBlockReopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s1.RegisterPatient(doc, "残留标记患者")
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// 模拟上次使用（或异常退出）留下的占用标记文件：文件在，但没有任何
	// 存活句柄持锁。单凭该文件不得判定目录仍被占用。
	marker := filepath.Join(dir, ".clinical.lock")
	if err := os.WriteFile(marker, []byte("stale marker from previous use\n"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("stale marker must not block reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if got, err := s2.GetPatient(doc, p.ID); err != nil || got.Name != "残留标记患者" {
		t.Fatalf("data not readable over stale marker: %+v, %v", got, err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	// 连续多轮开关都应正常，且锁标记再次残留也不影响。
	for i := 0; i < 3; i++ {
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen %d over residual marker failed: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

// ---- 打开失败时释放占用：数据损坏/格式不受支持后，修正数据可重开 ----

func TestOpenFailureReleasesDirectory(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content []byte
		setup   func(string) error
	}{
		{
			name:    "unreadable-json",
			content: []byte("{this is not valid json"),
		},
		{
			name:    "unsupported-format-version",
			content: []byte(fmt.Sprintf(`{"format_version":%d}`, dataVersion+99)),
		},
		{
			name: "data-path-is-directory",
			setup: func(dir string) error {
				// 快照路径存在且是目录时无法作为文件读取。
				return os.Mkdir(filepath.Join(dir, dataName), 0o755)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每个子场景从干净目录开始（锁标记文件残留无害，保留它还能
			// 顺带验证残留标记不影响数据损坏时的失败路径）。
			if err := os.RemoveAll(filepath.Join(dir, dataName)); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				if err := tc.setup(dir); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(dir, dataName), tc.content, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			s, err := Open(dir)
			if err == nil {
				_ = s.Close()
				t.Fatal("Open on unusable data must fail")
			}
			if s != nil {
				t.Fatalf("failed Open must return nil handle, got %#v", s)
			}
			if errors.Is(err, ErrClosed) {
				t.Fatalf("data error must not be masked as ErrClosed: %v", err)
			}

			// 失败的 Open 不得把目录一直占住：另一次打开仍会到达数据校验
			// 阶段并因同样的数据问题失败，而不是返回“目录正在使用”。
			if _, err2 := Open(dir); err2 == nil {
				t.Fatal("second Open must still fail on the bad data")
			} else if strings.Contains(err2.Error(), alreadyInUseMsg) {
				t.Fatalf("failed Open left the directory locked: %v", err2)
			}

			// 调用方修正数据问题后再次打开应成功。
			if err := os.RemoveAll(filepath.Join(dir, dataName)); err != nil {
				t.Fatal(err)
			}
			s2, err := Open(dir)
			if err != nil {
				t.Fatalf("reopen after fixing data: %v", err)
			}
			if p, err := s2.RegisterPatient(doc, "修复后患者"); err != nil {
				t.Fatalf("store unusable after fixing data: %v", err)
			} else if _, err := s2.GetPatient(doc, p.ID); err != nil {
				t.Fatalf("fixed store cannot read back: %v", err)
			}
			if err := s2.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// ---- 关闭与接手交错：旧 Store 余下的关闭动作不能破坏新 Store 的独占 ----

func TestCloseOpenInterleavingKeepsExclusivity(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s1.RegisterPatient(doc, "交接患者")
	if err != nil {
		t.Fatal(err)
	}

	// 还原关闭与接手的最坏交错：s1 的 Close 先释放文件锁（尚未走完全部
	// 关闭动作），此刻 s2 合法接手同一目录；随后 s1 才完成余下的关闭动作。
	// s1 余下的动作（关闭句柄、清理锁标记）绝不能打断 s2 的独占。
	unlockFile(s1.lock)
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("takeover during predecessor close must succeed: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if err := s1.Close(); err != nil {
		t.Fatalf("predecessor finishing close: %v", err)
	}

	// s2 占用期间，任何第三个调用方（本进程内）都必须立即被拒绝。
	s3, err := Open(dir)
	if err == nil {
		_ = s3.Close()
		t.Fatal("third Open must be rejected while successor holds the directory")
	}
	if s3 != nil {
		t.Fatalf("rejected Open must return nil handle, got %#v", s3)
	}
	if !strings.Contains(err.Error(), alreadyInUseMsg) {
		t.Fatalf("error does not say directory is in use: %v", err)
	}

	// 旧 Store 保持已关闭语义，不因交接复活；再次关闭也不影响新 Store。
	if _, err := s1.RegisterPatient(doc, "旧句柄患者"); !errors.Is(err, ErrClosed) {
		t.Fatalf("old handle method err = %v, want ErrClosed", err)
	}
	if err := s1.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("old handle second Close err = %v, want ErrClosed", err)
	}

	// 新 Store 读到接手前已提交的数据，并能继续正常保存。
	if got, err := s2.GetPatient(doc, p.ID); err != nil || got.Name != "交接患者" {
		t.Fatalf("successor cannot read committed data: %+v, %v", got, err)
	}
	if _, err := s2.RegisterPatient(doc, "接手后患者"); err != nil {
		t.Fatalf("successor cannot save after interleaved close: %v", err)
	}

	// 新 Store 正常关闭后目录释放，之后可再次打开。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s4, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after successor close: %v", err)
	}
	t.Cleanup(func() { _ = s4.Close() })
	if got, err := s4.GetPatient(doc, p.ID); err != nil || got.Name != "交接患者" {
		t.Fatalf("data lost across handoff: %+v, %v", got, err)
	}
}

// ---- 关闭后的旧句柄不因新 Store 打开同一目录而复活 ----

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

	if _, err := s1.RegisterPatient(doc, "旧句柄患者"); !errors.Is(err, ErrClosed) {
		t.Fatalf("old handle method err = %v, want ErrClosed", err)
	}
	if err := s1.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("old handle Close err = %v, want ErrClosed", err)
	}

	// 新句柄正常工作，证明目录占用已随 Close 转移，而非旧句柄复活。
	if _, err := s2.RegisterPatient(doc, "新句柄患者"); err != nil {
		t.Fatalf("new store should work: %v", err)
	}
}
