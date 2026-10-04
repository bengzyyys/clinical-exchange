package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestReceiptSaveFailureKeepsExchangePendingAndAllowsFreshReject 覆盖“指定接收方
// 凭正确交换标识与包摘要、携带非空拒绝原因登记拒绝回执——身份、摘要、参数等
// 全部业务条件都满足，但回执无法保存到本地”的场景。
//
// 失败必须以普通保存错误返回（不能伪装成摘要冲突 ErrConflict、参数不合法
// ErrInvalidArgument 或无权操作 ErrAccessDenied），且交换状态、回执内容与审计
// 三者要么全部生效、要么全部不动：失败后内部使用者看到的仍是待回执交换、没有
// 正式回执（不含失败尝试的原因与登记时间），患者原有审计事件（含另一份交换的
// 回执事件）完整保留、顺序不变，交换标识、冻结包与摘要原样保留，不重新打包。
//
// 保存条件恢复后，接收方可对同一份交换用与失败尝试不同的非空原因继续登记：
// 前一次没有成功建立回执，不得被判为已有回执冲突。成功回执必须对应本次提交
// （本次原因、成功登记时刻，而非失败尝试的时刻），且只新增一条由该接收方针对
// 该交换产生的回执审计。成功之后改变原因再次提交仍返回 ErrConflict，正式原因
// 与登记时间不变、不增加审计——失败尝试与成功回执必须能明确区分。
func TestReceiptSaveFailureKeepsExchangePendingAndAllowsFreshReject(t *testing.T) {
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
	p, err := s.RegisterPatient(doc, "回执保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 两条已生效记录：诊断已更正到第 2 版，医嘱为第 1 版。
	rd, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断v1内容")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, rd.ID); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	diagV2, err := s.CorrectRecord(doc, rd.ID, 1, "诊断v2内容", "录入笔误")
	if err != nil {
		t.Fatal(err)
	}
	ro, err := s.CreateDraft(doc, p.ID, e.ID, Order, "医嘱v1内容")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, ro.ID); err != nil {
		t.Fatal(err)
	}

	// 接收方持有当前有效授权，覆盖这两条记录。
	authStart := clk.t.Add(-time.Minute)
	authEnd := clk.t.Add(48 * time.Hour)
	a, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{
			{EncounterID: e.ID, Category: Diagnosis},
			{EncounterID: e.ID, Category: Order},
		},
		authStart, authEnd)
	if err != nil {
		t.Fatal(err)
	}

	// 失败尝试之前，患者已有一份成功保存且已登记拒绝回执的交换，用于验证
	// 失败不会波及既有交换及其回执与审计。
	clk.t = clk.t.Add(time.Hour)
	xPrior, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rd.ID}, "req-prior")
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	if _, err := s.SubmitReceipt(rcv, xPrior.ID, xPrior.Digest, ReceiptRejected, "既有拒绝原因"); err != nil {
		t.Fatal(err)
	}
	priorSnap, err := s.GetExchange(doc, p.ID, xPrior.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 本次登记的对象：一份已成功创建、仍在等待回执的交换。
	clk.t = clk.t.Add(time.Hour)
	x, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rd.ID, ro.ID}, "req-receipt-save")
	if err != nil {
		t.Fatal(err)
	}
	if x.Status != ExchangePending || x.Receipt != nil {
		t.Fatalf("target exchange should start pending without receipt: %+v", x)
	}
	// 包在创建时固化当前生效版本：诊断为 v2、医嘱为 v1，作为“原包内容不被
	// 登记失败改写”的核对基准。
	targetByID := recordsByID(x.Package.Records)
	if pr := targetByID[rd.ID]; pr.VersionID != diagV2.ID || pr.Version != 2 ||
		pr.Content != "诊断v2内容" {
		t.Fatalf("target package diagnosis wrong: %+v", pr)
	}
	if pr := targetByID[ro.ID]; pr.Version != 1 || pr.Content != "医嘱v1内容" {
		t.Fatalf("target package order wrong: %+v", pr)
	}

	// 提交前的正式基线：交换视图、交换列表、完整审计与接收方取包结果。
	targetSnap, err := s.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	exchangesBefore, err := s.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exchangesBefore) != 2 || exchangesBefore[0].ID != xPrior.ID || exchangesBefore[1].ID != x.ID {
		t.Fatalf("unexpected baseline exchange list: %+v", exchangesBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	receiptsBefore := auditActions(auditBefore)[ActionReceipted]
	if receiptsBefore != 1 {
		t.Fatalf("baseline receipt audits = %d, want 1 (prior exchange)", receiptsBefore)
	}
	delBefore, err := s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if delBefore.Status != ExchangePending || delBefore.Digest != x.Digest {
		t.Fatalf("unexpected baseline delivery: %+v", delBefore)
	}

	failedReason := "保存失败时提交的拒绝原因"

	// checkIntact 断言：失败尝试之后（以及重开之后），交换状态、回执内容与
	// 审计全部维持提交前状态——没有半条回执，也没有半条审计。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		// 内部使用者查看：目标交换仍是待回执，没有正式回执，因此也不会留下
		// 失败尝试的原因或登记时间。
		got, err := st.GetExchange(doc, p.ID, x.ID)
		if err != nil {
			t.Fatalf("%s: get target exchange: %v", label, err)
		}
		if got.Status != ExchangePending {
			t.Fatalf("%s: status = %q, want %q", label, got.Status, ExchangePending)
		}
		if got.Receipt != nil {
			t.Fatalf("%s: failed attempt left a receipt: %+v", label, got.Receipt)
		}
		// 标识、冻结包（含每条记录的版本与内容）、摘要、创建时间全部原样，
		// 没有因登记失败重新生成包。
		assertExchangeMatches(t, got, targetSnap, label+": target exchange")
		if recomputed := packageDigest(got.Package); recomputed != got.Digest {
			t.Fatalf("%s: digest no longer matches package: got %q, recomputed %q",
				label, got.Digest, recomputed)
		}

		// 既有交换及其已登记回执原样保留。
		gotPrior, err := st.GetExchange(doc, p.ID, xPrior.ID)
		if err != nil {
			t.Fatalf("%s: get prior exchange: %v", label, err)
		}
		assertExchangeMatches(t, gotPrior, priorSnap, label+": prior exchange")

		// 正式交换列表逐项与提交前一致。
		xs, err := st.ListExchanges(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list exchanges: %v", label, err)
		}
		if !reflect.DeepEqual(xs, exchangesBefore) {
			t.Fatalf("%s: exchange list changed:\nbefore: %+v\nafter:  %+v", label, exchangesBefore, xs)
		}

		// 审计事件（顺序与内容）完整保留：不新增回执登记事件，也不出现针对
		// 目标交换的任何事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		if got := auditActions(audit)[ActionReceipted]; got != receiptsBefore {
			t.Fatalf("%s: receipt_registered events = %d, want %d", label, got, receiptsBefore)
		}
		for _, ev := range audit {
			if ev.ObjectType == "exchange" && ev.ObjectID == x.ID && ev.Action == ActionReceipted {
				t.Fatalf("%s: receipt audit for failed attempt exists: %+v", label, ev)
			}
		}

		// 接收方取包仍看到待回执与原摘要，公开入口行为不变。
		del, err := st.FetchPackage(rcv, x.ID)
		if err != nil {
			t.Fatalf("%s: fetch package: %v", label, err)
		}
		if del.Status != ExchangePending || del.Digest != x.Digest {
			t.Fatalf("%s: delivery changed: %+v", label, del)
		}
		assertDeliveryMatches(t, del, delBefore, label+": delivery")
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、摘要或参数校验无关。原数据文件先挪到旁边，事后原样还原。
	failedAt := clk.t.Add(time.Hour)
	clk.t = failedAt
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 指定接收方、正确交换标识与摘要、有效非空拒绝原因：全部业务条件满足，
	// 仅本地保存失败。必须明确报保存错误，不能伪装成摘要冲突、参数不合法或
	// 无权操作。（与既有约定一致：非 nil 错误时返回值无意义，调用方必须以
	// 错误为准；下面的正式状态断言保证它没有成为一份正式回执。）
	if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, failedReason); err == nil {
		t.Fatal("receipt registration must fail when local save fails")
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrAccessDenied) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：状态、回执与审计全部维持提交前状态。
	checkIntact("after failed save", s)

	// 关闭后从原数据位置重新打开：先恢复保存条件（还原原数据文件）。
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
	// 重开后仍是操作前的正式数据：待回执、无回执、无失败尝试的审计。
	checkIntact("after reopen", s2)

	// 保存条件恢复后：对同一份交换继续登记拒绝回执。允许使用与失败尝试不同
	// 的非空原因——前一次没有成功建立回执，不得返回已有回执冲突。
	successReason := "保存恢复后登记的拒绝原因"
	successAt := failedAt.Add(2 * time.Hour)
	clk.t = successAt
	conf, err := s2.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, successReason)
	if err != nil {
		t.Fatalf("fresh rejection after save recovered must not conflict: %v", err)
	}
	if conf.ExchangeID != x.ID || conf.Status != ExchangeRejected || conf.Outcome != ReceiptRejected {
		t.Fatalf("bad confirmation: %+v", conf)
	}
	// 登记时间采用成功登记的时刻，而不是失败尝试的时刻。
	if !conf.RegisteredAt.Equal(successAt) {
		t.Fatalf("confirmation registered at %v, want %v (not failed attempt %v)",
			conf.RegisteredAt, successAt, failedAt)
	}

	// 内部使用者视图：已拒绝状态、完整原因与成功登记时间；失败尝试的原因与
	// 时间都不存在。交换标识、原包内容与摘要保持原样。
	got, err := s2.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ExchangeRejected {
		t.Fatalf("status = %q, want %q", got.Status, ExchangeRejected)
	}
	if got.Receipt == nil {
		t.Fatal("rejected exchange must carry the formal receipt")
	}
	if got.Receipt.Outcome != ReceiptRejected || got.Receipt.Reason != successReason {
		t.Fatalf("formal receipt does not match this submission: %+v", got.Receipt)
	}
	if got.Receipt.Reason == failedReason {
		t.Fatalf("formal reason is the failed attempt's reason %q", failedReason)
	}
	if !got.Receipt.RegisteredAt.Equal(successAt) {
		t.Fatalf("formal receipt registered at %v, want %v (failed attempt was %v)",
			got.Receipt.RegisteredAt, successAt, failedAt)
	}
	if got.ID != targetSnap.ID || got.Digest != targetSnap.Digest ||
		!got.CreatedAt.Equal(targetSnap.CreatedAt) {
		t.Fatalf("exchange identity changed after receipt:\n got=%+v\nwant=%+v", got, targetSnap)
	}
	assertExchangeMatches(t, got, Exchange{
		ID:              targetSnap.ID,
		PatientID:       targetSnap.PatientID,
		ReceiverID:      targetSnap.ReceiverID,
		AuthorizationID: targetSnap.AuthorizationID,
		RequestID:       targetSnap.RequestID,
		CreatorID:       targetSnap.CreatorID,
		Status:          ExchangeRejected,
		Digest:          targetSnap.Digest,
		Package:         targetSnap.Package,
		Receipt: &Receipt{
			Outcome:      ReceiptRejected,
			Reason:       successReason,
			RegisteredAt: successAt,
		},
		CreatedAt: targetSnap.CreatedAt,
	}, "exchange after successful registration")
	if recomputed := packageDigest(got.Package); recomputed != targetSnap.Digest {
		t.Fatalf("digest changed: got %q, recomputed %q", targetSnap.Digest, recomputed)
	}

	// 审计：相对失败前基线只在末尾新增一条由该接收方针对该交换产生的回执
	// 事件；原有事件（含既有交换的创建与回执事件）保持顺序和内容。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after successful registration:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionReceipted || last.ObjectType != "exchange" ||
		last.ObjectID != x.ID || last.ActorID != rcv.ID ||
		last.PatientID != p.ID || !last.OccurredAt.Equal(successAt) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if !last.OccurredAt.After(failedAt) {
		t.Fatalf("receipt audit time %v must be after failed attempt %v", last.OccurredAt, failedAt)
	}
	if got := auditActions(audit)[ActionReceipted]; got != receiptsBefore+1 {
		t.Fatalf("receipt_registered events = %d, want %d", got, receiptsBefore+1)
	}
	var targetReceiptEvents []AuditEvent
	for _, ev := range audit {
		if ev.ObjectType == "exchange" && ev.ObjectID == x.ID && ev.Action == ActionReceipted {
			targetReceiptEvents = append(targetReceiptEvents, ev)
		}
	}
	if len(targetReceiptEvents) != 1 {
		t.Fatalf("receipt audits for target exchange = %d, want 1: %+v",
			len(targetReceiptEvents), targetReceiptEvents)
	}

	// 接收方取包此时看到已拒绝状态，摘要仍是原包摘要。
	del, err := s2.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if del.Status != ExchangeRejected || del.Digest != x.Digest {
		t.Fatalf("delivery after registration: %+v", del)
	}

	// 已有回执不可覆盖：改变拒绝原因再次提交返回 ErrConflict，正式原因与
	// 登记时间不变，也不增加审计。
	clk.t = successAt.Add(time.Hour)
	if _, err := s2.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "成功以后又改的原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwriting receipt with changed reason err = %v, want ErrConflict", err)
	}
	got2, err := s2.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != ExchangeRejected || got2.Receipt == nil ||
		got2.Receipt.Reason != successReason || !got2.Receipt.RegisteredAt.Equal(successAt) {
		t.Fatalf("formal receipt changed after conflicting submit: %+v", got2.Receipt)
	}
	if evs, err := s2.AuditEvents(doc, p.ID); err != nil || len(evs) != len(audit) {
		t.Fatalf("conflicting submit changed audit count: %d -> %d (%v)",
			len(audit), len(evs), err)
	}

	// 相同回执（结果与原因均相同）重交仍返回原确认，不新增审计——公开入口
	// 及现有返回信息保持不变。
	confSame, err := s2.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, successReason)
	if err != nil {
		t.Fatalf("identical receipt resubmission: %v", err)
	}
	if confSame.Status != ExchangeRejected || confSame.Outcome != ReceiptRejected ||
		!confSame.RegisteredAt.Equal(successAt) {
		t.Fatalf("identical resubmission changed confirmation: %+v", confSame)
	}
	if evs, err := s2.AuditEvents(doc, p.ID); err != nil || len(evs) != len(audit) {
		t.Fatalf("identical resubmission changed audit count: %d -> %d (%v)",
			len(audit), len(evs), err)
	}

	// 关闭后从同一数据位置重新打开：正式回执与唯一一条回执审计完整可见，
	// 失败尝试不作为半条回执或事件出现。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })

	final, err := s3.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if final.Status != ExchangeRejected || final.Receipt == nil ||
		final.Receipt.Outcome != ReceiptRejected || final.Receipt.Reason != successReason ||
		!final.Receipt.RegisteredAt.Equal(successAt) {
		t.Fatalf("formal receipt after reopen wrong: %+v", final.Receipt)
	}
	if final.Digest != targetSnap.Digest || final.ID != targetSnap.ID {
		t.Fatalf("exchange identity changed after reopen: %+v", final)
	}
	finalAudit, err := s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalAudit) != len(auditBefore)+1 ||
		!reflect.DeepEqual(finalAudit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after reopen:\nbefore: %+v\nafter:  %+v", auditBefore, finalAudit)
	}
	var finalTargetReceiptEvents []AuditEvent
	for _, ev := range finalAudit {
		if ev.ObjectType == "exchange" && ev.ObjectID == x.ID && ev.Action == ActionReceipted {
			finalTargetReceiptEvents = append(finalTargetReceiptEvents, ev)
		}
	}
	if len(finalTargetReceiptEvents) != 1 ||
		!finalTargetReceiptEvents[0].OccurredAt.Equal(successAt) ||
		finalTargetReceiptEvents[0].ActorID != rcv.ID {
		t.Fatalf("receipt audits for target exchange after reopen wrong: %+v", finalTargetReceiptEvents)
	}
	// 既有交换的回执事件仍是最初那条，顺序与内容不变。
	finalPrior, err := s3.GetExchange(doc, p.ID, xPrior.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, finalPrior, priorSnap, "prior exchange after reopen")
}
