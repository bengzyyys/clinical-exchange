package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestReceiptSaveFailureKeepsExchangeAndAudit 覆盖“指定接收方凭交换标识与正确
// 摘要提交合法拒绝回执（原因非空），身份、摘要、状态等业务条件全部满足，但
// 本地保存失败”的回执登记场景。
//
// 失败必须以普通错误返回（不能伪装成摘要冲突、参数非法或无权操作），且交换
// 状态、回执与审计不能只有一部分成功：内部使用者看到的仍是待回执且无正式
// 回执的交换，失败尝试的原因与登记时间不留痕迹，既有审计事件完整保留、不
// 新增回执事件；交换标识、原包内容与摘要保持原样。保存条件恢复后，接收方
// 可用不同的非空原因成功登记（失败尝试不建立回执，不得报已有回执冲突），
// 正式原因与登记时间以成功这次为准，只新增一条回执审计。此后改变原因重交
// 仍返回 ErrConflict，相同回执重交返回原确认——既有公开行为不变。
func TestReceiptSaveFailureKeepsExchangeAndAudit(t *testing.T) {
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
	rd, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断v1内容")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, rd.ID); err != nil {
		t.Fatal(err)
	}
	ro, err := s.CreateDraft(doc, p.ID, e.ID, Order, "医嘱v1内容")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, ro.ID); err != nil {
		t.Fatal(err)
	}
	a, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{
			{EncounterID: e.ID, Category: Diagnosis},
			{EncounterID: e.ID, Category: Order},
		},
		clk.t.Add(-time.Minute), clk.t.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// 已经成功创建、仍在等待回执的交换：登记回执的起点。
	clk.t = clk.t.Add(time.Hour)
	x, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rd.ID, ro.ID}, "req-receipt-save")
	if err != nil {
		t.Fatal(err)
	}
	if x.Status != ExchangePending || x.Receipt != nil {
		t.Fatalf("baseline exchange must be pending without receipt: %+v", x)
	}

	// 提交前的正式基线：内部视图中的交换、交换列表、审计与接收方取包结果。
	xBefore := snapshotExchange(x)
	gotBefore, err := s.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, gotBefore, xBefore, "baseline exchange")
	listBefore, err := s.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore)[ActionReceipted]; got != 0 {
		t.Fatalf("baseline receipt events = %d, want 0", got)
	}
	deliveryBefore, err := s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deliveryBefore.Digest != x.Digest || deliveryBefore.Status != ExchangePending {
		t.Fatalf("unexpected baseline delivery: %+v", deliveryBefore)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），交换仍是待回执且无
	// 正式回执——失败尝试的原因与登记时间没有留下半条回执；审计、交换列表
	// 与包内容/摘要全部维持提交前状态。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		got, err := st.GetExchange(doc, p.ID, x.ID)
		if err != nil {
			t.Fatalf("%s: get exchange: %v", label, err)
		}
		assertExchangeMatches(t, got, xBefore, label+": exchange")
		if got.Status != ExchangePending || got.Receipt != nil {
			t.Fatalf("%s: failed attempt left a partial receipt: status=%q receipt=%+v",
				label, got.Status, got.Receipt)
		}

		xs, err := st.ListExchanges(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list exchanges: %v", label, err)
		}
		if !reflect.DeepEqual(xs, listBefore) {
			t.Fatalf("%s: exchange list changed:\nbefore: %+v\nafter:  %+v", label, listBefore, xs)
		}

		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		if got := auditActions(audit)[ActionReceipted]; got != 0 {
			t.Fatalf("%s: failed attempt added %d receipt events", label, got)
		}

		// 接收方取包仍得到原摘要与原包内容：登记失败没有重新生成包。
		d, err := st.FetchPackage(rcv, x.ID)
		if err != nil {
			t.Fatalf("%s: fetch package: %v", label, err)
		}
		assertDeliveryMatches(t, d, deliveryBefore, label+": delivery")
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、摘要或原因校验无关。原数据文件先挪到旁边，事后原样还原。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 指定接收方、正确摘要、非空拒绝原因——全部业务条件满足，仅保存失败：
	// 必须明确报错，且不能伪装成摘要冲突、参数非法或无权操作等业务结果。
	clk.t = clk.t.Add(time.Hour)
	failedAttemptAt := clk.t
	if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "失败尝试的拒绝原因"); err == nil {
		t.Fatal("receipt submission must fail when local save fails")
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) ||
		errors.Is(err, ErrNotFound) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：交换仍待回执、无正式回执，审计与包全部维持提交前状态。
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
	// 重开后仍是提交前的正式数据：失败尝试没有变成回执或事件。
	checkIntact("after reopen", s2)

	// 保存条件恢复后：同一接收方对同一份交换、以与失败尝试不同的非空原因
	// 重新登记。失败尝试没有建立回执，不得返回已有回执冲突。
	clk.t = clk.t.Add(time.Hour)
	succeededAt := clk.t
	if succeededAt.Equal(failedAttemptAt) {
		t.Fatal("test clock must distinguish failed attempt from successful registration")
	}
	conf, err := s2.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "重试成功的拒绝原因")
	if err != nil {
		t.Fatalf("retry with a different reason after recovery: %v", err)
	}
	if conf.ExchangeID != x.ID || conf.Status != ExchangeRejected || conf.Outcome != ReceiptRejected {
		t.Fatalf("bad confirmation: %+v", conf)
	}
	// 登记时间采用成功登记时的时间，而不是失败尝试的时间。
	if !conf.RegisteredAt.Equal(succeededAt) {
		t.Fatalf("registered at %v != successful attempt time %v (failed attempt was %v)",
			conf.RegisteredAt, succeededAt, failedAttemptAt)
	}

	// 内部视图：交换转为已拒绝，正式原因采用本次提交的文字；标识、原包
	// 内容与摘要保持原样，没有因登记重新生成包。
	want := snapshotExchange(xBefore)
	want.Status = ExchangeRejected
	want.Receipt = &Receipt{Outcome: ReceiptRejected, Reason: "重试成功的拒绝原因", RegisteredAt: succeededAt}
	got, err := s2.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, got, want, "exchange after successful retry")
	if got.Receipt.Reason == "失败尝试的拒绝原因" {
		t.Fatalf("failed attempt's reason became the official receipt: %+v", got.Receipt)
	}
	if got.ID != x.ID || got.Digest != x.Digest {
		t.Fatalf("exchange identity or digest changed by receipt: %+v", got)
	}
	if recomputed := packageDigest(got.Package); recomputed != got.Digest {
		t.Fatalf("package regenerated by receipt: digest %q, recomputed %q", got.Digest, recomputed)
	}

	// 审计：相对提交前基线只新增一条由该接收方针对该交换的回执事件，
	// 既有事件（含交换创建事件）保持顺序和内容。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after retry:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionReceipted || last.ObjectType != "exchange" ||
		last.ObjectID != x.ID || last.ActorID != rcv.ID ||
		last.PatientID != p.ID || !last.OccurredAt.Equal(succeededAt) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if got := auditActions(audit)[ActionReceipted]; got != 1 {
		t.Fatalf("receipt events = %d, want exactly 1 (failed attempt must not add one)", got)
	}

	// 交换列表仍只有这一份交换，内容与内部视图一致。
	xs, err := s2.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 {
		t.Fatalf("exchange count after retry = %d, want 1", len(xs))
	}
	assertExchangeMatches(t, xs[0], want, "listed exchange after retry")

	// 取包视图：状态转为已拒绝，摘要与包内容不变。
	d, err := s2.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != ExchangeRejected || d.Digest != x.Digest {
		t.Fatalf("delivery after retry: %+v", d)
	}
	assertDeliveryMatches(t, d, PackageDelivery{
		ExchangeID: deliveryBefore.ExchangeID,
		Status:     ExchangeRejected,
		Digest:     deliveryBefore.Digest,
		CreatedAt:  deliveryBefore.CreatedAt,
		Package:    deliveryBefore.Package,
	}, "delivery after retry")

	// 已有回执不可覆盖：改变原因再次提交返回 ErrConflict，正式回执与审计不变。
	clk.t = clk.t.Add(time.Hour)
	if _, err := s2.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "试图覆盖的拒绝原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrite attempt err = %v, want ErrConflict", err)
	}
	got, err = s2.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, got, want, "exchange after overwrite attempt")
	if audit2 := mustAudit(t, s2, p.ID); !reflect.DeepEqual(audit2, audit) {
		t.Fatalf("overwrite attempt changed audit:\nbefore: %+v\nafter:  %+v", audit, audit2)
	}

	// 既有公开行为保留：相同回执重交返回原确认，不新增审计、不改状态。
	conf2, err := s2.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "重试成功的拒绝原因")
	if err != nil {
		t.Fatalf("idempotent resubmit: %v", err)
	}
	if conf2.ExchangeID != conf.ExchangeID || conf2.Status != conf.Status ||
		conf2.Outcome != conf.Outcome || !conf2.RegisteredAt.Equal(conf.RegisteredAt) {
		t.Fatalf("idempotent resubmit returned %+v, want original confirmation %+v", conf2, conf)
	}
	if n := len(mustAudit(t, s2, p.ID)); n != len(audit) {
		t.Fatalf("idempotent resubmit added audit events: %d, want %d", n, len(audit))
	}

	// 关闭后从同一数据位置重新打开：已拒绝状态、正式原因与登记时间完整
	// 可见，失败尝试不作为另一份回执或事件出现。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })

	got, err = s3.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatalf("get exchange after reopen: %v", err)
	}
	assertExchangeMatches(t, got, want, "exchange after reopen")

	audit3 := mustAudit(t, s3, p.ID)
	if !reflect.DeepEqual(audit3, audit) {
		t.Fatalf("audit after reopen:\nbefore: %+v\nafter:  %+v", audit, audit3)
	}
	var receiptEvents []AuditEvent
	for _, ev := range audit3 {
		if ev.Action == ActionReceipted {
			receiptEvents = append(receiptEvents, ev)
		}
	}
	if len(receiptEvents) != 1 || receiptEvents[0].ObjectID != x.ID ||
		receiptEvents[0].ActorID != rcv.ID || !receiptEvents[0].OccurredAt.Equal(succeededAt) {
		t.Fatalf("receipt events after reopen = %+v, want exactly one for %q at %v",
			receiptEvents, x.ID, succeededAt)
	}
}
