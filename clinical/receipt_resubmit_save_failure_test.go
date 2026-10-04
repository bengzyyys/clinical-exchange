package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestReceiptResubmitSucceedsDespiteSaveFailure 覆盖“一份交换的接受或拒绝回执
// 已经成功保存后，指定接收方用同一交换标识、正确摘要与相同回执再次提交，而
// 本地保存条件当前不可用”的确认场景。
//
// 相同回执的重交只是确认既有登记：必须成功返回首次成功登记的原确认（交换标识、
// 交换状态、回执结果与登记时间完全一致，当前时间变化不重设登记时间），不要求
// 本次还能保存数据；不落盘、不新增回执事件、不改写正式回执、原包内容/摘要/
// 拒绝原因/既有审计保持原样。改为另一种合法结果、改用另一段非空白拒绝原因
// （包括仅首尾空格不同）或摘要不匹配时仍返回 ErrConflict，不变成保存错误；
// 其他接收方、内部使用者及不存在的交换仍返回 ErrAccessDenied。
//
// 这只适用于已成功登记的回执：仍待回执的交换首次登记必须真正保存成功，保存
// 失败时明确报错、交换保持待回执、无正式回执或新增审计，失败尝试不能当作已
// 登记的回执确认。授权撤回后，指定接收方仍可确认此前包的回执，响应只含确认
// 信息、不重新提供包内容。
func TestReceiptResubmitSucceedsDespiteSaveFailure(t *testing.T) {
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
	p, err := s.RegisterPatient(doc, "回执重交保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断内容")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, rd.ID); err != nil {
		t.Fatal(err)
	}
	ro, err := s.CreateDraft(doc, p.ID, e.ID, Order, "医嘱内容")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, ro.ID); err != nil {
		t.Fatal(err)
	}
	a, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}, {EncounterID: e.ID, Category: Order}},
		clk.t.Add(-time.Minute), clk.t.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// xRej：已成功登记拒绝回执（原因非空）的交换。
	xRej, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rd.ID}, "req-resub-rej")
	if err != nil {
		t.Fatal(err)
	}
	// xAcc：已成功登记接受回执的交换（接受不保存原因）。
	xAcc, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{ro.ID}, "req-resub-acc")
	if err != nil {
		t.Fatal(err)
	}
	// xPending：交换已保存、但始终等待回执，供验证首次登记仍要求保存成功。
	xPending, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rd.ID, ro.ID}, "req-resub-pending")
	if err != nil {
		t.Fatal(err)
	}

	clk.t = clk.t.Add(time.Hour)
	registeredAt := clk.t
	confRej, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, "正式的拒绝原因")
	if err != nil {
		t.Fatalf("register rejected receipt: %v", err)
	}
	if confRej.Status != ExchangeRejected || confRej.Outcome != ReceiptRejected ||
		confRej.ExchangeID != xRej.ID || !confRej.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("bad rejected confirmation: %+v", confRej)
	}
	clk.t = clk.t.Add(time.Minute)
	acceptedAt := clk.t
	confAcc, err := s.SubmitReceipt(rcv, xAcc.ID, xAcc.Digest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("register accepted receipt: %v", err)
	}
	if confAcc.Status != ExchangeAccepted || confAcc.Outcome != ReceiptAccepted ||
		confAcc.ExchangeID != xAcc.ID || !confAcc.RegisteredAt.Equal(acceptedAt) {
		t.Fatalf("bad accepted confirmation: %+v", confAcc)
	}

	// 正式基线：内部视图（两份已登记交换 + 待回执交换）、审计与取包视图。
	wantRej, err := s.GetExchange(doc, p.ID, xRej.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantRejBase := snapshotExchange(wantRej)
	wantAcc, err := s.GetExchange(doc, p.ID, xAcc.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantAccBase := snapshotExchange(wantAcc)
	wantPending, err := s.GetExchange(doc, p.ID, xPending.ID)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore)[ActionReceipted]; got != 2 {
		t.Fatalf("baseline receipt events = %d, want 2", got)
	}
	listBefore, err := s.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	deliveryRejBefore, err := s.FetchPackage(rcv, xRej.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、摘要或原因校验无关。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	blockSaves := func() {
		t.Helper()
		if err := os.Rename(dataPath, backupPath); err != nil {
			t.Fatalf("backup data file: %v", err)
		}
		if err := os.Mkdir(dataPath, 0o755); err != nil {
			t.Fatalf("block data path: %v", err)
		}
	}
	restoreSaves := func() {
		t.Helper()
		if err := os.Remove(dataPath); err != nil {
			t.Fatalf("unblock data path: %v", err)
		}
		if err := os.Rename(backupPath, dataPath); err != nil {
			t.Fatalf("restore data file: %v", err)
		}
	}
	blockSaves()

	// 时钟大幅后移/前移都不得影响重交返回的登记时间。
	clk.t = clk.t.Add(24 * time.Hour)

	// 相同拒绝回执重交（保存条件不可用）：成功返回首次登记的原确认。
	gotConf, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, "正式的拒绝原因")
	if err != nil {
		t.Fatalf("identical resubmit during save failure: %v", err)
	}
	if gotConf.ExchangeID != confRej.ExchangeID || gotConf.Status != confRej.Status ||
		gotConf.Outcome != confRej.Outcome || !gotConf.RegisteredAt.Equal(confRej.RegisteredAt) {
		t.Fatalf("resubmit confirmation %+v != original %+v", gotConf, confRej)
	}
	if !gotConf.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("registered time reset to %v, want original %v", gotConf.RegisteredAt, registeredAt)
	}

	// 接受回执不保存原因：重交时即使附带不同说明，也仍视为同一次接受。
	gotConf, err = s.SubmitReceipt(rcv, xAcc.ID, xAcc.Digest, ReceiptAccepted, "重交时附带的另一段说明")
	if err != nil {
		t.Fatalf("accepted resubmit with different note during save failure: %v", err)
	}
	if gotConf.ExchangeID != confAcc.ExchangeID || gotConf.Status != confAcc.Status ||
		gotConf.Outcome != confAcc.Outcome || !gotConf.RegisteredAt.Equal(confAcc.RegisteredAt) {
		t.Fatalf("accepted resubmit confirmation %+v != original %+v", gotConf, confAcc)
	}

	// 改为另一种合法结果：仍是 ErrConflict，不能因无法保存变成保存错误。
	if _, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptAccepted, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed outcome during save failure err = %v, want ErrConflict", err)
	}
	if _, err := s.SubmitReceipt(rcv, xAcc.ID, xAcc.Digest, ReceiptRejected, "改成拒绝"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed outcome during save failure err = %v, want ErrConflict", err)
	}
	// 改用另一段非空白拒绝原因：ErrConflict，且必须按原文比较——
	// 不能自行忽略首尾空格或改写文字。
	if _, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, "另一个非空白原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed reason during save failure err = %v, want ErrConflict", err)
	}
	if _, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, " 正式的拒绝原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("leading-space reason must not be treated as identical: err = %v, want ErrConflict", err)
	}
	if _, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, "正式的拒绝原因\n"); !errors.Is(err, ErrConflict) {
		t.Fatalf("trailing-newline reason must not be treated as identical: err = %v, want ErrConflict", err)
	}
	// 摘要不匹配：ErrConflict。
	if _, err := s.SubmitReceipt(rcv, xRej.ID, "deadbeef", ReceiptRejected, "正式的拒绝原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("digest mismatch during save failure err = %v, want ErrConflict", err)
	}
	// 其他接收方、内部使用者、不存在的交换：ErrAccessDenied，不能借重交查询他人回执。
	if _, err := s.SubmitReceipt(rcvB, xRej.ID, xRej.Digest, ReceiptRejected, "正式的拒绝原因"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other receiver resubmit err = %v, want ErrAccessDenied", err)
	}
	if _, err := s.SubmitReceipt(doc, xRej.ID, xRej.Digest, ReceiptRejected, "正式的拒绝原因"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("internal resubmit err = %v, want ErrAccessDenied", err)
	}
	if _, err := s.SubmitReceipt(rcv, "exch_missing", xRej.Digest, ReceiptRejected, "正式的拒绝原因"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("missing exchange resubmit err = %v, want ErrAccessDenied", err)
	}

	// 待回执交换的首次登记不受本次调整保护：保存失败必须明确报错，
	// 且不能伪装成任何业务错误。
	pendingAttemptAt := clk.t
	if _, err := s.SubmitReceipt(rcv, xPending.ID, xPending.Digest, ReceiptRejected, "待回执交换的首次尝试"); err == nil {
		t.Fatal("first registration during save failure must fail")
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) ||
		errors.Is(err, ErrNotFound) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败尝试不留临时文件，也不能被随后的重交当作已登记回执——保存仍不可用，
	// 重交必须继续报保存错误，而不是返回“原确认”。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}
	if _, err := s.SubmitReceipt(rcv, xPending.ID, xPending.Digest, ReceiptRejected, "待回执交换的首次尝试"); err == nil {
		t.Fatal("resubmit after a failed first save must still require a successful save")
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrAccessDenied) {
		t.Fatalf("failed first attempt treated as a registered receipt: %v", err)
	}

	// 保存不可用窗口结束：正式数据与全部重交前基线一致——重复提交没有新增
	// 回执事件、没有改写已保存回执；待回执交换仍无正式回执。
	restoreSaves()
	gotRej, err := s.GetExchange(doc, p.ID, xRej.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, gotRej, wantRejBase, "rejected exchange after blocked window")
	gotAcc, err := s.GetExchange(doc, p.ID, xAcc.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, gotAcc, wantAccBase, "accepted exchange after blocked window")
	gotPending, err := s.GetExchange(doc, p.ID, xPending.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, gotPending, wantPending, "pending exchange after failed attempts")
	if gotPending.Status != ExchangePending || gotPending.Receipt != nil {
		t.Fatalf("pending exchange gained a receipt: %+v", gotPending)
	}
	auditAfter, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditAfter, auditBefore) {
		t.Fatalf("resubmits changed audit:\nbefore: %+v\nafter:  %+v", auditBefore, auditAfter)
	}
	listAfter, err := s.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listAfter, listBefore) {
		t.Fatalf("resubmits changed exchange list:\nbefore: %+v\nafter:  %+v", listBefore, listAfter)
	}
	deliveryRejAfter, err := s.FetchPackage(rcv, xRej.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertDeliveryMatches(t, deliveryRejAfter, deliveryRejBefore, "delivery after blocked window")

	// 保存恢复后：待回执交换首次登记成功，失败尝试不构成冲突；
	// 正式登记时间以本次为准，只新增一条回执审计。
	clk.t = clk.t.Add(time.Hour)
	pendingRegisteredAt := clk.t
	if pendingRegisteredAt.Equal(pendingAttemptAt) {
		t.Fatal("test clock must distinguish failed attempt from successful registration")
	}
	confPending, err := s.SubmitReceipt(rcv, xPending.ID, xPending.Digest, ReceiptRejected, "恢复后正式登记原因")
	if err != nil {
		t.Fatalf("first registration after recovery: %v", err)
	}
	if confPending.Status != ExchangeRejected || !confPending.RegisteredAt.Equal(pendingRegisteredAt) {
		t.Fatalf("bad pending confirmation: %+v", confPending)
	}
	auditRecovered, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRecovered) != len(auditBefore)+1 ||
		!reflect.DeepEqual(auditRecovered[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after recovery:\nbefore: %+v\nafter:  %+v", auditBefore, auditRecovered)
	}
	last := auditRecovered[len(auditRecovered)-1]
	if last.Action != ActionReceipted || last.ObjectID != xPending.ID ||
		last.ActorID != rcv.ID || !last.OccurredAt.Equal(pendingRegisteredAt) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}

	// 授权撤回后取包已被拒绝，但指定接收方仍可确认此前已登记的回执；
	// 再次让本地保存失败，确认仍须成功且只含确认信息、不提供包内容。
	if err := s.Revoke(doc, p.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FetchPackage(rcv, xRej.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("fetch after revoke err = %v, want ErrAccessDenied", err)
	}
	blockSaves()
	gotConf, err = s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, "正式的拒绝原因")
	if err != nil {
		t.Fatalf("identical resubmit after revoke during save failure: %v", err)
	}
	if gotConf.ExchangeID != confRej.ExchangeID || gotConf.Status != confRej.Status ||
		gotConf.Outcome != confRej.Outcome || !gotConf.RegisteredAt.Equal(confRej.RegisteredAt) {
		t.Fatalf("post-revoke resubmit confirmation %+v != original %+v", gotConf, confRej)
	}
	restoreSaves()

	// 撤回本身新增了一条撤回审计；以撤回后的审计作为重开基线（回执事件仍为 3 条）。
	auditFinal, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditFinal)[ActionReceipted]; got != 3 {
		t.Fatalf("receipt events before reopen = %d, want exactly 3", got)
	}

	// 重开持久化校验：三份正式回执、原包内容/摘要、审计完整保留，
	// 重交没有留下额外回执事件或覆盖任何数据。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	gotRej, err = s2.GetExchange(doc, p.ID, xRej.ID)
	if err != nil {
		t.Fatalf("get rejected exchange after reopen: %v", err)
	}
	assertExchangeMatches(t, gotRej, wantRejBase, "rejected exchange after reopen")
	gotAcc, err = s2.GetExchange(doc, p.ID, xAcc.ID)
	if err != nil {
		t.Fatalf("get accepted exchange after reopen: %v", err)
	}
	assertExchangeMatches(t, gotAcc, wantAccBase, "accepted exchange after reopen")
	auditReopen, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditReopen, auditFinal) {
		t.Fatalf("audit after reopen:\nbefore: %+v\nafter:  %+v", auditFinal, auditReopen)
	}
	if got := auditActions(auditReopen)[ActionReceipted]; got != 3 {
		t.Fatalf("receipt events after reopen = %d, want exactly 3", got)
	}
}
