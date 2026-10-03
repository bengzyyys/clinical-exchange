package clinical

import (
	"errors"
	"testing"
	"time"
)

// 本文件固定一条约定：调用方拿到的交换、包内容与回执只是本次操作的结果
// 副本。修改手中的结果不能改变库内正式保存的交换，也不能影响另一份已经
// 取得的结果；正式状态与回执只以登记操作的结果为准。

// requireFrozenPackage 校验包仍是夹具创建时固化的内容：
// 诊断当前 v2、医嘱 v1，各两条、按标识可检索。
func requireFrozenPackage(t *testing.T, pkg Package, f exchangeFixture) {
	t.Helper()
	if pkg.PatientID != f.pid || pkg.ReceiverID != rcv.ID {
		t.Fatalf("package header changed: %+v", pkg)
	}
	if len(pkg.Records) != 2 {
		t.Fatalf("package records = %d, want 2: %+v", len(pkg.Records), pkg.Records)
	}
	byID := map[ID]PackagedRecord{}
	for _, pr := range pkg.Records {
		byID[pr.RecordID] = pr
	}
	diag, ok := byID[f.diag]
	if !ok || diag.VersionID != f.diagV2.ID || diag.Version != 2 || diag.Content != "诊断v2内容" {
		t.Fatalf("frozen diagnosis lost: %+v", diag)
	}
	ord, ok := byID[f.ord]
	if !ok || ord.Version != 1 || ord.Content != "医嘱v1内容" {
		t.Fatalf("frozen order lost: %+v", ord)
	}
}

// 篡改一份交换结果的全部可变字段：状态、摘要、包头与包内记录。
func tamperExchangeResult(x *Exchange) {
	x.Status = ExchangeAccepted
	x.Digest = "tampered-digest"
	x.Package.PatientID = "pat_forged"
	x.Package.ReceiverID = "rcv-forged"
	for i := range x.Package.Records {
		x.Package.Records[i].Content = "篡改内容"
		x.Package.Records[i].RecordID = "rec_forged"
		x.Package.Records[i].VersionID = "ver_forged"
		x.Package.Records[i].Version = 99
	}
}

// ---- 首次创建返回的交换：本地修改不影响库内正式保存的交换 ----

func TestCreatedExchangeResultIsIsolated(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-iso-create")
	if err != nil {
		t.Fatal(err)
	}
	wantID, wantDigest := x.ID, x.Digest
	auditBefore := len(mustAudit(t, f.s, f.pid))

	// 改掉返回包内记录的内容、标识、版本信息，并改动记录集合本身。
	tamperExchangeResult(&x)
	x.Package.Records = append(x.Package.Records, PackagedRecord{RecordID: "rec_extra", Content: "夹带"})
	x.Package.Records = x.Package.Records[:0]

	got, err := f.s.GetExchange(doc, f.pid, wantID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ExchangePending || got.Digest != wantDigest || got.Receipt != nil {
		t.Fatalf("stored exchange changed by local mutation: %+v", got)
	}
	requireFrozenPackage(t, got.Package, f)

	// 接收方取包同样不受调用方本地修改影响。
	del, err := f.s.FetchPackage(rcv, wantID)
	if err != nil {
		t.Fatal(err)
	}
	if del.Digest != wantDigest || del.Status != ExchangePending {
		t.Fatalf("delivery changed by local mutation: %+v", del)
	}
	requireFrozenPackage(t, del.Package, f)

	// 本地修改不产生任何审计事件。
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("local mutation added audit events: %d -> %d", auditBefore, got)
	}
}

// ---- 相同请求重试返回的交换：同样只是结果副本 ----

func TestRetriedExchangeResultIsIsolated(t *testing.T) {
	f := setupExchange(t)
	x1, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-iso-retry")
	if err != nil {
		t.Fatal(err)
	}
	// 先篡改首次返回的结果，重试仍应返回原交换的原始内容。
	tamperExchangeResult(&x1)

	x2, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.ord, f.diag}, "req-iso-retry")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if x2.ID == "" || x2.Status != ExchangePending {
		t.Fatalf("retry returned mutated state: %+v", x2)
	}
	requireFrozenPackage(t, x2.Package, f)
	wantDigest := x2.Digest

	// 再篡改重试结果，库内交换仍保持不变。
	tamperExchangeResult(&x2)
	got, err := f.s.GetExchange(doc, f.pid, x2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != wantDigest || got.Status != ExchangePending {
		t.Fatalf("stored exchange changed after retry result mutation: %+v", got)
	}
	requireFrozenPackage(t, got.Package, f)
}

// ---- 查询结果（单份与列表）各自独立，互不影响 ----

func TestQueriedExchangeResultsAreIsolated(t *testing.T) {
	f := setupExchange(t)
	x1, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-iso-q1")
	if err != nil {
		t.Fatal(err)
	}
	f.clk.t = f.clk.t.Add(time.Minute)
	x2, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-iso-q2")
	if err != nil {
		t.Fatal(err)
	}

	// 篡改列表中的第一项。
	xs, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 2 || xs[0].ID != x1.ID || xs[1].ID != x2.ID {
		t.Fatalf("unexpected list: %+v", xs)
	}
	tamperExchangeResult(&xs[0])
	xs[0].Package.Records = nil

	// 重新查询：两份交换都保持原样，修改列表项不连带影响另一份交换。
	xs2, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs2) != 2 {
		t.Fatalf("list changed size: %d", len(xs2))
	}
	for _, got := range xs2 {
		if got.Status != ExchangePending || got.Digest == "tampered-digest" {
			t.Fatalf("stored exchange changed via list result: %+v", got)
		}
		requireFrozenPackage(t, got.Package, f)
	}
	if xs2[0].Digest != x1.Digest || xs2[1].Digest != x2.Digest {
		t.Fatal("digests changed after list result mutation")
	}

	// 同一份交换先后取得的两份结果各自独立。
	a, err := f.s.GetExchange(doc, f.pid, x1.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.s.GetExchange(doc, f.pid, x1.ID)
	if err != nil {
		t.Fatal(err)
	}
	tamperExchangeResult(&a)
	a.Package.Records[0] = PackagedRecord{}
	if b.Status != ExchangePending || b.Digest != x1.Digest {
		t.Fatalf("second result changed by mutating the first: %+v", b)
	}
	requireFrozenPackage(t, b.Package, f)
	// 库内正式交换也未受影响。
	c, err := f.s.GetExchange(doc, f.pid, x1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest != x1.Digest || c.Status != ExchangePending {
		t.Fatalf("stored exchange changed: %+v", c)
	}
	requireFrozenPackage(t, c.Package, f)
}

// ---- 接收方取得的包：本地修改不影响再次取包、内部视图与回执登记 ----

func TestFetchedPackageDeliveryIsIsolated(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-iso-fetch")
	if err != nil {
		t.Fatal(err)
	}

	del, err := f.s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := del.Digest

	// 篡改手中包的内容与摘要。
	for i := range del.Package.Records {
		del.Package.Records[i].Content = "篡改内容"
		del.Package.Records[i].RecordID = "rec_forged"
		del.Package.Records[i].VersionID = "ver_forged"
	}
	del.Package.Records = append(del.Package.Records, PackagedRecord{RecordID: "rec_extra"})
	del.Digest = "forged-digest"
	del.Status = ExchangeAccepted

	// 再次取包仍是原有内容与摘要。
	del2, err := f.s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if del2.Digest != wantDigest || del2.Status != ExchangePending {
		t.Fatalf("re-fetch changed after local mutation: %+v", del2)
	}
	requireFrozenPackage(t, del2.Package, f)

	// 内部使用者查看原交换也不受影响。
	got, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != wantDigest || got.Status != ExchangePending {
		t.Fatalf("internal view changed after delivery mutation: %+v", got)
	}
	requireFrozenPackage(t, got.Package, f)

	// 用篡改后的摘要提交回执：ErrConflict，交换保持待回执，不新增回执审计。
	auditBefore := len(mustAudit(t, f.s, f.pid))
	if _, err := f.s.SubmitReceipt(rcv, x.ID, "forged-digest", ReceiptAccepted, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("forged digest err = %v, want ErrConflict", err)
	}
	got, err = f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ExchangePending || got.Receipt != nil {
		t.Fatalf("forged digest changed exchange: %+v", got)
	}
	if n := len(mustAudit(t, f.s, f.pid)); n != auditBefore {
		t.Fatalf("forged digest added audit events: %d -> %d", auditBefore, n)
	}

	// 先前的本地修改不削弱原摘要的登记能力：用原摘要提交仍成功。
	conf, err := f.s.SubmitReceipt(rcv, x.ID, wantDigest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("receipt with original digest: %v", err)
	}
	if conf.Status != ExchangeAccepted {
		t.Fatalf("status = %q", conf.Status)
	}
	got, err = f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ExchangeAccepted || got.Receipt == nil || got.Receipt.Outcome != ReceiptAccepted {
		t.Fatalf("receipt not registered after local mutations: %+v", got)
	}
}

// ---- 已登记回执的交换：查询结果中的回执只是副本 ----

func TestReceiptInQueryResultIsIsolated(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-iso-rcpt")
	if err != nil {
		t.Fatal(err)
	}
	conf, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "内容缺失")
	if err != nil {
		t.Fatal(err)
	}
	auditBefore := len(mustAudit(t, f.s, f.pid))

	// 在查询结果中改动回执结果、拒绝原因与登记时间。
	r1, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Receipt == nil {
		t.Fatal("receipt missing from query result")
	}
	r1.Status = ExchangeAccepted
	r1.Receipt.Outcome = ReceiptAccepted
	r1.Receipt.Reason = "篡改原因"
	r1.Receipt.RegisteredAt = f.clk.t.Add(72 * time.Hour)

	// 正式交换仍保留实际登记的回执。
	r2, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status != ExchangeRejected || r2.Receipt == nil {
		t.Fatalf("stored receipt changed: %+v", r2)
	}
	if r2.Receipt.Outcome != ReceiptRejected || r2.Receipt.Reason != "内容缺失" ||
		!r2.Receipt.RegisteredAt.Equal(conf.RegisteredAt) {
		t.Fatalf("stored receipt mutated: %+v", r2.Receipt)
	}

	// 另一份查询结果也不随之变化：篡改 r2 的回执，再查仍原样。
	r2.Receipt.Outcome = ReceiptAccepted
	r2.Receipt.Reason = "再次篡改"
	r3, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r3.Receipt.Outcome != ReceiptRejected || r3.Receipt.Reason != "内容缺失" {
		t.Fatalf("receipt changed across query results: %+v", r3.Receipt)
	}

	// 列表结果中的回执同样只是副本。
	xs, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].Receipt == nil {
		t.Fatalf("unexpected list: %+v", xs)
	}
	xs[0].Receipt.Outcome = ReceiptAccepted
	xs[0].Receipt.Reason = "列表篡改"
	r4, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r4.Receipt.Outcome != ReceiptRejected || r4.Receipt.Reason != "内容缺失" {
		t.Fatalf("stored receipt changed via list result: %+v", r4.Receipt)
	}

	// 以上本地修改均不产生审计事件。
	if n := len(mustAudit(t, f.s, f.pid)); n != auditBefore {
		t.Fatalf("local receipt mutation added audit events: %d -> %d", auditBefore, n)
	}
}

// ---- 待回执的交换：在手中结果里补上回执不产生任何业务变更 ----

func TestLocallyAttachedReceiptHasNoEffect(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-iso-pending")
	if err != nil {
		t.Fatal(err)
	}

	// 尚未登记回执：查询结果没有回执。
	got, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Receipt != nil || got.Status != ExchangePending {
		t.Fatalf("pending exchange should have no receipt: %+v", got)
	}
	auditBefore := len(mustAudit(t, f.s, f.pid))

	// 在手中的结果里补上回执信息。
	got.Status = ExchangeAccepted
	got.Receipt = &Receipt{Outcome: ReceiptAccepted, RegisteredAt: f.clk.t}

	// 重新查询仍没有回执，状态仍待回执，也没有新增审计。
	again, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Receipt != nil || again.Status != ExchangePending {
		t.Fatalf("locally attached receipt leaked into store: %+v", again)
	}
	xs, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].Receipt != nil || xs[0].Status != ExchangePending {
		t.Fatalf("list shows locally attached receipt: %+v", xs)
	}
	if n := len(mustAudit(t, f.s, f.pid)); n != auditBefore {
		t.Fatalf("local receipt attachment added audit events: %d -> %d", auditBefore, n)
	}
	var receiptEvents int
	for _, ev := range mustAudit(t, f.s, f.pid) {
		if ev.Action == ActionReceipted {
			receiptEvents++
		}
	}
	if receiptEvents != 0 {
		t.Fatalf("receipt audit events = %d, want 0", receiptEvents)
	}
}
