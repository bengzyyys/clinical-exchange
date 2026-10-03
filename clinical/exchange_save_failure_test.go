package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestExchangeCreateSaveFailureKeepsStateAndFreesRequestID 覆盖“身份有权操作、
// 患者未停用、绑定授权属于同一患者与接收方且当前有效、所选记录均为已生效并被
// 该授权自身覆盖、请求号尚未用过——全部业务条件都满足，但本地保存失败”的
// 交换创建场景。
//
// 失败必须以普通错误返回（不能伪装成 ErrConflict/ErrInvalidArgument/
// ErrAccessDenied），且正式交换列表、审计、临床记录当前版本与版本历史、
// 既有成功交换（标识、冻结包、摘要、回执）全部维持提交前状态；不允许出现
// 只有交换没有审计或只有审计没有交换的半成品。保存条件恢复后，失败的请求号
// 必须仍可使用；所选记录在失败后经过一次合法更正的，重试成功的包必须固化
// 重试时的当前版本与完整内容，而不是失败当时准备好的版本。关闭重开后同样
// 只见成功交换与其审计，失败尝试不留任何痕迹。
func TestExchangeCreateSaveFailureKeepsStateAndFreesRequestID(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "交换保存失败患者")
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

	// 限定授权：不借整类范围，明确选出这两条已生效记录；授权本身属于该
	// 患者与接收方，两条记录都由这同一条绑定授权自身覆盖。
	authStart := clk.t.Add(-time.Minute)
	authEnd := clk.t.Add(48 * time.Hour)
	a, err := s.GrantSelective(doc, p.ID, rcv.ID, nil,
		[]RecordSelection{
			{EncounterID: e.ID, Category: Diagnosis, RecordID: rd.ID},
			{EncounterID: e.ID, Category: Order, RecordID: ro.ID},
		},
		authStart, authEnd)
	if err != nil {
		t.Fatal(err)
	}

	// 失败尝试之前，患者已有一份成功保存的交换，并且接收方已登记拒绝回执。
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

	// 提交前的正式基线：交换列表、审计、完整档案与接收方读取结果。
	exchangesBefore, err := s.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	readBefore, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	createsBefore := auditActions(auditBefore)[ActionExchanged]

	// checkIntact 断言：失败尝试之后（以及重开之后），全部正式数据与提交前
	// 逐项一致——没有新交换、没有新事件，临床记录与既有交换都未被改动。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		xs, err := st.ListExchanges(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list exchanges: %v", label, err)
		}
		if !reflect.DeepEqual(xs, exchangesBefore) {
			t.Fatalf("%s: exchange list changed:\nbefore: %+v\nafter:  %+v", label, exchangesBefore, xs)
		}
		for _, x := range xs {
			if x.RequestID == "req-failed-save" {
				t.Fatalf("%s: failed attempt surfaced as an exchange: %+v", label, x)
			}
		}

		// 既有交换的标识、冻结包版本与内容、摘要、状态与已有回执原样保留。
		gotPrior, err := st.GetExchange(doc, p.ID, xPrior.ID)
		if err != nil {
			t.Fatalf("%s: get prior exchange: %v", label, err)
		}
		assertExchangeMatches(t, gotPrior, priorSnap, label+": prior exchange")

		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		if got := auditActions(audit)[ActionExchanged]; got != createsBefore {
			t.Fatalf("%s: exchange_created events = %d, want %d", label, got, createsBefore)
		}

		// 临床记录的当前版本、完整内容与版本历史不变。
		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		if !reflect.DeepEqual(chart.Records, chartBefore.Records) {
			t.Fatalf("%s: clinical records changed:\nbefore: %+v\nafter:  %+v",
				label, chartBefore.Records, chart.Records)
		}

		// 接收方读取仍给出提交前的当前生效版本。
		res, err := st.Read(rcv, p.ID, e.ID, Diagnosis)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、授权、请求号或记录集合等业务校验无关。原数据文件先挪到旁边，
	// 事后原样还原。
	clk.t = clk.t.Add(time.Hour)
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 全部业务条件满足（含重复标识与乱序入参），仅本地保存失败：必须明确
	// 报错，且不能伪装成请求号冲突、参数非法或访问拒绝等业务结果。
	// （与更正保存失败的约定一致：非 nil 错误时返回值无意义，调用方必须
	// 以错误为准；下面的正式状态断言保证它没有成为一份正式交换。）
	if _, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID,
		[]ID{ro.ID, rd.ID, ro.ID}, "req-failed-save"); err == nil {
		t.Fatal("exchange creation must fail when local save fails")
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：交换列表、审计、临床记录与既有交换全部维持提交前状态。
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
	// 重开后仍是操作前的正式数据：失败交换与其创建事件都不存在。
	checkIntact("after reopen", s2)

	// 失败之后，所选诊断记录经历一次合法更正：当前推进到第 3 版。
	clk.t = clk.t.Add(time.Hour)
	diagV3, err := s2.CorrectRecord(doc, rd.ID, 2, "诊断v3内容", "失败后复核更正")
	if err != nil {
		t.Fatalf("legal correction after failed save: %v", err)
	}
	if diagV3.Number != 3 || diagV3.PrevID != diagV2.ID {
		t.Fatalf("bad v3: %+v", diagV3)
	}

	// 更正之后、重试之前的审计基线：相对失败前只多出这一条更正事件，
	// 失败交换仍然没有任何事件。
	auditAtRetry, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditAtRetry) != len(auditBefore)+1 ||
		!reflect.DeepEqual(auditAtRetry[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after legal correction:\nbefore: %+v\nafter:  %+v", auditBefore, auditAtRetry)
	}
	if last := auditAtRetry[len(auditAtRetry)-1]; last.Action != ActionCorrected ||
		last.ObjectID != rd.ID {
		t.Fatalf("unexpected event from legal correction: %+v", last)
	}
	if xs, _ := s2.ListExchanges(doc, p.ID); len(xs) != 1 {
		t.Fatalf("correction must not add exchanges: %d", len(xs))
	}

	// 保存条件恢复后：同一内部使用者沿用失败的请求号、同一患者/接收方/
	// 绑定授权与原记录集合再次创建。失败尝试不占用请求号，不得返回冲突。
	clk.t = clk.t.Add(time.Hour)
	xNew, err := s2.CreateExchange(doc, p.ID, rcv.ID, a.ID,
		[]ID{ro.ID, rd.ID, ro.ID}, "req-failed-save")
	if err != nil {
		t.Fatalf("retry with failed request id after recovery: %v", err)
	}
	if xNew.ID == "" || xNew.ID == xPrior.ID {
		t.Fatalf("retry must create a new exchange, got %+v", xNew)
	}
	if xNew.Status != ExchangePending || xNew.Receipt != nil {
		t.Fatalf("new exchange should be pending receipt without receipt: %+v", xNew)
	}
	if xNew.RequestID != "req-failed-save" || xNew.CreatorID != doc.ID {
		t.Fatalf("new exchange header wrong: %+v", xNew)
	}
	if !xNew.CreatedAt.Equal(clk.t) {
		t.Fatalf("created at %v != %v", xNew.CreatedAt, clk.t)
	}

	// 新包固化的是“成功创建时”的当前版本与完整内容：诊断为 v3，
	// 而不是失败当时准备好的 v2；医嘱仍为 v1。重复标识合并为两条。
	if len(xNew.Package.Records) != 2 {
		t.Fatalf("new package records = %d, want 2", len(xNew.Package.Records))
	}
	newByID := recordsByID(xNew.Package.Records)
	gotDiag := newByID[rd.ID]
	if gotDiag.VersionID != diagV3.ID || gotDiag.Version != 3 ||
		gotDiag.Content != "诊断v3内容" || !gotDiag.EffectiveAt.Equal(diagV3.CreatedAt) {
		t.Fatalf("new package froze stale v2 instead of current v3: %+v", gotDiag)
	}
	gotOrd := newByID[ro.ID]
	if gotOrd.VersionID == "" || gotOrd.Version != 1 || gotOrd.Content != "医嘱v1内容" {
		t.Fatalf("order record in new package wrong: %+v", gotOrd)
	}
	if xNew.Digest == "" || xNew.Digest == xPrior.Digest {
		t.Fatalf("new package digest wrong: %q vs prior %q", xNew.Digest, xPrior.Digest)
	}
	if recomputed := packageDigest(xNew.Package); recomputed != xNew.Digest {
		t.Fatalf("digest mismatch: got %q, recomputed %q", xNew.Digest, recomputed)
	}

	// 成功这一步只新增一份待回执交换：列表恰为两份，原交换在前且原样保留，
	// 没有为撤销失败而清空历史。
	xs, err := s2.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 2 || xs[0].ID != xPrior.ID || xs[1].ID != xNew.ID {
		t.Fatalf("exchange list after retry: %+v", xs)
	}
	assertExchangeMatches(t, xs[0], priorSnap, "prior exchange after retry")
	// 原交换包内的诊断仍是创建当时固化的 v2，未被后续更正或本次重试改写。
	priorByID := recordsByID(xs[0].Package.Records)
	if pr := priorByID[rd.ID]; pr.VersionID != diagV2.ID || pr.Version != 2 ||
		pr.Content != "诊断v2内容" {
		t.Fatalf("prior package mutated: %+v", pr)
	}
	if xs[0].Status != ExchangeRejected || xs[0].Receipt == nil ||
		xs[0].Receipt.Outcome != ReceiptRejected || xs[0].Receipt.Reason != "既有拒绝原因" {
		t.Fatalf("prior receipt lost: %+v", xs[0].Receipt)
	}

	// 审计相对重试前基线只新增一条交换创建事件，记录本次成功操作的身份、
	// 时间与交换标识；既有事件（含更正事件）原样保留。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditAtRetry)+1 || !reflect.DeepEqual(audit[:len(auditAtRetry)], auditAtRetry) {
		t.Fatalf("audit after retry:\nbefore: %+v\nafter:  %+v", auditAtRetry, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionExchanged || last.ObjectType != "exchange" ||
		last.ObjectID != xNew.ID || last.ActorID != doc.ID ||
		last.PatientID != p.ID || !last.OccurredAt.Equal(clk.t) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if got := auditActions(audit)[ActionExchanged]; got != createsBefore+1 {
		t.Fatalf("exchange_created events = %d, want %d", got, createsBefore+1)
	}

	// 关闭后从同一数据位置重新打开：成功交换与其审计完整可见，失败尝试
	// 不能作为另一份交换或事件出现。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })

	got, err := s3.GetExchange(doc, p.ID, xNew.ID)
	if err != nil {
		t.Fatalf("get new exchange after reopen: %v", err)
	}
	assertExchangeMatches(t, got, xNew, "new exchange after reopen")
	gotList, err := s3.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotList) != 2 {
		t.Fatalf("exchange count after reopen = %d, want 2", len(gotList))
	}
	assertExchangeMatches(t, gotList[0], priorSnap, "prior exchange after reopen")

	var createIDs []ID
	for _, ev := range mustAudit(t, s3, p.ID) {
		if ev.Action == ActionExchanged {
			createIDs = append(createIDs, ev.ObjectID)
		}
	}
	if !reflect.DeepEqual(createIDs, []ID{xPrior.ID, xNew.ID}) {
		t.Fatalf("exchange_created events after reopen = %v, want [%q %q]",
			createIDs, xPrior.ID, xNew.ID)
	}

	// 既有公开行为保留：同请求号同参数重试返回本次成功交换，不另建交换或事件。
	retry, err := s3.CreateExchange(doc, p.ID, rcv.ID, a.ID,
		[]ID{rd.ID, ro.ID}, "req-failed-save")
	if err != nil {
		t.Fatalf("idempotent retry after reopen: %v", err)
	}
	if retry.ID != xNew.ID {
		t.Fatalf("retry returned %q, want %q", retry.ID, xNew.ID)
	}
	if xs, _ := s3.ListExchanges(doc, p.ID); len(xs) != 2 {
		t.Fatalf("idempotent retry changed exchange count: %d", len(xs))
	}
	if evs := mustAudit(t, s3, p.ID); len(evs) != len(auditAtRetry)+1 {
		t.Fatalf("idempotent retry added audit events: %d", len(evs))
	}
}
