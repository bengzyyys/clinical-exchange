package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖创建交换时请求号的 UTF-8 校验与保存一致性：请求号夹带任何
// 无效 UTF-8 字节（含不完整多字节字符）时，CreateExchange 必须整体返回
// ErrInvalidArgument，不能靠静默替换（U+FFFD）、截断或跳过坏字节继续创建，
// 也不能把处理后的字符串当作另一个已保存请求号来返回旧交换。合法 UTF-8
// 请求号（含中文、表情、换行、首尾空白与用户明确输入的合法 U+FFFD）必须
// 原样保存与比较，关闭后从同一位置重新打开看到的请求号逐字相同，重复提交
// 仍命中同一份交换。

// invalidRequestIDs 是各类无效 UTF-8 请求号：坏序列出现在开头、结尾、中间，
// 孤立续字节，不完整多字节字符，overlong 编码与 UTF-16 代理项编码；另有一例
// 在合法 U+FFFD 之后紧跟非法字节——必须因那个坏字节被拒绝。
var invalidRequestIDs = []struct {
	label     string
	requestID string
}{
	{"开头非法字节", "\xffreq-1"},
	{"结尾非法字节", "req-1\xff"},
	{"中间非法字节", "req\xff-1"},
	{"中文之间夹坏字节", "请求\xfe-1"},
	{"孤立续字节", "req-\x80"},
	{"结尾不完整多字节字符", "请求-\xe6"},
	{"中间不完整多字节序列", "ab\xe6\x9dcd"},
	{"中文之后截断三字节序列", "请求号\xe7\xa1"},
	{"单个非法字节", "\x80"},
	{"overlong 编码", "\xc0\x80"},
	{"UTF-16 代理项编码", "请求\n\xed\xa0\x80\n"},
	{"合法替换字符后紧跟非法字节", "请求�\xff"},
}

// validRequestIDs 是必须逐字原样保存的合法 UTF-8 请求号。
var validRequestIDs = []struct {
	label     string
	requestID string
}{
	{"纯英文数字", "req-0001"},
	{"中文请求号", "请求-甲-0001"},
	{"表情字符", "req-🩸-😷"},
	{"组合字符", "réq-1"},
	{"含换行制表", "req\n第二行\t-1"},
	{"首尾空白保留", "  req-保留空白  "},
	{"用户明确输入的合法替换字符", "请求-�"},
}

// exchangeUTF8Fixture 是请求号 UTF-8 测试的夹具：一名患者、一次就诊、
// 一条已生效诊断记录与一条覆盖它的有效授权。
type exchangeUTF8Fixture struct {
	pid  ID
	rid  ID
	auth Authorization
}

func setupExchangeUTF8(t *testing.T, s *Store, clk *fakeClock) exchangeUTF8Fixture {
	t.Helper()
	p, err := s.RegisterPatient(doc, "请求号UTF8患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断：高血压 I10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, d.ID); err != nil {
		t.Fatal(err)
	}
	a, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return exchangeUTF8Fixture{pid: p.ID, rid: d.ID, auth: a}
}

func exchangedAuditCount(events []AuditEvent) int {
	n := 0
	for _, e := range events {
		if e.Action == ActionExchanged {
			n++
		}
	}
	return n
}

// TestCreateExchangeRejectsInvalidUTF8RequestID 覆盖各类坏字节请求号：
// 一律 ErrInvalidArgument，不返回交换标识，不新增交换或交换创建审计，
// 不占用请求号；关闭重开后交换清单与审计仍是拒绝前的结果。
func TestCreateExchangeRejectsInvalidUTF8RequestID(t *testing.T) {
	for _, tc := range invalidRequestIDs {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			f := setupExchangeUTF8(t, s, clk)
			auditBefore := mustAudit(t, s, f.pid)

			x, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, tc.requestID)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("CreateExchange(%q) err = %v, want ErrInvalidArgument", tc.requestID, err)
			}
			if x.ID != "" {
				t.Fatalf("rejected create returned exchange id %q", x.ID)
			}
			// 拒绝不能留下交换或交换创建审计。
			xs, err := s.ListExchanges(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if len(xs) != 0 {
				t.Fatalf("rejected create left exchanges: %+v", xs)
			}
			audit, err := s.AuditEvents(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after rejected create")
			}

			// 坏字节没有落盘：关闭重开后交换清单与审计仍是拒绝前的结果。
			s = reopenStore(t, s, dir, clk)
			xs, err = s.ListExchanges(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if len(xs) != 0 {
				t.Fatalf("rejected create persisted after reopen: %+v", xs)
			}
			audit, err = s.AuditEvents(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after reopen")
			}

			// 被拒绝的请求号不被占用：坏字节不会被替换成某个合法请求号去
			// 命中或占用它——随后用对应的合法形式可以正常创建。
			clean := "req-clean-after-reject"
			if _, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, clean); err != nil {
				t.Fatalf("create after rejection: %v", err)
			}
			_ = s.Close()
		})
	}

	// 空串与全空白请求号继续按既有规则拒绝。
	s, clk := newTestStore(t)
	f := setupExchangeUTF8(t, s, clk)
	for _, blank := range []string{"", " ", "\n\t\r ", "　"} {
		if _, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, blank); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("CreateExchange blank request id %q err = %v, want ErrInvalidArgument", blank, err)
		}
	}
	xs, err := s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 0 {
		t.Fatalf("blank request ids left exchanges: %+v", xs)
	}
}

// TestCreateExchangeValidUTF8RequestIDPreservedRoundTrip 覆盖合法请求号：
// 原样保存与比较，不统一字符形态、不去首尾空白；当次与关闭重开后看到的
// 请求号逐字相同，同一使用者以相同请求号与相同参数重试仍返回原交换。
func TestCreateExchangeValidUTF8RequestIDPreservedRoundTrip(t *testing.T) {
	for _, tc := range validRequestIDs {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			f := setupExchangeUTF8(t, s, clk)

			x, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, tc.requestID)
			if err != nil {
				t.Fatalf("CreateExchange(%q): %v", tc.requestID, err)
			}
			if x.RequestID != tc.requestID {
				t.Fatalf("returned request id = %q, want %q", x.RequestID, tc.requestID)
			}

			// 相同请求号与相同参数重试：返回原交换，不新增交换或审计。
			auditBefore := mustAudit(t, s, f.pid)
			again, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, tc.requestID)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if again.ID != x.ID || again.RequestID != tc.requestID {
				t.Fatalf("retry returned %+v, want exchange %q", again, x.ID)
			}
			if got := exchangedAuditCount(mustAudit(t, s, f.pid)); got != exchangedAuditCount(auditBefore) {
				t.Fatalf("retry added an exchange audit event")
			}

			// 关闭后从同一位置重新打开：请求号逐字保持，重试仍命中同一份。
			s = reopenStore(t, s, dir, clk)
			t.Cleanup(func() { _ = s.Close() })
			xs, err := s.ListExchanges(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if len(xs) != 1 || xs[0].RequestID != tc.requestID {
				t.Fatalf("exchanges after reopen = %+v, want one with request id %q", xs, tc.requestID)
			}
			again, err = s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, tc.requestID)
			if err != nil {
				t.Fatalf("retry after reopen: %v", err)
			}
			if again.ID != x.ID {
				t.Fatalf("retry after reopen returned exchange %q, want %q", again.ID, x.ID)
			}
			xs, err = s.ListExchanges(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if len(xs) != 1 {
				t.Fatalf("retry after reopen created another exchange: %+v", xs)
			}
		})
	}
}

// TestCreateExchangeInvalidRequestIDDoesNotHitReplacementCharExchange 覆盖
// 撞号场景：已存在明确包含合法 U+FFFD 的请求号时，夹带非法字节的请求号
// 不能被替换后命中那份旧交换，也不能另建一份让两者重开后变成同一个请求号。
func TestCreateExchangeInvalidRequestIDDoesNotHitReplacementCharExchange(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	f := setupExchangeUTF8(t, s, clk)

	// 用户明确输入合法 U+FFFD 的请求号可以正常创建。
	explicit := "请求-�"
	x1, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, explicit)
	if err != nil {
		t.Fatalf("create with explicit U+FFFD: %v", err)
	}

	// 夹带非法字节的请求号（替换后会变成 explicit）必须被拒绝，而不是
	// 命中或另建：返回 ErrInvalidArgument，交换清单与审计不变。
	auditBefore := mustAudit(t, s, f.pid)
	if _, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, "请求-\xff"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid utf8 request id err = %v, want ErrInvalidArgument", err)
	}
	xs, err := s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].ID != x1.ID || xs[0].RequestID != explicit {
		t.Fatalf("exchanges after rejected create = %+v, want only %q", xs, explicit)
	}
	audit, err := s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit changed after rejected create")
	}

	// 关闭重开后仍只有那一份明确含 U+FFFD 的交换，请求号逐字相同；
	// 用明确输入的合法请求号重试仍稳定返回它。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	xs, err = s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].ID != x1.ID || xs[0].RequestID != explicit {
		t.Fatalf("exchanges after reopen = %+v, want only %q", xs, explicit)
	}
	again, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, explicit)
	if err != nil {
		t.Fatalf("retry explicit U+FFFD after reopen: %v", err)
	}
	if again.ID != x1.ID {
		t.Fatalf("retry after reopen returned exchange %q, want %q", again.ID, x1.ID)
	}
}

// TestCreateExchangeRequestIDRetrySemanticsPreserved 保证合法请求号的既有
// 重复提交行为不变：相同参数重试返回原交换，参数变化返回 ErrConflict，
// 不同内部使用者的请求号互不占用。
func TestCreateExchangeRequestIDRetrySemanticsPreserved(t *testing.T) {
	s, clk := newTestStore(t)
	f := setupExchangeUTF8(t, s, clk)

	x, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, "req-语义")
	if err != nil {
		t.Fatal(err)
	}

	// 相同请求号 + 相同参数（记录集合排列变化不算参数变化）：返回原交换。
	again, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid, f.rid}, "req-语义")
	if err != nil {
		t.Fatalf("same-params retry: %v", err)
	}
	if again.ID != x.ID {
		t.Fatalf("same-params retry returned %q, want %q", again.ID, x.ID)
	}

	// 相同请求号改用其他参数：ErrConflict。
	if _, err := s.CreateExchange(doc, f.pid, rcvB.ID, f.auth.ID, []ID{f.rid}, "req-语义"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different-params retry err = %v, want ErrConflict", err)
	}

	// 不同内部使用者的同一请求号互不占用。
	other, err := s.CreateExchange(doc2, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, "req-语义")
	if err != nil {
		t.Fatalf("other creator with same request id: %v", err)
	}
	if other.ID == x.ID {
		t.Fatalf("different creators must not share request id %q", "req-语义")
	}
}
