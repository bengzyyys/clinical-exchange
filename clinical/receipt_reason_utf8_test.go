package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖拒绝回执原因的 UTF-8 校验与保存一致性：拒绝原因夹带任何无效
// UTF-8 字节（含不完整多字节字符）时，SubmitReceipt 必须整体返回
// ErrInvalidArgument 和空回执确认，不能靠静默替换（U+FFFD）、截断或跳过
// 坏字节继续登记，也不能让当次内存里保留原字节、重开后却变成替换字符。
// 合法 UTF-8 原因（含中文、换行、引号、反斜杠、首尾空白与用户明确输入的
// 合法 U+FFFD）必须原样保存与比较，当次内部查看与关闭重开后的查看逐字
// 相同。接受回执继续忽略传入原因：即使被忽略的原因夹带坏字节，也不能
// 拒绝接受或改变相同接受回执的重交结果。

// invalidRejectionReasons 是各类无效 UTF-8 拒绝原因：坏序列出现在开头、
// 结尾、中间，夹在合法中文之间，孤立续字节，不完整多字节字符，单个非法
// 字节，overlong 编码与 UTF-16 代理项编码；另有一例在合法 U+FFFD 之后
// 紧跟非法字节——必须因那个坏字节被拒绝。
var invalidRejectionReasons = []struct {
	label  string
	reason string
}{
	{"开头非法字节", "\xff拒绝：内容不完整"},
	{"结尾非法字节", "拒绝：内容不完整\xff"},
	{"中间非法字节", "拒绝\xff：内容不完整"},
	{"中文之间夹坏字节", "记录缺\xfe失，拒绝接收"},
	{"孤立续字节", "拒绝接收\x80"},
	{"结尾不完整多字节字符", "拒绝接收：记录\xe6"},
	{"中间不完整多字节序列", "ab\xe6\x9d拒绝cd"},
	{"中文之后截断三字节序列", "拒绝接收：记录\xe7\xa1"},
	{"最后一个多字节字符没有写完整", "拒收原因\xe4\xb8"},
	{"单个非法字节", "\x80"},
	{"overlong 编码", "拒绝\xc0\x80原因"},
	{"UTF-16 代理项编码", "第一行\n\xed\xa0\x80\n第二行"},
	{"合法替换字符后紧跟非法字节", "拒绝：补充\ufffd\xff"},
}

// validRejectionReasons 是必须逐字原样保存的合法 UTF-8 拒绝原因。
var validRejectionReasons = []struct {
	label  string
	reason string
}{
	{"纯中文", "记录不完整，拒绝接收"},
	{"中文与全角标点", "诊断：内容与摘要不符（复核确认）"},
	{"表情字符", "内容异常 🩸，拒绝接收 😷"},
	{"换行引号反斜杠", "  第一行\n第二行\t\"引号\" 与 C:\\路径  "},
	{"首尾空白保留", "\t\n  拒绝原因保持原样  \r\n "},
	{"用户明确输入的合法替换字符", "拒绝：补充\ufffd（用户明确输入的 U+FFFD）"},
}

// receiptUTF8Fixture 是回执原因 UTF-8 测试的夹具：一名患者、一次就诊、
// 一条已生效诊断记录与一条覆盖它的有效授权。
type receiptUTF8Fixture struct {
	pid  ID
	rid  ID
	auth Authorization
}

func setupReceiptUTF8(t *testing.T, s *Store, clk *fakeClock) receiptUTF8Fixture {
	t.Helper()
	p, err := s.RegisterPatient(doc, "回执原因UTF8患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断：回执UTF8测试内容 I10")
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
	return receiptUTF8Fixture{pid: p.ID, rid: d.ID, auth: a}
}

// TestSubmitReceiptRejectsInvalidUTF8Reason 覆盖各类坏字节拒绝原因：指定
// 接收方以正确摘要登记、其他业务条件均满足时，一律 ErrInvalidArgument 与
// 空回执确认；交换保持提交前状态——待回执、无回执原因与登记时间、原包与
// 摘要原样、不新增回执审计，关闭重开后仍然如此；此前失败不构成已拒绝，
// 随后换用合法原因仍能正常登记这份回执。
func TestSubmitReceiptRejectsInvalidUTF8Reason(t *testing.T) {
	for _, tc := range invalidRejectionReasons {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 6, 4, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			f := setupReceiptUTF8(t, s, clk)
			x, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, "req-rcpt-utf8")
			if err != nil {
				t.Fatal(err)
			}
			before := snapshotExchange(mustGetExchange(t, s, f.pid, x.ID))
			auditBefore := mustAudit(t, s, f.pid)

			// 坏字节原因：ErrInvalidArgument，且确认必须是空值（无交换标识、
			// 状态、结果与登记时间）。
			conf, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("SubmitReceipt(%q) err = %v, want ErrInvalidArgument", tc.reason, err)
			}
			if conf != (ReceiptConfirmation{}) {
				t.Fatalf("rejected receipt returned non-empty confirmation: %+v", conf)
			}

			// 同样的坏字节原因再交一次，仍是参数错误，而不是“原因不同”的冲突。
			if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("repeat invalid-utf8 reason err = %v, want ErrInvalidArgument", err)
			}

			// 当次内部视图：交换保持提交前状态——待回执、没有回执、原包与摘要
			// 原样，审计不新增。
			assertExchangeMatches(t, mustGetExchange(t, s, f.pid, x.ID), before, "exchange after rejected receipt")
			if c := receiptAuditCount(t, s, f.pid); c != 0 {
				t.Fatalf("rejected receipt left %d receipt audit events", c)
			}
			if audit, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after rejected receipt: %v %+v", err, audit)
			}
			if d, err := s.FetchPackage(rcv, x.ID); err != nil || d.Status != ExchangePending || d.Digest != x.Digest {
				t.Fatalf("delivery changed after rejected receipt: %+v err=%v", d, err)
			}

			// 坏字节没有落盘：关闭重开后仍是同一份待回执交换，审计不变。
			s = reopenStore(t, s, dir, clk)
			assertExchangeMatches(t, mustGetExchange(t, s, f.pid, x.ID), before, "exchange after reopen")
			if audit, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after reopen: %v", err)
			}

			// 此前失败不是已拒绝：换用合法原因仍能完成这份尚未登记的回执。
			clk.t = clk.t.Add(time.Hour)
			registeredAt := clk.t
			legalReason := "换用合法原因后的拒绝说明"
			conf2, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, legalReason)
			if err != nil {
				t.Fatalf("legal receipt after rejected attempt: %v", err)
			}
			if conf2.ExchangeID != x.ID || conf2.Status != ExchangeRejected ||
				conf2.Outcome != ReceiptRejected || !conf2.RegisteredAt.Equal(registeredAt) {
				t.Fatalf("unexpected confirmation after legal reason: %+v", conf2)
			}
			got := mustGetExchange(t, s, f.pid, x.ID)
			if got.Status != ExchangeRejected || got.Receipt == nil ||
				got.Receipt.Outcome != ReceiptRejected || got.Receipt.Reason != legalReason ||
				!got.Receipt.RegisteredAt.Equal(registeredAt) {
				t.Fatalf("legal receipt not stored as expected: %+v", got.Receipt)
			}
			// 失败尝试不产生审计：整份交换只有成功登记这一条回执事件。
			events := mustAudit(t, s, f.pid)
			var rcpt []AuditEvent
			for _, ev := range events {
				if ev.Action == ActionReceipted && ev.ObjectID == x.ID {
					rcpt = append(rcpt, ev)
				}
			}
			if len(rcpt) != 1 || !rcpt[0].OccurredAt.Equal(registeredAt) || rcpt[0].ActorID != rcv.ID {
				t.Fatalf("receipt audit events = %+v, want one at %v", rcpt, registeredAt)
			}
			_ = s.Close()
		})
	}

	// 空串与全空白原因继续按既有规则拒绝（与是否含坏字节无关）。
	s, clk := newTestStore(t)
	f := setupReceiptUTF8(t, s, clk)
	x, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, "req-rcpt-blank")
	if err != nil {
		t.Fatal(err)
	}
	for _, blank := range []string{"", " ", "\n\t\r ", "　"} {
		if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, blank); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("blank rejection reason %q err = %v, want ErrInvalidArgument", blank, err)
		}
	}
	got := mustGetExchange(t, s, f.pid, x.ID)
	if got.Status != ExchangePending || got.Receipt != nil {
		t.Fatalf("blank reasons left a receipt: %+v", got)
	}
}

// TestSubmitReceiptValidUTF8ReasonPreservedRoundTrip 覆盖合法拒绝原因：原样
// 保存与比较，不去首尾空白、不改写字符形态；内部使用者当次查看与关闭重开
// 后的查看得到相同原因；相同结果与原样原因重交返回首次确认及首次登记时间、
// 不新增审计；换成另一份合法原因返回 ErrConflict。
func TestSubmitReceiptValidUTF8ReasonPreservedRoundTrip(t *testing.T) {
	for _, tc := range validRejectionReasons {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			f := setupReceiptUTF8(t, s, clk)
			x, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, "req-rcpt-valid")
			if err != nil {
				t.Fatal(err)
			}

			conf, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason)
			if err != nil {
				t.Fatalf("SubmitReceipt(%q): %v", tc.reason, err)
			}
			if conf.Status != ExchangeRejected || conf.Outcome != ReceiptRejected {
				t.Fatalf("unexpected confirmation: %+v", conf)
			}

			// 当次内部查看：原因逐字保留。
			got := mustGetExchange(t, s, f.pid, x.ID)
			if got.Receipt == nil || got.Receipt.Reason != tc.reason ||
				!got.Receipt.RegisteredAt.Equal(conf.RegisteredAt) {
				t.Fatalf("returned/stored reason mismatch: %+v", got.Receipt)
			}

			// 相同结果与原样原因重交：首次确认与首次登记时间，不新增审计。
			clk.t = clk.t.Add(2 * time.Hour)
			again, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason)
			if err != nil {
				t.Fatalf("identical resubmit: %v", err)
			}
			if again.ExchangeID != conf.ExchangeID || again.Status != conf.Status ||
				again.Outcome != conf.Outcome || !again.RegisteredAt.Equal(conf.RegisteredAt) {
				t.Fatalf("resubmit changed confirmation:\n got=%+v\nwant=%+v (resubmit time must not be used)",
					again, conf)
			}
			if c := receiptAuditCount(t, s, f.pid); c != 1 {
				t.Fatalf("identical resubmit changed receipt audit count: %d", c)
			}

			// 换成另一份合法原因：ErrConflict，不改写已保存原因或登记时间。
			otherReason := "另一段完全不同但合法的拒绝原因"
			if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, otherReason); !errors.Is(err, ErrConflict) {
				t.Fatalf("different-reason resubmit err = %v, want ErrConflict", err)
			}
			got = mustGetExchange(t, s, f.pid, x.ID)
			if got.Receipt.Reason != tc.reason || !got.Receipt.RegisteredAt.Equal(conf.RegisteredAt) {
				t.Fatalf("conflicting resubmit rewrote receipt: %+v", got.Receipt)
			}
			if c := receiptAuditCount(t, s, f.pid); c != 1 {
				t.Fatalf("conflicting resubmit changed receipt audit count: %d", c)
			}

			// 关闭重开：内部查看得到逐字相同原因与首次登记时间；原样原因重交
			// 仍返回同一份确认。
			s = reopenStore(t, s, dir, clk)
			t.Cleanup(func() { _ = s.Close() })
			got = mustGetExchange(t, s, f.pid, x.ID)
			if got.Status != ExchangeRejected || got.Receipt == nil ||
				got.Receipt.Outcome != ReceiptRejected || got.Receipt.Reason != tc.reason ||
				!got.Receipt.RegisteredAt.Equal(conf.RegisteredAt) {
				t.Fatalf("reason changed across reopen:\n got=%+v\nwant reason %q at %v",
					got.Receipt, tc.reason, conf.RegisteredAt)
			}
			again2, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason)
			if err != nil {
				t.Fatalf("identical resubmit after reopen: %v", err)
			}
			if again2.ExchangeID != conf.ExchangeID || !again2.RegisteredAt.Equal(conf.RegisteredAt) {
				t.Fatalf("resubmit after reopen changed confirmation: got %+v, want %+v", again2, conf)
			}
			if c := receiptAuditCount(t, s, f.pid); c != 1 {
				t.Fatalf("resubmit after reopen changed receipt audit count: %d", c)
			}
		})
	}
}

// TestSubmitReceiptExplicitReplacementCharIsLegal 守住“用户明确输入的合法
// U+FFFD 可以保存，它与无效字节不是一回事”：明确含 U+FFFD 的原因登记成功
// 并原样往返；只有对应位置换成坏字节的同形原因必须被 ErrInvalidArgument
// 拒绝，既不能命中已保存的那份回执，也不能在重开后与其变成同一个原因。
func TestSubmitReceiptExplicitReplacementCharIsLegal(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 6, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	f := setupReceiptUTF8(t, s, clk)
	x, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, "req-rcpt-fffd")
	if err != nil {
		t.Fatal(err)
	}

	// 用户明确输入的合法 U+FFFD 原因可以正常登记。
	explicit := "拒绝：补充\ufffd"
	conf, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, explicit)
	if err != nil {
		t.Fatalf("register reason with explicit U+FFFD: %v", err)
	}
	auditBefore := mustAudit(t, s, f.pid)

	// 同形位置换成坏字节的原因：参数错误，而不是“原因相同”的确认或
	// “原因不同”的冲突——坏字节根本不允许进入登记判定。
	bad := "拒绝：补充\xff"
	if c, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad-byte lookalike reason err = %v, confirmation = %+v, want ErrInvalidArgument", err, c)
	}
	got := mustGetExchange(t, s, f.pid, x.ID)
	if got.Receipt == nil || got.Receipt.Reason != explicit {
		t.Fatalf("saved reason changed after rejected lookalike: %+v", got.Receipt)
	}
	if audit, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit changed after rejected lookalike: %v", err)
	}

	// 关闭重开：保存的仍是明确的合法 U+FFFD，原样重交返回首次确认。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	got = mustGetExchange(t, s, f.pid, x.ID)
	if got.Receipt == nil || got.Receipt.Reason != explicit ||
		!got.Receipt.RegisteredAt.Equal(conf.RegisteredAt) {
		t.Fatalf("reason changed across reopen: %+v", got.Receipt)
	}
	again, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, explicit)
	if err != nil {
		t.Fatalf("resubmit explicit U+FFFD after reopen: %v", err)
	}
	if again.ExchangeID != conf.ExchangeID || !again.RegisteredAt.Equal(conf.RegisteredAt) {
		t.Fatalf("resubmit after reopen changed confirmation: got %+v, want %+v", again, conf)
	}
}

// TestSubmitReceiptAcceptedIgnoresInvalidUTF8Reason 覆盖接受回执：传入原因
// 一律被忽略并保存为空，即使原因夹带无效字节，也不能因此拒绝接受或改变
// 相同接受回执的重交结果（返回首次确认、不新增审计、关闭重开后一致）。
func TestSubmitReceiptAcceptedIgnoresInvalidUTF8Reason(t *testing.T) {
	for _, tc := range []struct {
		label  string
		reason string
	}{
		{"坏字节开头", "\xff随手附带的接受文字"},
		{"坏字节结尾", "随手附带的接受文字\xff"},
		{"不完整多字节字符", "随手附带\xe7\xa1"},
		{"单个非法字节", "\x80"},
		{"普通合法文字", "首次登记时随手附带的接受文字"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 6, 7, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			f := setupReceiptUTF8(t, s, clk)
			x, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.rid}, "req-acc-utf8-"+tc.label)
			if err != nil {
				t.Fatal(err)
			}

			conf, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, tc.reason)
			if err != nil {
				t.Fatalf("accept with ignored bad-byte reason: %v", err)
			}
			if conf.Status != ExchangeAccepted || conf.Outcome != ReceiptAccepted || conf.RegisteredAt.IsZero() {
				t.Fatalf("unexpected accept confirmation: %+v", conf)
			}
			got := mustGetExchange(t, s, f.pid, x.ID)
			if got.Status != ExchangeAccepted || got.Receipt == nil ||
				got.Receipt.Outcome != ReceiptAccepted || got.Receipt.Reason != "" {
				t.Fatalf("accepted receipt must store empty reason, got %+v", got.Receipt)
			}

			// 用另一段同样夹带坏字节（字节都不同）的原因重交接受：被忽略，
			// 仍返回首次确认，不新增审计。
			clk.t = clk.t.Add(2 * time.Hour)
			again, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, "重交时另一段坏字节\xfe文字")
			if err != nil {
				t.Fatalf("accept resubmit with another bad-byte reason: %v", err)
			}
			if again.ExchangeID != conf.ExchangeID || !again.RegisteredAt.Equal(conf.RegisteredAt) {
				t.Fatalf("accept resubmit changed confirmation: got %+v, want %+v", again, conf)
			}
			if c := receiptAuditCount(t, s, f.pid); c != 1 {
				t.Fatalf("accept resubmit changed receipt audit count: %d", c)
			}
			got = mustGetExchange(t, s, f.pid, x.ID)
			if got.Receipt.Reason != "" {
				t.Fatalf("ignored reason leaked into saved receipt: %+v", got.Receipt)
			}

			// 关闭重开：接受状态与空原因保持，重交仍返回首次确认。
			s = reopenStore(t, s, dir, clk)
			t.Cleanup(func() { _ = s.Close() })
			got = mustGetExchange(t, s, f.pid, x.ID)
			if got.Status != ExchangeAccepted || got.Receipt == nil || got.Receipt.Reason != "" ||
				!got.Receipt.RegisteredAt.Equal(conf.RegisteredAt) {
				t.Fatalf("accepted receipt changed across reopen: %+v", got.Receipt)
			}
			again2, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, "\xff\xff")
			if err != nil {
				t.Fatalf("accept resubmit after reopen: %v", err)
			}
			if again2.ExchangeID != conf.ExchangeID || !again2.RegisteredAt.Equal(conf.RegisteredAt) {
				t.Fatalf("accept resubmit after reopen changed confirmation: got %+v, want %+v", again2, conf)
			}
			if c := receiptAuditCount(t, s, f.pid); c != 1 {
				t.Fatalf("resubmit after reopen changed receipt audit count: %d", c)
			}
		})
	}
}
