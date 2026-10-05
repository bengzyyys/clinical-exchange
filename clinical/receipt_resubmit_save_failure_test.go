package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestReceiptResubmitConfirmedWithoutSave 为“交换回执的重复确认”补回归保障：
// 接收方已通过 SubmitReceipt 成功登记回执后，本地保存条件暂时不可用期间再次
// 提交同一份回执，必须仍能拿到首次成功登记时的原确认——重复确认只读取既有
// 结果，不能再次依赖写盘成功。
//
// 覆盖约定：
//   - 接受回执不保存原因：首次登记或重交时附带的原因文字都不会成为正式原因，
//     重交时文字不同也仍确认同一个接受结果；登记时间保持首次登记时刻，不被
//     改成重交时刻。
//   - 拒绝回执以“原拒绝结果 + 原原因”共同确认：原原因中的非空白文字及首尾
//     空格原样保留；原样重交（含空格）得到原确认；去掉首尾空格或换成另一段
//     非空白原因都返回 ErrConflict，不能覆盖已保存原因或登记时间。
//   - 接受改成带非空白原因的拒绝、拒绝改成接受，均明确冲突。
//   - 上述冲突在无法保存期间仍按业务规则返回，不能被保存错误替代；其他参数
//     合法但摘要不符，也不能因为交换已有回执而得到成功确认。
//   - 重复确认不改变内部视图中的正式回执、原包内容与摘要，不新增第二条回执
//     登记审计事件，既有审计事件保持原内容与顺序。
//   - 首次登记与重复确认的边界：另一份仍在等待回执的交换，即使接收方、摘要
//     与回执内容都合法，在同样无法保存的条件下首次登记仍返回实际保存错误，
//     内部继续看到待回执且无正式回执，不新增回执审计。
func TestReceiptResubmitConfirmedWithoutSave(t *testing.T) {
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
	p, err := s.RegisterPatient(doc, "回执重复确认患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断v1内容")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, rd.ID); err != nil {
		t.Fatal(err)
	}
	grant, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Minute), clk.t.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// 三份起点交换：一份接受、一份拒绝（均已成功登记），另一份仍待回执，
	// 用于守住“首次登记仍须落盘”的边界。
	newExchange := func(req string) Exchange {
		x, err := s.CreateExchange(doc, p.ID, rcv.ID, grant.ID, []ID{rd.ID}, req)
		if err != nil {
			t.Fatalf("create exchange %q: %v", req, err)
		}
		return x
	}
	xAcc := newExchange("req-accepted")
	xRej := newExchange("req-rejected")
	xPending := newExchange("req-pending")

	// 首次登记接受回执：附带的原因文字必须被丢弃，不成为正式原因。
	clk.t = clk.t.Add(time.Hour)
	acceptedAt := clk.t
	confAcc, err := s.SubmitReceipt(rcv, xAcc.ID, xAcc.Digest, ReceiptAccepted, "首次登记时随手附带的接受文字")
	if err != nil {
		t.Fatalf("first accepted receipt: %v", err)
	}
	if confAcc.ExchangeID != xAcc.ID || confAcc.Status != ExchangeAccepted ||
		confAcc.Outcome != ReceiptAccepted || !confAcc.RegisteredAt.Equal(acceptedAt) {
		t.Fatalf("unexpected first accepted confirmation: %+v", confAcc)
	}

	// 首次登记拒绝回执：原因带非空白文字与首尾空格，必须原样保存。
	clk.t = clk.t.Add(time.Hour)
	rejectedAt := clk.t
	rejectionReason := "\t 保留首尾空格的拒绝原因 \n"
	confRej, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, rejectionReason)
	if err != nil {
		t.Fatalf("first rejected receipt: %v", err)
	}
	if confRej.ExchangeID != xRej.ID || confRej.Status != ExchangeRejected ||
		confRej.Outcome != ReceiptRejected || !confRej.RegisteredAt.Equal(rejectedAt) {
		t.Fatalf("unexpected first rejected confirmation: %+v", confRej)
	}

	// 保存条件不可用之前的正式基线：内部视图、审计与取包视图。
	accBefore := snapshotExchange(mustGetExchange(t, s, p.ID, xAcc.ID))
	rejBefore := snapshotExchange(mustGetExchange(t, s, p.ID, xRej.ID))
	pendingBefore := snapshotExchange(mustGetExchange(t, s, p.ID, xPending.ID))
	if accBefore.Receipt == nil || accBefore.Receipt.Outcome != ReceiptAccepted || accBefore.Receipt.Reason != "" {
		t.Fatalf("accepted receipt must store no reason, got %+v", accBefore.Receipt)
	}
	if rejBefore.Receipt == nil || rejBefore.Receipt.Reason != rejectionReason {
		t.Fatalf("rejected reason must be preserved verbatim, got %+v", rejBefore.Receipt)
	}
	if pendingBefore.Status != ExchangePending || pendingBefore.Receipt != nil {
		t.Fatalf("pending baseline exchange must have no receipt: %+v", pendingBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}
	restoreSave := func() {
		t.Helper()
		if err := os.Remove(dataPath); err != nil {
			t.Fatalf("unblock data path: %v", err)
		}
		if err := os.Rename(backupPath, dataPath); err != nil {
			t.Fatalf("restore data file: %v", err)
		}
	}

	// 重交时刻明确晚于两次首次登记：原确认的登记时间绝不能被改成这个时刻。
	clk.t = clk.t.Add(2 * time.Hour)
	resubmitAt := clk.t
	if !resubmitAt.After(acceptedAt) || !resubmitAt.After(rejectedAt) {
		t.Fatal("test clock must place resubmissions after first registrations")
	}

	assertSameConfirmation := func(label string, got, want ReceiptConfirmation) {
		t.Helper()
		if got.ExchangeID != want.ExchangeID || got.Status != want.Status ||
			got.Outcome != want.Outcome || !got.RegisteredAt.Equal(want.RegisteredAt) {
			t.Fatalf("%s: confirmation changed:\n got=%+v\nwant=%+v (resubmit time %v must not be used)",
				label, got, want, resubmitAt)
		}
	}

	// 1) 接受回执重交：换一段完全不同的原因文字，仍确认同一个接受结果，
	//    不需要落盘即可在保存失败期间成功。
	gotAcc, err := s.SubmitReceipt(rcv, xAcc.ID, xAcc.Digest, ReceiptAccepted, "重交时另一段完全不同的接受文字")
	if err != nil {
		t.Fatalf("accepted resubmit during save failure: %v", err)
	}
	assertSameConfirmation("accepted resubmit", gotAcc, confAcc)

	// 2) 拒绝回执原样重交（含首尾空白）：得到首次登记的原确认。
	gotRej, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, rejectionReason)
	if err != nil {
		t.Fatalf("identical rejected resubmit during save failure: %v", err)
	}
	assertSameConfirmation("rejected resubmit", gotRej, confRej)

	// 3) 拒绝回执只去掉首尾空格：原因并非原样，必须冲突，空格不能被归一化。
	if _, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, "保留首尾空格的拒绝原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("trimmed-reason resubmit err = %v, want ErrConflict", err)
	}

	// 4) 拒绝回执改成另一段非空白原因：ErrConflict，不允许覆盖。
	if _, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptRejected, "试图覆盖的另一段拒绝原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different-reason resubmit err = %v, want ErrConflict", err)
	}

	// 5) 已保存的接受改成带非空白原因的拒绝：明确冲突。
	if _, err := s.SubmitReceipt(rcv, xAcc.ID, xAcc.Digest, ReceiptRejected, "想把接受翻成拒绝"); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted->rejected err = %v, want ErrConflict", err)
	}

	// 6) 已保存的拒绝改成接受：明确冲突。
	if _, err := s.SubmitReceipt(rcv, xRej.ID, xRej.Digest, ReceiptAccepted, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("rejected->accepted err = %v, want ErrConflict", err)
	}

	// 7) 其他参数合法但摘要不符：不能因为交换已有回执就成功确认，也不能
	//    返回保存错误——摘要核对先于重复确认，返回 ErrConflict。
	if _, err := s.SubmitReceipt(rcv, xAcc.ID, "digest-does-not-match", ReceiptAccepted, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong digest on receipted exchange err = %v, want ErrConflict", err)
	}
	if _, err := s.SubmitReceipt(rcv, xRej.ID, "digest-does-not-match", ReceiptRejected, rejectionReason); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong digest on rejected exchange err = %v, want ErrConflict", err)
	}

	// 8) 首次登记边界：仍在等待回执的交换，接收方、摘要与回执内容均合法，
	//    但保存失败——必须返回实际保存错误，而不是业务错误。
	if _, err := s.SubmitReceipt(rcv, xPending.ID, xPending.Digest, ReceiptAccepted, ""); err == nil {
		t.Fatal("first receipt registration during save failure must fail")
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) ||
		errors.Is(err, ErrNotFound) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 保存失败期间内部视图仍可查：正式回执、原包内容与摘要均保持首次登记
	// 后状态，重交与冲突尝试没有留下任何变化。
	assertExchangeMatches(t, mustGetExchange(t, s, p.ID, xAcc.ID), accBefore, "accepted exchange during outage")
	assertExchangeMatches(t, mustGetExchange(t, s, p.ID, xRej.ID), rejBefore, "rejected exchange during outage")
	assertExchangeMatches(t, mustGetExchange(t, s, p.ID, xPending.ID), pendingBefore, "pending exchange during outage")

	// 审计：重复确认不新增第二条回执事件，冲突尝试与失败的首次登记也不新增，
	// 既有事件保持原内容与顺序。
	auditDuring, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditDuring, auditBefore) {
		t.Fatalf("audit changed during outage:\nbefore: %+v\nafter:  %+v", auditBefore, auditDuring)
	}

	// 取包视图同样维持原状态与原摘要。
	dAcc, err := s.FetchPackage(rcv, xAcc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dAcc.Status != ExchangeAccepted || dAcc.Digest != xAcc.Digest {
		t.Fatalf("accepted delivery changed during outage: %+v", dAcc)
	}
	dRej, err := s.FetchPackage(rcv, xRej.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dRej.Status != ExchangeRejected || dRej.Digest != xRej.Digest {
		t.Fatalf("rejected delivery changed during outage: %+v", dRej)
	}

	// 恢复保存条件：内存中的正式状态与中断前一致（中断期间本就无落盘）。
	restoreSave()

	gotAcc2 := mustGetExchange(t, s, p.ID, xAcc.ID)
	gotRej2 := mustGetExchange(t, s, p.ID, xRej.ID)
	gotPending2 := mustGetExchange(t, s, p.ID, xPending.ID)
	assertExchangeMatches(t, gotAcc2, accBefore, "accepted exchange after recovery")
	assertExchangeMatches(t, gotRej2, rejBefore, "rejected exchange after recovery")
	assertExchangeMatches(t, gotPending2, pendingBefore, "pending exchange after recovery")
	if gotAcc2.Receipt.Reason != "" || !gotAcc2.Receipt.RegisteredAt.Equal(acceptedAt) {
		t.Fatalf("accepted receipt altered by resubmits: %+v", gotAcc2.Receipt)
	}
	if gotRej2.Receipt.Reason != rejectionReason || !gotRej2.Receipt.RegisteredAt.Equal(rejectedAt) {
		t.Fatalf("rejected receipt altered by conflicting attempts: %+v", gotRej2.Receipt)
	}
	if gotPending2.Status != ExchangePending || gotPending2.Receipt != nil {
		t.Fatalf("failed first registration left a receipt: %+v", gotPending2)
	}
	// 包摘要仍可由原包内容重算，重复确认没有触发重新打包。
	for _, g := range []Exchange{gotAcc2, gotRej2, gotPending2} {
		if recomputed := packageDigest(g.Package); recomputed != g.Digest {
			t.Fatalf("package regenerated for exchange %q: digest %q, recomputed %q", g.ID, g.Digest, recomputed)
		}
	}

	auditAfter, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditAfter, auditBefore) {
		t.Fatalf("audit changed after recovery:\nbefore: %+v\nafter:  %+v", auditBefore, auditAfter)
	}
	// 每份已登记交换恰好一条回执事件，待回执交换没有任何回执事件。
	receiptEvents := map[ID][]AuditEvent{}
	for _, ev := range auditAfter {
		if ev.Action == ActionReceipted {
			receiptEvents[ev.ObjectID] = append(receiptEvents[ev.ObjectID], ev)
		}
	}
	if len(receiptEvents[xAcc.ID]) != 1 || receiptEvents[xAcc.ID][0].OccurredAt != acceptedAt ||
		receiptEvents[xAcc.ID][0].ActorID != rcv.ID {
		t.Fatalf("accepted exchange receipt events = %+v, want one at %v", receiptEvents[xAcc.ID], acceptedAt)
	}
	if len(receiptEvents[xRej.ID]) != 1 || receiptEvents[xRej.ID][0].OccurredAt != rejectedAt ||
		receiptEvents[xRej.ID][0].ActorID != rcv.ID {
		t.Fatalf("rejected exchange receipt events = %+v, want one at %v", receiptEvents[xRej.ID], rejectedAt)
	}
	if len(receiptEvents[xPending.ID]) != 0 {
		t.Fatalf("pending exchange must have no receipt events, got %+v", receiptEvents[xPending.ID])
	}

	// 关闭后从同一数据位置重新打开：磁盘上的正式状态与上述全部断言一致，
	// 中断期间的重复确认/冲突/失败首登都没有改变任何已保存内容。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	assertExchangeMatches(t, mustGetExchange(t, s2, p.ID, xAcc.ID), accBefore, "accepted exchange after reopen")
	assertExchangeMatches(t, mustGetExchange(t, s2, p.ID, xRej.ID), rejBefore, "rejected exchange after reopen")
	assertExchangeMatches(t, mustGetExchange(t, s2, p.ID, xPending.ID), pendingBefore, "pending exchange after reopen")
	auditReopen, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditReopen, auditBefore) {
		t.Fatalf("audit changed after reopen:\nbefore: %+v\nafter:  %+v", auditBefore, auditReopen)
	}

	// 恢复后首次登记此前待回执的交换：保存成功，只新增一条回执审计，
	// 证明中断期间的失败尝试没有建立任何回执。
	clk.t = clk.t.Add(time.Hour)
	registeredAfterRecovery := clk.t
	confPending, err := s2.SubmitReceipt(rcv, xPending.ID, xPending.Digest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("first registration after recovery: %v", err)
	}
	if confPending.Status != ExchangeAccepted || !confPending.RegisteredAt.Equal(registeredAfterRecovery) {
		t.Fatalf("unexpected confirmation after recovery: %+v", confPending)
	}
	auditFinal, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditFinal) != len(auditBefore)+1 || !reflect.DeepEqual(auditFinal[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after late first registration:\nbefore: %+v\nafter:  %+v", auditBefore, auditFinal)
	}
}

// mustGetExchange 取内部使用者视角下的交换，失败即终止测试。
func mustGetExchange(t *testing.T, s *Store, pid, xid ID) Exchange {
	t.Helper()
	x, err := s.GetExchange(doc, pid, xid)
	if err != nil {
		t.Fatalf("get exchange %q: %v", xid, err)
	}
	return x
}
