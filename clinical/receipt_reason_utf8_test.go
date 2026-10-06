package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖拒绝回执原因的 UTF-8 校验与保存一致性：拒绝原因夹带任何无效
// UTF-8 字节（含不完整多字节字符）时，SubmitReceipt 必须整体返回
// ErrInvalidArgument 与空确认，不能靠静默替换（U+FFFD）、截断或跳过坏字节
// 继续登记；失败后交换仍保持提交前状态——不留下拒绝原因、登记时间或新增
// 回执审计，原包与摘要保持原样，接收方随后换用合法原因仍能正常登记这份
// 尚未完成的回执。合法 UTF-8 原因（含中文、换行、引号、反斜杠、首尾空白
// 与用户明确输入的合法 U+FFFD）必须逐字原样保存，当次内部查看与关闭后从
// 同一位置重新打开看到的原因相同；原样重交返回首次确认与登记时间、不新增
// 审计，换成另一份合法原因返回 ErrConflict。接受回执继续忽略传入原因并保存
// 为空——即使被忽略的原因夹带无效字节，也不拒绝接受回执或改变重交结果。

// invalidRejectionReasons 是各类无效 UTF-8 拒绝原因：坏序列出现在开头、
// 结尾、中间，夹在合法中文之间，孤立续字节，不完整多字节字符，单个非法
// 字节，overlong 编码与 UTF-16 代理项编码；另有整体非空白但夹带坏字节的
// 原因，以及在合法 U+FFFD 之后紧跟非法字节——必须因那个坏字节被拒绝。
var invalidRejectionReasons = []struct {
	label  string
	reason string
}{
	{"开头非法字节", "\xff拒绝原因"},
	{"结尾非法字节", "拒绝原因\xff"},
	{"中间非法字节", "拒\xff绝原因"},
	{"中文之间夹坏字节", "拒绝\xfe原因"},
	{"孤立续字节", "拒绝原因\x80"},
	{"结尾不完整多字节字符", "拒绝原因\xe6"},
	{"中间不完整多字节序列", "ab\xe6\x9dcd"},
	{"中文之后截断三字节序列", "拒绝原因\xe7\xa1"},
	{"单个非法字节", "\x80"},
	{"overlong 编码", "\xc0\x80"},
	{"UTF-16 代理项编码", "拒绝\n\xed\xa0\x80\n"},
	{"合法替换字符后紧跟非法字节", "拒绝�\xff"},
	{"非空白原因夹带坏字节", "  拒绝原因 \xff  "},
}

// validRejectionReasons 是必须逐字原样保存的合法 UTF-8 拒绝原因。
var validRejectionReasons = []struct {
	label  string
	reason string
}{
	{"中文与全角标点", "拒绝原因：信息不一致（需复核）"},
	{"表情字符", "拒绝：内容异常 🩸，请核对 😷"},
	{"换行引号反斜杠与首尾空白", "  第一行\n第二行\t\"引号\" 与 C:\\路径  "},
	{"首尾空白", "\t\n  拒绝原因保持原样  \r\n "},
	{"用户明确输入的合法替换字符", "拒绝原因\ufffd（用户明确输入的 U+FFFD）"},
}

// setupReceiptUTF8 准备回执 UTF-8 测试夹具：一名患者、一次就诊、一条已生效
// 诊断、一条覆盖它的有效授权，以及一份已创建、尚待回执的交换。
func setupReceiptUTF8(t *testing.T, s *Store, clk *fakeClock, requestID string) (ID, Exchange) {
	t.Helper()
	p, err := s.RegisterPatient(doc, "回执原因UTF8患者")
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
	x, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{d.ID}, requestID)
	if err != nil {
		t.Fatal(err)
	}
	return p.ID, x
}

func receiptedAuditCount(events []AuditEvent) int {
	n := 0
	for _, e := range events {
		if e.Action == ActionReceipted {
			n++
		}
	}
	return n
}

// TestSubmitReceiptRejectsInvalidUTF8Reason 覆盖各类坏字节拒绝原因：在指定
// 接收方、正确摘要、其他业务条件均满足时一律 ErrInvalidArgument 与空确认，
// 不留下拒绝原因、登记时间或回执审计，原包与摘要保持原样；坏字节从未落盘
// （关闭重开后交换仍待回执、审计不变）；随后换用合法原因仍能正常登记这份
// 尚未完成的回执，此前失败不被当成已经拒绝。
func TestSubmitReceiptRejectsInvalidUTF8Reason(t *testing.T) {
	for _, tc := range invalidRejectionReasons {
		t.Run(tc.label, func(t *testing.T) {
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
			pid, x := setupReceiptUTF8(t, s, clk, "req-receipt-bad-utf8-"+tc.label)

			// 提交前基线：待回执、无正式回执，原包与摘要已固化。
			before := snapshotExchange(mustGetExchange(t, s, pid, x.ID))
			if before.Status != ExchangePending || before.Receipt != nil {
				t.Fatalf("baseline exchange must be pending without receipt: %+v", before)
			}
			if recomputed := packageDigest(before.Package); recomputed != before.Digest {
				t.Fatalf("baseline package digest mismatch")
			}
			auditBefore := mustAudit(t, s, pid)

			conf, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("SubmitReceipt(%q) err = %v, want ErrInvalidArgument", tc.reason, err)
			}
			// 失败必须返回空的回执确认。
			if conf != (ReceiptConfirmation{}) {
				t.Fatalf("rejected receipt returned non-empty confirmation: %+v", conf)
			}

			// 当次内部查看：交换仍保持提交前状态，无回执原因、无登记时间。
			got := mustGetExchange(t, s, pid, x.ID)
			assertExchangeMatches(t, got, before, "exchange after rejected receipt")
			if got.Status != ExchangePending || got.Receipt != nil {
				t.Fatalf("rejected receipt left receipt state: %+v", got.Receipt)
			}
			audit, err := s.AuditEvents(doc, pid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after rejected receipt")
			}

			// 坏字节没有落盘：关闭重开后仍是提交前状态，审计不变。
			s = reopenStore(t, s, dir, clk)
			got = mustGetExchange(t, s, pid, x.ID)
			assertExchangeMatches(t, got, before, "exchange after reopen")
			audit, err = s.AuditEvents(doc, pid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after reopen")
			}

			// 此前失败不被当成已经拒绝：换用合法原因仍能登记这份待回执交换，
			// 只新增这一次真正完成的回执审计。
			clk.t = clk.t.Add(time.Hour)
			registeredAt := clk.t
			validReason := "坏字节被拒后补登的合法原因"
			conf2, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, validReason)
			if err != nil {
				t.Fatalf("valid receipt after rejected attempt: %v", err)
			}
			if conf2.ExchangeID != x.ID || conf2.Status != ExchangeRejected ||
				conf2.Outcome != ReceiptRejected || !conf2.RegisteredAt.Equal(registeredAt) {
				t.Fatalf("unexpected confirmation after rejected attempt: %+v", conf2)
			}
			done := mustGetExchange(t, s, pid, x.ID)
			if done.Status != ExchangeRejected || done.Receipt == nil ||
				done.Receipt.Outcome != ReceiptRejected || done.Receipt.Reason != validReason ||
				!done.Receipt.RegisteredAt.Equal(registeredAt) {
				t.Fatalf("valid receipt not registered verbatim: %+v", done.Receipt)
			}
			// 原包与摘要仍保持创建时的原值（状态与回执的变化是这次合法登记本身）。
			if done.Digest != before.Digest ||
				done.Package.PatientID != before.Package.PatientID ||
				done.Package.ReceiverID != before.Package.ReceiverID ||
				!reflect.DeepEqual(done.Package.Records, before.Package.Records) {
				t.Fatalf("package or digest changed after late registration:\n got=%+v\nwant=%+v",
					done.Package, before.Package)
			}
			if recomputed := packageDigest(done.Package); recomputed != done.Digest {
				t.Fatalf("package digest no longer matches after late registration")
			}
			if got := receiptedAuditCount(mustAudit(t, s, pid)); got != receiptedAuditCount(auditBefore)+1 {
				t.Fatalf("exactly one receipt audit expected, got %d", got)
			}
			_ = s.Close()
		})
	}

	// 只含空白的原因继续被拒绝，交换仍待回执。
	s, clk := newTestStore(t)
	pid, x := setupReceiptUTF8(t, s, clk, "req-receipt-blank")
	for _, blank := range []string{"", " ", "\n\t\r ", "　"} {
		if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, blank); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("blank rejection reason %q err = %v, want ErrInvalidArgument", blank, err)
		}
	}
	got := mustGetExchange(t, s, pid, x.ID)
	if got.Status != ExchangePending || got.Receipt != nil {
		t.Fatalf("blank reasons left receipt state: %+v", got.Receipt)
	}
	if n := receiptedAuditCount(mustAudit(t, s, pid)); n != 0 {
		t.Fatalf("blank reasons added receipt audit events: %d", n)
	}
}

// TestSubmitReceiptValidReasonPreservedRoundTrip 覆盖合法拒绝原因：原样保存
// （中文、换行、引号、反斜杠与首尾空格均不改写），当次内部查看与关闭重开后
// 看到相同原因；原样重交返回首次确认与登记时间、不新增审计，换成另一份
// 合法原因返回 ErrConflict。
func TestSubmitReceiptValidReasonPreservedRoundTrip(t *testing.T) {
	for _, tc := range validRejectionReasons {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			pid, x := setupReceiptUTF8(t, s, clk, "req-receipt-valid-"+tc.label)

			clk.t = clk.t.Add(time.Hour)
			registeredAt := clk.t
			conf, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason)
			if err != nil {
				t.Fatalf("SubmitReceipt(%q): %v", tc.reason, err)
			}
			if conf.ExchangeID != x.ID || conf.Status != ExchangeRejected ||
				conf.Outcome != ReceiptRejected || !conf.RegisteredAt.Equal(registeredAt) {
				t.Fatalf("unexpected first confirmation: %+v", conf)
			}

			// 当次内部查看：原因逐字原样保存。
			view := mustGetExchange(t, s, pid, x.ID)
			if view.Status != ExchangeRejected || view.Receipt == nil ||
				view.Receipt.Reason != tc.reason ||
				!view.Receipt.RegisteredAt.Equal(registeredAt) {
				t.Fatalf("stored receipt = %+v, want reason %q", view.Receipt, tc.reason)
			}
			auditBefore := mustAudit(t, s, pid)

			// 重交时刻更晚：原样重交仍返回首次确认与首次登记时间。
			clk.t = clk.t.Add(2 * time.Hour)
			again, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason)
			if err != nil {
				t.Fatalf("identical resubmit: %v", err)
			}
			if again.ExchangeID != conf.ExchangeID || again.Status != conf.Status ||
				again.Outcome != conf.Outcome || !again.RegisteredAt.Equal(conf.RegisteredAt) {
				t.Fatalf("identical resubmit returned %+v, want first confirmation %+v", again, conf)
			}
			if got := receiptedAuditCount(mustAudit(t, s, pid)); got != receiptedAuditCount(auditBefore) {
				t.Fatalf("identical resubmit added a receipt audit event")
			}

			// 换成另一份合法原因：ErrConflict，不覆盖已保存原因或登记时间。
			if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason+"-另一份合法原因"); !errors.Is(err, ErrConflict) {
				t.Fatalf("different-reason resubmit err = %v, want ErrConflict", err)
			}
			view = mustGetExchange(t, s, pid, x.ID)
			if view.Receipt == nil || view.Receipt.Reason != tc.reason ||
				!view.Receipt.RegisteredAt.Equal(registeredAt) {
				t.Fatalf("conflicting resubmit changed stored receipt: %+v", view.Receipt)
			}

			// 关闭后从同一位置重新打开：内部查看得到逐字相同的原因与登记时间。
			s = reopenStore(t, s, dir, clk)
			t.Cleanup(func() { _ = s.Close() })
			reopened := mustGetExchange(t, s, pid, x.ID)
			if reopened.Receipt == nil || reopened.Receipt.Reason != tc.reason ||
				!reopened.Receipt.RegisteredAt.Equal(registeredAt) {
				t.Fatalf("receipt after reopen = %+v, want reason %q", reopened.Receipt, tc.reason)
			}
			xs, err := s.ListExchanges(doc, pid)
			if err != nil {
				t.Fatal(err)
			}
			if len(xs) != 1 || xs[0].Receipt == nil || xs[0].Receipt.Reason != tc.reason {
				t.Fatalf("ListExchanges after reopen = %+v", xs)
			}

			// 重开后用相同结果和原样原因重交：仍返回首次确认，不新增审计。
			again2, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, tc.reason)
			if err != nil {
				t.Fatalf("identical resubmit after reopen: %v", err)
			}
			if !again2.RegisteredAt.Equal(registeredAt) || again2.Outcome != ReceiptRejected {
				t.Fatalf("resubmit after reopen returned %+v, want registered at %v", again2, registeredAt)
			}
			if got := receiptedAuditCount(mustAudit(t, s, pid)); got != receiptedAuditCount(auditBefore) {
				t.Fatalf("resubmit after reopen added a receipt audit event")
			}
		})
	}
}

// TestSubmitReceiptExplicitReplacementCharDistinctFromBadBytes 覆盖撞号场景：
// 已登记一份原因中含用户明确输入的合法 U+FFFD 的拒绝回执后，提交只有该位置
// 换成坏字节（JSON 落盘会被替换成 U+FFFD）的原因必须返回 ErrInvalidArgument，
// 而不是被当成同一份回执返回原确认；已保存原因、登记时间与审计保持原样，
// 关闭重开后也不变。
func TestSubmitReceiptExplicitReplacementCharDistinctFromBadBytes(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	pid, x := setupReceiptUTF8(t, s, clk, "req-receipt-fffd")

	explicit := "拒绝原因-�"
	clk.t = clk.t.Add(time.Hour)
	registeredAt := clk.t
	conf, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, explicit)
	if err != nil {
		t.Fatalf("register receipt with explicit U+FFFD: %v", err)
	}
	auditBefore := mustAudit(t, s, pid)

	// 坏字节版本（替换后恰好等于 explicit）必须被拒绝，不能返回已保存确认。
	bad, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "拒绝原因-\xff")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad-utf8 resubmit err = %v, want ErrInvalidArgument", err)
	}
	if bad != (ReceiptConfirmation{}) {
		t.Fatalf("bad-utf8 resubmit returned non-empty confirmation: %+v", bad)
	}

	// 已保存回执原样不变。
	view := mustGetExchange(t, s, pid, x.ID)
	if view.Receipt == nil || view.Receipt.Reason != explicit ||
		!view.Receipt.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("stored receipt changed: %+v", view.Receipt)
	}
	if got := receiptedAuditCount(mustAudit(t, s, pid)); got != receiptedAuditCount(auditBefore) {
		t.Fatalf("bad-utf8 resubmit added a receipt audit event")
	}

	// 关闭重开后仍是明确输入的合法 U+FFFD 原因；原样重交返回首次确认。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	reopened := mustGetExchange(t, s, pid, x.ID)
	if reopened.Receipt == nil || reopened.Receipt.Reason != explicit ||
		!reopened.Receipt.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("receipt after reopen = %+v, want %q", reopened.Receipt, explicit)
	}
	again, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, explicit)
	if err != nil {
		t.Fatalf("resubmit explicit U+FFFD after reopen: %v", err)
	}
	if again.ExchangeID != conf.ExchangeID || !again.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("resubmit after reopen returned %+v, want confirmation at %v", again, registeredAt)
	}
}

// TestSubmitReceiptAcceptedIgnoresReasonEvenWithBadUTF8 覆盖接受回执：传入原因
// 被忽略并保存为空，即使原因夹带无效 UTF-8 字节也不拒绝接受回执；换一段同样
// 含坏字节的原因重交仍返回首次确认，不新增审计；关闭重开后接受回执的原因仍
// 为空、登记时间保持首次值。
func TestSubmitReceiptAcceptedIgnoresReasonEvenWithBadUTF8(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	pid, x := setupReceiptUTF8(t, s, clk, "req-receipt-accepted-bad-utf8")

	clk.t = clk.t.Add(time.Hour)
	registeredAt := clk.t
	conf, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, "接受时夹带坏字节\xff的被忽略原因")
	if err != nil {
		t.Fatalf("accepted receipt with bad-utf8 ignored reason: %v", err)
	}
	if conf.ExchangeID != x.ID || conf.Status != ExchangeAccepted ||
		conf.Outcome != ReceiptAccepted || !conf.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("unexpected accepted confirmation: %+v", conf)
	}
	view := mustGetExchange(t, s, pid, x.ID)
	if view.Status != ExchangeAccepted || view.Receipt == nil ||
		view.Receipt.Outcome != ReceiptAccepted || view.Receipt.Reason != "" {
		t.Fatalf("accepted receipt must store empty reason, got %+v", view.Receipt)
	}
	auditBefore := mustAudit(t, s, pid)

	// 重交时换成另一段含坏字节的原因：接受忽略原因，仍确认同一接受结果，
	// 登记时间不被改写，也不新增审计。
	clk.t = clk.t.Add(2 * time.Hour)
	for _, reason := range []string{
		"另一段含坏字节\xff的被忽略原因",
		"\xe6截断的多字节字符",
		"普通合法文字也一样被忽略",
		"",
	} {
		again, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, reason)
		if err != nil {
			t.Fatalf("accepted resubmit with ignored reason %q: %v", reason, err)
		}
		if again.ExchangeID != conf.ExchangeID || again.Status != ExchangeAccepted ||
			!again.RegisteredAt.Equal(registeredAt) {
			t.Fatalf("accepted resubmit returned %+v, want first confirmation %+v", again, conf)
		}
	}
	if got := receiptedAuditCount(mustAudit(t, s, pid)); got != receiptedAuditCount(auditBefore) {
		t.Fatalf("accepted resubmits added receipt audit events")
	}

	// 关闭重开：接受回执原因仍为空，登记时间保持首次值；坏字节从未落盘。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	reopened := mustGetExchange(t, s, pid, x.ID)
	if reopened.Status != ExchangeAccepted || reopened.Receipt == nil ||
		reopened.Receipt.Reason != "" || !reopened.Receipt.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("accepted receipt after reopen = %+v", reopened.Receipt)
	}
	again, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, "重开后再次夹带坏字节\xff")
	if err != nil {
		t.Fatalf("accepted resubmit after reopen: %v", err)
	}
	if !again.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("registered time changed after reopen: %v", again.RegisteredAt)
	}
	if got := receiptedAuditCount(mustAudit(t, s, pid)); got != receiptedAuditCount(auditBefore) {
		t.Fatalf("accepted resubmit after reopen added a receipt audit event")
	}
}

// TestSubmitReceiptInvalidUTF8ReasonAfterDeactivation 保证 UTF-8 校验与“停用
// 后仍可确认此前已取得包”的既有规则并存：患者停用后，夹带坏字节的拒绝原因
// 仍返回 ErrInvalidArgument 且不留状态；换用合法原因则仍可正常登记。
func TestSubmitReceiptInvalidUTF8ReasonAfterDeactivation(t *testing.T) {
	s, clk := newTestStore(t)
	pid, x := setupReceiptUTF8(t, s, clk, "req-receipt-deactivated")

	// 接收方先成功取包，随后内部使用者停用患者。
	if _, err := s.FetchPackage(rcv, x.ID); err != nil {
		t.Fatalf("fetch before deactivation: %v", err)
	}
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatal(err)
	}

	auditBefore := mustAudit(t, s, pid)
	if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "停用后登记\xff坏原因"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad-utf8 rejection after deactivation err = %v, want ErrInvalidArgument", err)
	}
	got := mustGetExchange(t, s, pid, x.ID)
	if got.Status != ExchangePending || got.Receipt != nil {
		t.Fatalf("bad-utf8 rejection after deactivation left state: %+v", got.Receipt)
	}
	if !reflect.DeepEqual(mustAudit(t, s, pid), auditBefore) {
		t.Fatalf("bad-utf8 rejection after deactivation added audit")
	}

	// 停用后换用合法原因仍可确认此前已取得的包。
	clk.t = clk.t.Add(time.Hour)
	conf, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "停用后补登的合法拒绝原因")
	if err != nil {
		t.Fatalf("valid rejection after deactivation: %v", err)
	}
	if conf.Status != ExchangeRejected || !conf.RegisteredAt.Equal(clk.t) {
		t.Fatalf("unexpected confirmation after deactivation: %+v", conf)
	}
}
