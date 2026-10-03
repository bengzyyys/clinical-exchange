package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestCreateExchangeSaveFailureLeavesNothingAndRequestIDReusable 覆盖“内部使用者
// 身份合法、患者未停用、绑定授权当前有效且自身覆盖全部所选已生效记录、请求号
// 尚未用过，但本地保存失败”的交换创建场景：
//
//   - CreateExchange 必须明确报错，且不能伪装成 ErrConflict、ErrInvalidArgument
//     或 ErrAccessDenied 等业务拒绝；
//   - 失败后正式数据维持提交前状态：没有这次失败产生的交换与交换创建审计，
//     既有记录的当前版本、内容与版本历史不变，患者此前已成功保存的交换
//     （标识、包内版本与内容、摘要、回执）原样保留，不存在只有交换没有审计
//     或只有审计没有交换的半截结果；
//   - 保存条件恢复后，同一内部使用者可沿用失败时的请求号再次创建，不得收到
//     请求号冲突；所选记录经过一次合法更正后，新包必须固化成功创建时的当前
//     版本，不能沿用失败时准备的版本；
//   - 成功重试只新增一份待回执交换与一条对应的创建审计，原有交换不变；
//   - 关闭后从同一位置重新打开，成功交换及其审计仍在，失败尝试不留下任何
//     交换或事件。
func TestCreateExchangeSaveFailureLeavesNothingAndRequestIDReusable(t *testing.T) {
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
	enc, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 诊断：生效后更正过一次，当前为第 2 版。
	diag, err := s.CreateDraft(doc, p.ID, enc.ID, Diagnosis, "交换前诊断v1内容")
	if err != nil {
		t.Fatal(err)
	}
	diagV1, err := s.ActivateRecord(doc, diag.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	diagV2, err := s.CorrectRecord(doc, diag.ID, 1, "交换前诊断v2内容", "录入笔误")
	if err != nil {
		t.Fatal(err)
	}
	// 医嘱：已生效第 1 版。
	ord, err := s.CreateDraft(doc, p.ID, enc.ID, Order, "交换前医嘱v1内容")
	if err != nil {
		t.Fatal(err)
	}
	ordV1, err := s.ActivateRecord(doc, ord.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 绑定授权：属于同一患者与接收方、当前有效，整类范围自身覆盖诊断与医嘱。
	authStart := clk.t.Add(-time.Hour)
	authEnd := clk.t.Add(48 * time.Hour)
	auth, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{
			{EncounterID: enc.ID, Category: Diagnosis},
			{EncounterID: enc.ID, Category: Order},
		}, authStart, authEnd)
	if err != nil {
		t.Fatal(err)
	}

	// 患者已有一份成功保存、且已登记回执的交换，作为不得被动到的既有数据。
	clk.t = clk.t.Add(time.Hour)
	xOld, err := s.CreateExchange(doc, p.ID, rcv.ID, auth.ID,
		[]ID{diag.ID}, "req-old-exchange")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitReceipt(rcv, xOld.ID, xOld.Digest, ReceiptAccepted, ""); err != nil {
		t.Fatal(err)
	}
	// 推进时钟，使失败尝试与既有交换的创建时间可区分。
	clk.t = clk.t.Add(time.Hour)

	// 提交前基线：交换列表、审计、完整档案（含记录版本历史）。
	listBefore, err := s.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listBefore) != 1 {
		t.Fatalf("baseline exchanges = %d, want 1", len(listBefore))
	}
	oldWant := snapshotExchange(listBefore[0])
	if oldWant.ID != xOld.ID || oldWant.Status != ExchangeAccepted ||
		oldWant.Receipt == nil || oldWant.Receipt.Outcome != ReceiptAccepted {
		t.Fatalf("baseline old exchange wrong: %+v", oldWant)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	diagHistBefore := findHistory(chartBefore, diag.ID)
	ordHistBefore := findHistory(chartBefore, ord.ID)
	if diagHistBefore == nil || diagHistBefore.CurrentVersion == nil ||
		diagHistBefore.CurrentVersion.ID != diagV2.ID || len(diagHistBefore.Versions) != 2 ||
		diagV2.PrevID != diagV1.ID {
		t.Fatalf("unexpected baseline diagnosis history: %+v", diagHistBefore)
	}
	if ordHistBefore == nil || ordHistBefore.CurrentVersion == nil ||
		ordHistBefore.CurrentVersion.ID != ordV1.ID || len(ordHistBefore.Versions) != 1 {
		t.Fatalf("unexpected baseline order history: %+v", ordHistBefore)
	}

	// checkIntact 断言失败后（以及重开后）的正式数据与提交前完全一致：
	// 没有新增交换或交换创建审计；既有交换（含包与回执）、记录当前版本与
	// 完整版本历史全部原样保留。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		lst, err := st.ListExchanges(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list exchanges: %v", label, err)
		}
		if len(lst) != 1 {
			t.Fatalf("%s: exchange count = %d, want 1 (failed create must not persist)", label, len(lst))
		}
		assertExchangeMatches(t, lst[0], oldWant, label+": existing exchange")

		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		if got := auditActions(audit)[ActionExchanged]; got != 1 {
			t.Fatalf("%s: exchange_created events = %d, want only the prior 1", label, got)
		}

		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		if !reflect.DeepEqual(chart, chartBefore) {
			t.Fatalf("%s: chart changed after failed save:\nbefore: %+v\nafter:  %+v",
				label, chartBefore, chart)
		}
		dh := findHistory(chart, diag.ID)
		if dh == nil || dh.Record.CurrentVersionID != diagV2.ID ||
			dh.CurrentVersion == nil || dh.CurrentVersion.ID != diagV2.ID ||
			dh.CurrentVersion.Content != "交换前诊断v2内容" || len(dh.Versions) != 2 {
			t.Fatalf("%s: diagnosis current version/history changed: %+v", label, dh)
		}
		oh := findHistory(chart, ord.ID)
		if oh == nil || oh.Record.CurrentVersionID != ordV1.ID ||
			oh.CurrentVersion == nil || oh.CurrentVersion.ID != ordV1.ID ||
			oh.CurrentVersion.Content != "交换前医嘱v1内容" || len(oh.Versions) != 1 {
			t.Fatalf("%s: order current version/history changed: %+v", label, oh)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、授权、记录集合等业务条件无关。原数据文件先挪到旁边，事后原样还原。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 业务条件全部满足：内部使用者、未停用的合成患者、同一患者与接收方的当前
	// 有效授权、两条均被该绑定授权自身覆盖的已生效记录、尚未用过的请求号。
	// 唯一的失败原因是本地保存。
	failedRecs := []ID{ord.ID, diag.ID}
	if _, err := s.CreateExchange(doc, p.ID, rcv.ID, auth.ID, failedRecs, "req-save-fail"); err == nil {
		t.Fatal("CreateExchange must fail when local save fails")
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrDeactivated) ||
		errors.Is(err, ErrNotFound) || errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("save failure surfaced as a business rejection: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后立即查看：交换列表、审计、记录与既有交换全部维持提交前状态。
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
	// 重开后正式数据仍是操作前的样子：失败尝试没有变成交换或事件。
	checkIntact("after reopen", s2)

	// 所选记录经过一次合法更正：诊断推进到第 3 版。
	clk.t = clk.t.Add(time.Hour)
	diagV3, err := s2.CorrectRecord(doc, diag.ID, 2, "重试时诊断v3内容", "失败后合法更正")
	if err != nil {
		t.Fatalf("legal correction after failed save: %v", err)
	}
	if diagV3.Number != 3 || diagV3.PrevID != diagV2.ID {
		t.Fatalf("bad v3: %+v", diagV3)
	}
	// 合法更正在重试前已新增一条更正审计；此后交换创建只应再新增一条事件。
	auditAtRetry, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditAtRetry[:len(auditBefore)], auditBefore) {
		t.Fatalf("legal correction altered prior audit events:\nbefore: %+v\nafter:  %+v",
			auditBefore, auditAtRetry[:len(auditBefore)])
	}
	corrEvent := auditAtRetry[len(auditAtRetry)-1]
	if corrEvent.Action != ActionCorrected || corrEvent.ObjectID != diag.ID || corrEvent.ActorID != doc.ID {
		t.Fatalf("unexpected correction audit event: %+v", corrEvent)
	}

	// 失败时准备的包若被错误固化，其摘要对应“诊断 v2 + 医嘱 v1”的内容。
	stalePkg := Package{PatientID: p.ID, ReceiverID: rcv.ID}
	stalePkg.Records = []PackagedRecord{
		{
			EncounterID: enc.ID, Category: diagV2.Category, RecordID: diag.ID,
			VersionID: diagV2.ID, Version: diagV2.Number,
			EffectiveAt: diagV2.CreatedAt, Content: diagV2.Content,
		},
		{
			EncounterID: enc.ID, Category: ordV1.Category, RecordID: ord.ID,
			VersionID: ordV1.ID, Version: ordV1.Number,
			EffectiveAt: ordV1.CreatedAt, Content: ordV1.Content,
		},
	}
	staleDigest := packageDigest(stalePkg)

	// 保存条件恢复后：同一内部使用者沿用失败的请求号、同一患者/接收方/绑定
	// 授权与同一记录集合再次创建——不得因失败尝试收到请求号冲突。
	clk.t = clk.t.Add(time.Hour)
	xNew, err := s2.CreateExchange(doc, p.ID, rcv.ID, auth.ID, failedRecs, "req-save-fail")
	if err != nil {
		t.Fatalf("retry with the failed request id must succeed: %v", err)
	}
	if xNew.ID == "" || xNew.ID == xOld.ID {
		t.Fatalf("bad new exchange id: %+v", xNew)
	}
	if xNew.Status != ExchangePending || xNew.Receipt != nil {
		t.Fatalf("new exchange must be pending receipt with no receipt: %+v", xNew)
	}
	if !xNew.CreatedAt.Equal(clk.t) || xNew.CreatorID != doc.ID ||
		xNew.RequestID != "req-save-fail" {
		t.Fatalf("new exchange header wrong: %+v", xNew)
	}

	// 新包固化成功创建时的当前版本与完整内容：诊断 v3（而非失败时准备的
	// v2），医嘱仍为 v1；摘要随之按当前内容重算，不能沿用失败时的半成品包。
	if len(xNew.Package.Records) != 2 {
		t.Fatalf("new package records = %d, want 2", len(xNew.Package.Records))
	}
	newByID := recordsByID(xNew.Package.Records)
	gotDiag := newByID[diag.ID]
	if gotDiag.VersionID != diagV3.ID || gotDiag.Version != 3 ||
		gotDiag.Content != "重试时诊断v3内容" || !gotDiag.EffectiveAt.Equal(diagV3.CreatedAt) {
		t.Fatalf("new package froze non-current diagnosis version: %+v", gotDiag)
	}
	gotOrd := newByID[ord.ID]
	if gotOrd.VersionID != ordV1.ID || gotOrd.Version != 1 ||
		gotOrd.Content != "交换前医嘱v1内容" {
		t.Fatalf("new package order entry wrong: %+v", gotOrd)
	}
	if xNew.Digest == staleDigest {
		t.Fatal("new exchange reused the package prepared during the failed attempt (stale v2 digest)")
	}
	if xNew.Digest != packageDigest(xNew.Package) {
		t.Fatalf("new exchange digest %q does not match its frozen package %+v",
			xNew.Digest, xNew.Package)
	}
	if xNew.Digest == xOld.Digest {
		t.Fatal("new exchange digest unexpectedly equals the prior exchange digest")
	}

	// 成功重试只新增一份待回执交换：列表长度 +1，既有交换原样排在前面。
	listAfter, err := s2.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listAfter) != 2 {
		t.Fatalf("exchanges after retry = %d, want 2", len(listAfter))
	}
	assertExchangeMatches(t, listAfter[0], oldWant, "existing exchange stays unchanged after retry")
	assertExchangeMatches(t, listAfter[1], snapshotExchange(xNew), "new exchange in list")

	// 审计：既有事件原样保留（前缀完全一致），只新增一条交换创建事件，
	// 记录本次成功操作的身份、时间与交换标识；失败尝试没有任何事件。
	auditAfter, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(auditAfter) != len(auditAtRetry)+1 {
		t.Fatalf("audit count after retry = %d, want %d", len(auditAfter), len(auditAtRetry)+1)
	}
	if !reflect.DeepEqual(auditAfter[:len(auditAtRetry)], auditAtRetry) {
		t.Fatalf("prior audit events mutated:\nbefore: %+v\nafter:  %+v",
			auditAtRetry, auditAfter[:len(auditAtRetry)])
	}
	last := auditAfter[len(auditAfter)-1]
	if last.Action != ActionExchanged || last.ObjectType != "exchange" ||
		last.ObjectID != xNew.ID || last.ActorID != doc.ID ||
		last.PatientID != p.ID || !last.OccurredAt.Equal(clk.t) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if got := auditActions(auditAfter)[ActionExchanged]; got != 2 {
		t.Fatalf("exchange_created events = %d, want 2 (prior + retried)", got)
	}

	// 成功后以相同请求号再试：仍是幂等返回这份新交换，不重新固化内容、
	// 不新增审计（既有公开行为保持不变）。
	retry, err := s2.CreateExchange(doc, p.ID, rcv.ID, auth.ID,
		[]ID{diag.ID, ord.ID, diag.ID}, "req-save-fail")
	if err != nil {
		t.Fatalf("idempotent retry after recovered create: %v", err)
	}
	if retry.ID != xNew.ID || retry.Digest != xNew.Digest {
		t.Fatalf("idempotent retry did not return the retried exchange:\n%+v\n%+v", retry, xNew)
	}
	if n := len(mustAudit(t, s2, p.ID)); n != len(auditAtRetry)+1 {
		t.Fatalf("idempotent retry changed audit count: %d", n)
	}

	// 关闭后从同一数据位置重新打开：成功交换及其审计完整保留，
	// 失败尝试既不作为另一份交换、也不作为另一条事件出现。
	newWant := snapshotExchange(xNew)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })

	lst3, err := s3.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatalf("list after final reopen: %v", err)
	}
	if len(lst3) != 2 {
		t.Fatalf("exchanges after final reopen = %d, want 2", len(lst3))
	}
	assertExchangeMatches(t, lst3[0], oldWant, "old exchange after final reopen")
	assertExchangeMatches(t, lst3[1], newWant, "retried exchange after final reopen")

	gotNew, err := s3.GetExchange(doc, p.ID, xNew.ID)
	if err != nil {
		t.Fatalf("get retried exchange after reopen: %v", err)
	}
	assertExchangeMatches(t, gotNew, newWant, "GetExchange retried after final reopen")
	// 重开后包内仍是成功创建时固化的诊断 v3 完整内容。
	reopenedDiag := recordsByID(gotNew.Package.Records)[diag.ID]
	if reopenedDiag.Version != 3 || reopenedDiag.VersionID != diagV3.ID ||
		reopenedDiag.Content != "重试时诊断v3内容" {
		t.Fatalf("retried package lost frozen v3 after reopen: %+v", reopenedDiag)
	}

	audit3, err := s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatalf("audit after final reopen: %v", err)
	}
	if len(audit3) != len(auditAfter) {
		t.Fatalf("audit count after final reopen = %d, want %d", len(audit3), len(auditAfter))
	}
	if !reflect.DeepEqual(audit3, auditAfter) {
		t.Fatalf("audit after final reopen differs from before close:\nbefore: %+v\nafter:  %+v",
			auditAfter, audit3)
	}
}
