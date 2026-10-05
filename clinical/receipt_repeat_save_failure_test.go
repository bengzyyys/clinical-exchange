package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestRepeatReceiptConfirmsWithoutSave 保护“首次登记”与“确认既有回执”的边界。
//
// 接收方已通过 SubmitReceipt 成功登记接受与拒绝两份回执后，令本地保存条件
// 暂时不可用（数据文件原位置被同名目录占据，原子改名必然失败）。在此期间：
//   - 指定接收方携带正确摘要重交同一份回执：成功返回首次成功登记的确认
//     （交换标识、当前状态、回执结果、登记时间逐字一致），不尝试写盘；即使
//     提交时刻晚于首次登记，登记时间也不得改成本次时间；
//   - 接受回执不保存原因：首次提交与重交时附带的原因文字都不会成为正式原因，
//     因此附带不同文字仍确认同一个接受结果；
//   - 拒绝回执以原结果与原原因共同确认：原原因中的非空白文字及首尾空格原样
//     保留，逐字相同（含首尾空格）才确认；去掉空格或换成另一段非空白原因
//     都返回 ErrConflict，不能覆盖已保存的原因或登记时间；
//   - 接受改拒绝（带非空白原因）、拒绝改接受同样返回 ErrConflict；摘要不符
//     也不能因为交换已有回执而得到成功确认。这些冲突在无法保存期间仍按业务
//     规则返回，不能被保存错误替代；
//   - 另一份仍在等待回执的交换，即使接收方、摘要与回执内容全部合法，保存
//     不可用期间的首次登记仍返回实际保存错误：交换继续待回执、没有正式回执、
//     不新增回执审计、不留临时文件。
//
// 保存恢复并重开后，两份已完成回执（含带首尾空格的拒绝原因）与登记时间完整
// 保留、审计顺序不变；待回执交换在此刻首次登记才真正成功，失败尝试既不成为
// 成功依据也不产生第二条回执登记事件。
func TestRepeatReceiptConfirmsWithoutSave(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	t.Cleanup(func() { _ = s.Close() })

	p, err := s.RegisterPatient(doc, "回执重复确认患者")
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
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// 三份交换：一份接受、一份拒绝、一份保持待回执（用于首次登记边界）。
	clk.t = clk.t.Add(time.Hour)
	create := func(req string) Exchange {
		t.Helper()
		x, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rd.ID, ro.ID}, req)
		if err != nil {
			t.Fatal(err)
		}
		if x.Status != ExchangePending || x.Receipt != nil {
			t.Fatalf("fresh exchange must be pending without receipt: %+v", x)
		}
		return x
	}
	xA := create("req-rcpt-accept")
	xR := create("req-rcpt-reject")
	xP := create("req-rcpt-pending")

	// ---- 首次成功登记（保存条件正常） ----

	clk.t = clk.t.Add(time.Hour)
	acceptAt := clk.t
	acceptConf, err := s.SubmitReceipt(rcv, xA.ID, xA.Digest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	if acceptConf != (ReceiptConfirmation{
		ExchangeID:   xA.ID,
		Status:       ExchangeAccepted,
		Outcome:      ReceiptAccepted,
		RegisteredAt: acceptAt,
	}) {
		t.Fatalf("bad accept confirmation: %+v", acceptConf)
	}

	clk.t = clk.t.Add(time.Hour)
	rejectAt := clk.t
	// 非空白文字与首尾空格都必须原样保留为正式原因。
	rejectReason := "  原拒绝原因：材料不完整  "
	rejectConf, err := s.SubmitReceipt(rcv, xR.ID, xR.Digest, ReceiptRejected, rejectReason)
	if err != nil {
		t.Fatalf("first reject: %v", err)
	}
	if rejectConf != (ReceiptConfirmation{
		ExchangeID:   xR.ID,
		Status:       ExchangeRejected,
		Outcome:      ReceiptRejected,
		RegisteredAt: rejectAt,
	}) {
		t.Fatalf("bad reject confirmation: %+v", rejectConf)
	}

	// 提交前（保存条件正常）的正式基线：内部视图、交换列表与完整审计序列。
	acceptSnap, err := s.GetExchange(doc, p.ID, xA.ID)
	if err != nil {
		t.Fatal(err)
	}
	acceptWant := snapshotExchange(acceptSnap)
	if acceptWant.Status != ExchangeAccepted || acceptWant.Receipt == nil ||
		acceptWant.Receipt.Outcome != ReceiptAccepted || acceptWant.Receipt.Reason != "" ||
		!acceptWant.Receipt.RegisteredAt.Equal(acceptAt) {
		t.Fatalf("baseline accepted receipt wrong: %+v", acceptWant)
	}
	rejectSnap, err := s.GetExchange(doc, p.ID, xR.ID)
	if err != nil {
		t.Fatal(err)
	}
	rejectWant := snapshotExchange(rejectSnap)
	if rejectWant.Status != ExchangeRejected || rejectWant.Receipt == nil ||
		rejectWant.Receipt.Outcome != ReceiptRejected || rejectWant.Receipt.Reason != rejectReason ||
		!rejectWant.Receipt.RegisteredAt.Equal(rejectAt) {
		t.Fatalf("baseline rejected receipt wrong: %+v", rejectWant)
	}
	pendingWant := snapshotExchange(xP)
	listBefore, err := s.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listBefore) != 3 {
		t.Fatalf("baseline exchange count = %d, want 3", len(listBefore))
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore)[ActionReceipted]; got != 2 {
		t.Fatalf("baseline receipt events = %d, want 2", got)
	}

	// assertUnchanged 断言：保存不可用期间的所有重交与冲突尝试之后，三份交换
	// 的正式状态、回执（含登记时间与带空格原因）、包内容与摘要、交换列表以及
	// 完整审计序列（内容与顺序）都保持首次登记后的基线。
	assertUnchanged := func(label string) {
		t.Helper()

		gotA, err := s.GetExchange(doc, p.ID, xA.ID)
		if err != nil {
			t.Fatalf("%s: get accepted exchange: %v", label, err)
		}
		assertExchangeMatches(t, gotA, acceptWant, label+": accepted exchange")
		if recomputed := packageDigest(gotA.Package); recomputed != gotA.Digest {
			t.Fatalf("%s: accepted package/digest altered: digest=%q recomputed=%q",
				label, gotA.Digest, recomputed)
		}

		gotR, err := s.GetExchange(doc, p.ID, xR.ID)
		if err != nil {
			t.Fatalf("%s: get rejected exchange: %v", label, err)
		}
		assertExchangeMatches(t, gotR, rejectWant, label+": rejected exchange")
		if gotR.Receipt.Reason != rejectReason {
			t.Fatalf("%s: official reason altered: got %q, want %q",
				label, gotR.Receipt.Reason, rejectReason)
		}
		if recomputed := packageDigest(gotR.Package); recomputed != gotR.Digest {
			t.Fatalf("%s: rejected package/digest altered: digest=%q recomputed=%q",
				label, gotR.Digest, recomputed)
		}

		gotP, err := s.GetExchange(doc, p.ID, xP.ID)
		if err != nil {
			t.Fatalf("%s: get pending exchange: %v", label, err)
		}
		assertExchangeMatches(t, gotP, pendingWant, label+": pending exchange")
		if gotP.Status != ExchangePending || gotP.Receipt != nil {
			t.Fatalf("%s: pending exchange gained a receipt: %+v", label, gotP)
		}

		lst, err := s.ListExchanges(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list exchanges: %v", label, err)
		}
		if !reflect.DeepEqual(lst, listBefore) {
			t.Fatalf("%s: exchange list changed:\nbefore: %+v\nafter:  %+v", label, listBefore, lst)
		}

		audit, err := s.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		if got := auditActions(audit)[ActionReceipted]; got != 2 {
			t.Fatalf("%s: receipt events = %d, want 2 (no second registration)", label, got)
		}
	}

	// ---- 令本地保存条件暂时不可用 ----
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataPath) })

	// 所有重交都晚于首次登记：确认中的登记时间必须仍是首次时间。
	later := rejectAt.Add(2 * time.Hour)
	clk.t = later

	// 接受回执重交（不带原因）：原确认，且不尝试写盘。
	conf, err := s.SubmitReceipt(rcv, xA.ID, xA.Digest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("repeat accept while save unavailable: %v", err)
	}
	if conf != acceptConf {
		t.Fatalf("repeat accept confirmation = %+v, want original %+v", conf, acceptConf)
	}
	if conf.RegisteredAt.Equal(later) || !conf.RegisteredAt.Equal(acceptAt) {
		t.Fatalf("repeat accept must not restamp registration: got %v, want %v (submit at %v)",
			conf.RegisteredAt, acceptAt, later)
	}

	// 接受不保存原因：首次提交与重交附带不同原因文字，仍确认同一个接受结果。
	for _, reason := range []string{"重交时附带的原因甲", "另一段完全不同的文字乙"} {
		conf, err = s.SubmitReceipt(rcv, xA.ID, xA.Digest, ReceiptAccepted, reason)
		if err != nil {
			t.Fatalf("repeat accept carrying reason %q: %v", reason, err)
		}
		if conf != acceptConf {
			t.Fatalf("accept with reason %q changed confirmation: %+v vs %+v", reason, conf, acceptConf)
		}
	}

	// 拒绝回执重交：结果与原因（含首尾空格）逐字一致才确认原结果。
	conf, err = s.SubmitReceipt(rcv, xR.ID, xR.Digest, ReceiptRejected, rejectReason)
	if err != nil {
		t.Fatalf("repeat reject with identical reason: %v", err)
	}
	if conf != rejectConf {
		t.Fatalf("repeat reject confirmation = %+v, want original %+v", conf, rejectConf)
	}
	if !conf.RegisteredAt.Equal(rejectAt) {
		t.Fatalf("repeat reject restamped registration: got %v, want %v", conf.RegisteredAt, rejectAt)
	}

	// 内部使用者仍看到原来的正式回执、原包内容与摘要；审计不新增第二条事件。
	assertUnchanged("after idempotent confirmations during save failure")

	// ---- 冲突在无法保存期间仍按业务规则返回 ErrConflict，不被保存错误替代 ----

	mustConflict := func(label string, exchangeID ID, digest, outcome, reason string) {
		t.Helper()
		_, err := s.SubmitReceipt(rcv, exchangeID, digest, outcome, reason)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: err = %v, want ErrConflict (business rule must outrank save failure)",
				label, err)
		}
	}

	// 拒绝原因换成另一段非空白文字：冲突，不得覆盖正式原因或登记时间。
	mustConflict("reject with different reason", xR.ID, xR.Digest, ReceiptRejected, "另一段非空白原因")
	// 首尾空格是正式原因的一部分：去掉空格也算不同原因。
	mustConflict("reject with trimmed reason", xR.ID, xR.Digest, ReceiptRejected, "原拒绝原因：材料不完整")
	// 已保存的接受改成带非空白原因的拒绝：冲突。
	mustConflict("accept changed to reject", xA.ID, xA.Digest, ReceiptRejected, "试图改判为拒绝")
	// 已保存的拒绝改成接受：冲突。
	mustConflict("reject changed to accept", xR.ID, xR.Digest, ReceiptAccepted, "")
	// 其他参数合法但摘要不符：不能因为交换已有回执而得到成功确认。
	mustConflict("accepted exchange with mismatched digest", xA.ID, "digest-does-not-match", ReceiptAccepted, "")
	mustConflict("rejected exchange with mismatched digest", xR.ID, "digest-does-not-match",
		ReceiptRejected, rejectReason)

	assertUnchanged("after conflicting resubmissions during save failure")

	// 其余既有错误含义同样不被保存错误替代：参数非法与身份/存在性校验在落盘
	// 之前完成，保存不可用不改变这些结论。
	if _, err := s.SubmitReceipt(rcv, xP.ID, xP.Digest, ReceiptRejected, "   "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank reject reason while save blocked: err = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.SubmitReceipt(rcv, xA.ID, xA.Digest, "maybe", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad outcome while save blocked: err = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.SubmitReceipt(rcvB, xA.ID, xA.Digest, ReceiptAccepted, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other receiver while save blocked: err = %v, want ErrAccessDenied", err)
	}
	if _, err := s.SubmitReceipt(rcv, "exch_missing", xA.Digest, ReceiptAccepted, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("missing exchange while save blocked: err = %v, want ErrAccessDenied", err)
	}
	assertUnchanged("after pre-persist validation failures during save failure")

	// ---- 首次登记边界：待回执交换在保存不可用时必须返回实际保存错误 ----
	//
	// 即使接收方、摘要与回执内容全部合法，接受与拒绝两种首次登记都不能借
	// “确认”名义成功；交换继续待回执、没有正式回执、不新增回执审计。
	for _, tc := range []struct {
		name    string
		outcome string
		reason  string
	}{
		{"first accept", ReceiptAccepted, ""},
		{"first reject", ReceiptRejected, "待回执交换的拒绝原因"},
	} {
		if _, err := s.SubmitReceipt(rcv, xP.ID, xP.Digest, tc.outcome, tc.reason); err == nil {
			t.Fatalf("%s must fail when local save is unavailable", tc.name)
		} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidArgument) ||
			errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) ||
			errors.Is(err, ErrNotFound) || errors.Is(err, ErrMismatchedPatient) {
			t.Fatalf("%s: save failure surfaced as a business error: %v", tc.name, err)
		}
		// 失败不能留下半截写入的临时文件。
		if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
			t.Fatalf("%s left a temp file: %v", tc.name, statErr)
		}
	}
	assertUnchanged("after failed first registrations during save failure")

	// 接收方取包：待回执交换的状态与摘要仍是首次创建时的值，失败尝试不留痕。
	delivery, err := s.FetchPackage(rcv, xP.ID)
	if err != nil {
		t.Fatalf("fetch pending exchange: %v", err)
	}
	if delivery.Status != ExchangePending || delivery.Digest != xP.Digest {
		t.Fatalf("pending delivery altered: %+v", delivery)
	}

	// ---- 恢复保存条件并重开：正式回执原样，待回执交换仍可首次登记 ----
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dataPath); err != nil {
		t.Fatalf("unblock data path: %v", err)
	}
	if err := os.Rename(backupPath, dataPath); err != nil {
		t.Fatalf("restore data file: %v", err)
	}

	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	gotA, err := s2.GetExchange(doc, p.ID, xA.ID)
	if err != nil {
		t.Fatalf("accepted exchange after reopen: %v", err)
	}
	assertExchangeMatches(t, gotA, acceptWant, "accepted exchange after reopen")
	gotR, err := s2.GetExchange(doc, p.ID, xR.ID)
	if err != nil {
		t.Fatalf("rejected exchange after reopen: %v", err)
	}
	assertExchangeMatches(t, gotR, rejectWant, "rejected exchange after reopen")
	if gotR.Receipt.Reason != rejectReason {
		t.Fatalf("rejected reason after reopen = %q, want %q", gotR.Receipt.Reason, rejectReason)
	}
	gotP, err := s2.GetExchange(doc, p.ID, xP.ID)
	if err != nil {
		t.Fatalf("pending exchange after reopen: %v", err)
	}
	assertExchangeMatches(t, gotP, pendingWant, "pending exchange after reopen")

	// 重开后审计内容与顺序保持：xA、xR 各一条，xP 没有，没有第二条登记事件。
	auditReopened, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditReopened, auditBefore) {
		t.Fatalf("audit after reopen changed:\nbefore: %+v\nafter:  %+v", auditBefore, auditReopened)
	}

	// 重开后的重复确认仍返回原确认（不落盘路径跨重开保持）。
	confA, err := s2.SubmitReceipt(rcv, xA.ID, xA.Digest, ReceiptAccepted, "恢复后重交仍不保存原因")
	if err != nil {
		t.Fatalf("repeat accept after reopen: %v", err)
	}
	if confA != acceptConf {
		t.Fatalf("repeat accept after reopen = %+v, want %+v", confA, acceptConf)
	}
	confR, err := s2.SubmitReceipt(rcv, xR.ID, xR.Digest, ReceiptRejected, rejectReason)
	if err != nil {
		t.Fatalf("repeat reject after reopen: %v", err)
	}
	if confR != rejectConf {
		t.Fatalf("repeat reject after reopen = %+v, want %+v", confR, rejectConf)
	}
	if got := len(mustAudit(t, s2, p.ID)); got != len(auditBefore) {
		t.Fatalf("confirmations after reopen added audit events: %d, want %d",
			got, len(auditBefore))
	}

	// 保存恢复后，待回执交换的首次登记才真正成功：采用本次时间，只追加一条
	// 新的回执登记事件；此前保存不可用时的失败尝试不构成成功依据。
	clk.t = clk.t.Add(time.Hour)
	registeredPendingAt := clk.t
	confP, err := s2.SubmitReceipt(rcv, xP.ID, xP.Digest, ReceiptRejected, "恢复后才登记的拒绝原因")
	if err != nil {
		t.Fatalf("first registration after recovery: %v", err)
	}
	if confP.ExchangeID != xP.ID || confP.Status != ExchangeRejected ||
		confP.Outcome != ReceiptRejected || !confP.RegisteredAt.Equal(registeredPendingAt) {
		t.Fatalf("bad confirmation for previously pending exchange: %+v", confP)
	}

	doneP, err := s2.GetExchange(doc, p.ID, xP.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doneP.Status != ExchangeRejected || doneP.Receipt == nil ||
		doneP.Receipt.Outcome != ReceiptRejected || doneP.Receipt.Reason != "恢复后才登记的拒绝原因" ||
		!doneP.Receipt.RegisteredAt.Equal(registeredPendingAt) {
		t.Fatalf("pending exchange receipt after recovery wrong: %+v", doneP)
	}
	if doneP.ID != xP.ID || doneP.Digest != xP.Digest {
		t.Fatalf("pending exchange identity or digest changed: %+v", doneP)
	}

	auditAfter, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditAfter) != len(auditBefore)+1 ||
		!reflect.DeepEqual(auditAfter[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after genuine registration:\nbefore: %+v\nafter:  %+v", auditBefore, auditAfter)
	}
	last := auditAfter[len(auditAfter)-1]
	if last.Action != ActionReceipted || last.ObjectType != "exchange" ||
		last.ObjectID != xP.ID || last.ActorID != rcv.ID || last.PatientID != p.ID ||
		!last.OccurredAt.Equal(registeredPendingAt) {
		t.Fatalf("unexpected new receipt event: %+v", last)
	}
	if got := auditActions(auditAfter)[ActionReceipted]; got != 3 {
		t.Fatalf("receipt events = %d, want exactly 3", got)
	}
}
