package clinical

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“返回结果隔离”约定补回归保障：调用方从 CreateExchange、幂等重试、
// GetExchange、ListExchanges、FetchPackage 拿到的交换、包内容与回执只是本次
// 操作的结果视图。调用方在本地修改手中的结果，既不能改写库内正式保存的交换
// （固化包、摘要、状态、回执），也不能波及另一份已经取得的结果；纯本地修改
// 不产生业务变更，也不新增审计事件。

// ---- 测试自用的独立深拷贝与篡改工具 ----
//
// 刻意不调用生产代码的 cloneExchangeValue/clonePackage：预期基线由测试自行
// 深拷贝，这样即便生产拷贝退化为浅拷贝，基线仍保持创建时内容，隔离断言才能
// 真正抓住泄漏。

// snapshotExchange 独立深拷贝一份交换作为预期基线（在篡改手中结果之前调用）。
func snapshotExchange(x Exchange) Exchange {
	cp := x
	recs := make([]PackagedRecord, len(x.Package.Records))
	copy(recs, x.Package.Records)
	cp.Package.Records = recs
	if x.Receipt != nil {
		r := *x.Receipt
		cp.Receipt = &r
	}
	return cp
}

// snapshotDelivery 独立深拷贝一份取包视图作为预期基线。
func snapshotDelivery(d PackageDelivery) PackageDelivery {
	cp := d
	recs := make([]PackagedRecord, len(d.Package.Records))
	copy(recs, d.Package.Records)
	cp.Package.Records = recs
	return cp
}

func recordsByID(recs []PackagedRecord) map[ID]PackagedRecord {
	m := make(map[ID]PackagedRecord, len(recs))
	for _, r := range recs {
		m[r.RecordID] = r
	}
	return m
}

// tamperExchangeResult 在调用方手中的交换上做尽可能广泛的本地修改：
// 状态、摘要、包头、首条记录的全部字段（内容/记录标识/版本标识/版本号等）、
// 记录集合（丢掉一条）以及回执（凭空伪造或覆盖既有回执）。
func tamperExchangeResult(x *Exchange) {
	x.Status = ExchangeAccepted
	x.Digest = "digest-tampered-by-caller"
	x.Package.PatientID = "pat_tampered"
	x.Package.ReceiverID = "rcv_tampered"
	if len(x.Package.Records) > 0 {
		x.Package.Records[0].EncounterID = "enc_tampered"
		x.Package.Records[0].Category = Order
		x.Package.Records[0].RecordID = "rec_tampered"
		x.Package.Records[0].VersionID = "ver_tampered"
		x.Package.Records[0].Version = 999
		x.Package.Records[0].EffectiveAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		x.Package.Records[0].Content = "调用方本地篡改的内容"
	}
	if len(x.Package.Records) > 1 {
		// 调用方改动手中的记录集合（丢掉一条）。
		x.Package.Records = x.Package.Records[:1]
	}
	x.Receipt = &Receipt{
		Outcome:      ReceiptAccepted,
		Reason:       "调用方在手中伪造的回执",
		RegisteredAt: time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC),
	}
}

// tamperDeliveryResult 在接收方手中的取包视图上做同样广泛的本地修改。
func tamperDeliveryResult(d *PackageDelivery) {
	d.ExchangeID = "exch_tampered"
	d.Status = ExchangeAccepted
	d.Digest = "digest-tampered-by-receiver"
	d.CreatedAt = time.Date(2002, 3, 4, 5, 6, 7, 0, time.UTC)
	d.Package.PatientID = "pat_tampered"
	d.Package.ReceiverID = "rcv_tampered"
	if len(d.Package.Records) > 0 {
		d.Package.Records[0].EncounterID = "enc_tampered"
		d.Package.Records[0].Category = Order
		d.Package.Records[0].RecordID = "rec_tampered"
		d.Package.Records[0].VersionID = "ver_tampered"
		d.Package.Records[0].Version = 999
		d.Package.Records[0].EffectiveAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		d.Package.Records[0].Content = "接收方本地篡改的内容"
	}
	if len(d.Package.Records) > 1 {
		d.Package.Records = d.Package.Records[:1]
	}
}

func assertExchangeMatches(t *testing.T, got, want Exchange, label string) {
	t.Helper()
	if got.ID != want.ID || got.Status != want.Status || got.Digest != want.Digest ||
		got.PatientID != want.PatientID || got.ReceiverID != want.ReceiverID ||
		got.AuthorizationID != want.AuthorizationID || got.RequestID != want.RequestID ||
		got.CreatorID != want.CreatorID || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("%s: exchange header changed:\n got=%+v\nwant=%+v", label, got, want)
	}
	if got.Package.PatientID != want.Package.PatientID || got.Package.ReceiverID != want.Package.ReceiverID {
		t.Fatalf("%s: package header changed:\n got=%+v\nwant=%+v", label, got.Package, want.Package)
	}
	if len(got.Package.Records) != len(want.Package.Records) {
		t.Fatalf("%s: record set changed: got %d records %+v, want %d records %+v",
			label, len(got.Package.Records), got.Package.Records, len(want.Package.Records), want.Package.Records)
	}
	gm := recordsByID(got.Package.Records)
	wm := recordsByID(want.Package.Records)
	if len(gm) != len(wm) {
		t.Fatalf("%s: duplicate record ids after mutation: %+v", label, got.Package.Records)
	}
	for rid, wr := range wm {
		gr, ok := gm[rid]
		if !ok {
			t.Fatalf("%s: record %q missing from package: %+v", label, rid, got.Package.Records)
		}
		if gr != wr {
			t.Fatalf("%s: record %q changed:\n got=%+v\nwant=%+v", label, rid, gr, wr)
		}
	}
	if (got.Receipt == nil) != (want.Receipt == nil) {
		t.Fatalf("%s: receipt presence changed: got present=%v, want present=%v",
			label, got.Receipt != nil, want.Receipt != nil)
	}
	if want.Receipt != nil {
		if got.Receipt.Outcome != want.Receipt.Outcome || got.Receipt.Reason != want.Receipt.Reason ||
			!got.Receipt.RegisteredAt.Equal(want.Receipt.RegisteredAt) {
			t.Fatalf("%s: receipt changed:\n got=%+v\nwant=%+v", label, *got.Receipt, *want.Receipt)
		}
	}
}

func assertDeliveryMatches(t *testing.T, got, want PackageDelivery, label string) {
	t.Helper()
	if got.ExchangeID != want.ExchangeID || got.Status != want.Status ||
		got.Digest != want.Digest || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("%s: delivery header changed:\n got=%+v\nwant=%+v", label, got, want)
	}
	if got.Package.PatientID != want.Package.PatientID || got.Package.ReceiverID != want.Package.ReceiverID {
		t.Fatalf("%s: delivery package header changed:\n got=%+v\nwant=%+v", label, got.Package, want.Package)
	}
	if len(got.Package.Records) != len(want.Package.Records) {
		t.Fatalf("%s: delivery record set changed: got %+v, want %+v",
			label, got.Package.Records, want.Package.Records)
	}
	gm := recordsByID(got.Package.Records)
	wm := recordsByID(want.Package.Records)
	if len(gm) != len(wm) {
		t.Fatalf("%s: duplicate record ids after mutation: %+v", label, got.Package.Records)
	}
	for rid, wr := range wm {
		gr, ok := gm[rid]
		if !ok {
			t.Fatalf("%s: record %q missing from delivery: %+v", label, rid, got.Package.Records)
		}
		if gr != wr {
			t.Fatalf("%s: delivery record %q changed:\n got=%+v\nwant=%+v", label, rid, gr, wr)
		}
	}
}

func receiptAuditCount(t *testing.T, s *Store, pid ID) int {
	t.Helper()
	return auditActions(mustAudit(t, s, pid))[ActionReceipted]
}

// ---- 首次创建返回的交换：本地篡改不入库 ----

func TestCreateExchangeResultIsDetachedFromStore(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag, f.ord}, "req-iso-create")
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotExchange(x)

	auditBefore := len(mustAudit(t, f.s, f.pid))

	tamperExchangeResult(&x)

	// 重新查看原交换：完整包、原摘要、原状态、无回执。
	got, err := f.s.GetExchange(doc, f.pid, want.ID)
	if err != nil {
		t.Fatalf("re-get after local tamper: %v", err)
	}
	assertExchangeMatches(t, got, want, "GetExchange after tampering create result")

	// 列表视图同样保持原样。
	lst, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(lst) != 1 {
		t.Fatalf("list len = %d, want 1", len(lst))
	}
	assertExchangeMatches(t, lst[0], want, "ListExchanges after tampering create result")

	// 纯本地修改不产生审计事件。
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("local tamper changed audit count: before=%d after=%d", auditBefore, got)
	}
}

// ---- 相同请求重试：被篡改的首次结果不影响重试，重试结果也独立 ----

func TestIdempotentRetryResultIsDetached(t *testing.T) {
	f := setupExchange(t)
	first, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.ord, f.diag}, "req-iso-retry")
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotExchange(first)

	// 改掉首次返回的交换。
	tamperExchangeResult(&first)

	// 相同请求重试：必须返回库内原交换，而不是被首次结果污染的内容。
	second, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag, f.ord, f.diag}, "req-iso-retry")
	if err != nil {
		t.Fatalf("idempotent retry after local tamper: %v", err)
	}
	assertExchangeMatches(t, second, want, "retry result after first result tampered")

	// 再改掉重试返回的交换，库内仍保持原样。
	tamperExchangeResult(&second)
	got, err := f.s.GetExchange(doc, f.pid, want.ID)
	if err != nil {
		t.Fatalf("re-get after tampering retry result: %v", err)
	}
	assertExchangeMatches(t, got, want, "GetExchange after tampering retry result")

	if n := auditActions(mustAudit(t, f.s, f.pid))[ActionExchanged]; n != 1 {
		t.Fatalf("exchange_created audit events = %d, want 1", n)
	}
}

// ---- 按患者查看：先后取得的两份结果彼此独立、也独立于库内交换 ----

func TestGetExchangeCopiesAreIndependent(t *testing.T) {
	f := setupExchange(t)
	created, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag, f.ord}, "req-iso-get")
	if err != nil {
		t.Fatal(err)
	}

	g1, err := f.s.GetExchange(doc, f.pid, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotExchange(g1)

	// 先取得两份独立结果。
	g2, err := f.s.GetExchange(doc, f.pid, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 修改第一份：第二份仍保留取得时的内容。
	tamperExchangeResult(&g1)
	assertExchangeMatches(t, g2, want, "previously fetched copy after another copy tampered")

	// 库内交换不变。
	g3, err := f.s.GetExchange(doc, f.pid, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, g3, want, "fresh GetExchange after tampering a copy")

	// 修改第二份同样不反向影响。
	tamperExchangeResult(&g2)
	g4, err := f.s.GetExchange(doc, f.pid, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, g4, want, "fresh GetExchange after tampering second copy")
}

// ---- 列表：改一项不连带其他交换；两次列表结果相互独立 ----

func TestListExchangesCopiesAreIndependent(t *testing.T) {
	f := setupExchange(t)
	x1, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag}, "req-iso-list-1")
	if err != nil {
		t.Fatal(err)
	}
	f.clk.t = f.clk.t.Add(time.Minute)
	x2, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.ord}, "req-iso-list-2")
	if err != nil {
		t.Fatal(err)
	}
	want1 := snapshotExchange(x1)
	want2 := snapshotExchange(x2)

	list1, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	list2, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list1) != 2 || list1[0].ID != want1.ID || list1[1].ID != want2.ID {
		t.Fatalf("unexpected list order: %+v", list1)
	}

	// 修改第一份列表中的第一项：同列表的第二项不得被连带改变。
	tamperExchangeResult(&list1[0])
	assertExchangeMatches(t, list1[1], want2, "sibling item in same list")

	// 另一份列表结果完全不受影响。
	assertExchangeMatches(t, list2[0], want1, "same item in second list")
	assertExchangeMatches(t, list2[1], want2, "sibling item in second list")

	// 库内两份交换都保持原样。
	g1, _ := f.s.GetExchange(doc, f.pid, want1.ID)
	g2, _ := f.s.GetExchange(doc, f.pid, want2.ID)
	assertExchangeMatches(t, g1, want1, "exchange 1 in store")
	assertExchangeMatches(t, g2, want2, "exchange 2 in store")

	// 反向再改第二份列表的第二项，第一份列表的第二项与库内仍不变。
	tamperExchangeResult(&list2[1])
	assertExchangeMatches(t, list1[1], want2, "list1 item after list2 tampered")
	g2b, _ := f.s.GetExchange(doc, f.pid, want2.ID)
	assertExchangeMatches(t, g2b, want2, "exchange 2 in store after second list tampered")
}

// ---- 接收方取包：包视图独立于库内交换，多次取包彼此独立 ----

func TestFetchedPackageIsDetached(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag, f.ord}, "req-iso-fetch")
	if err != nil {
		t.Fatal(err)
	}

	d1, err := f.s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotDelivery(d1)
	auditBefore := len(mustAudit(t, f.s, f.pid))

	// 再取一份。
	d2, err := f.s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 改第一份包中的记录内容、标识、版本与集合：第二份保持取得时内容。
	tamperDeliveryResult(&d1)
	assertDeliveryMatches(t, d2, want, "second delivery after first tampered")

	// 再次取包仍是创建时固化的原内容与原摘要、原状态。
	d3, err := f.s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertDeliveryMatches(t, d3, want, "re-fetch after tampering held delivery")

	// 内部使用者查看原交换：原内容与摘要不受接收方本地修改影响。
	g, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, g, x, "internal view after receiver tampered delivery")

	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("local package tamper changed audit count: before=%d after=%d", auditBefore, got)
	}
}

// ---- 本地改过摘要再交回执：ErrConflict、保持待回执、无回执审计；
//      使用原摘要仍可正常登记 ----

func TestTamperedDigestReceiptConflictsButOriginalStillWorks(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag, f.ord}, "req-iso-digest")
	if err != nil {
		t.Fatal(err)
	}
	originalDigest := x.Digest

	d, err := f.s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 调用方在手中把摘要改成另一个值，并顺手改了包内容。
	d.Digest = "caller-local-digest-value"
	d.Package.Records[0].Content = "本地改过的内容"

	if receiptAuditCount(t, f.s, f.pid) != 0 {
		t.Fatal("unexpected receipt audit before any registration")
	}

	// 用改过的摘要登记：ErrConflict。
	if _, err := f.s.SubmitReceipt(rcv, x.ID, d.Digest, ReceiptAccepted, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("submit with tampered digest err = %v, want ErrConflict", err)
	}

	// 原交换继续保持待回执、无回执，摘要与包内容不变。
	g, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != ExchangePending || g.Receipt != nil {
		t.Fatalf("exchange changed after conflicting receipt: %+v", g)
	}
	assertExchangeMatches(t, g, x, "exchange after conflicting receipt")
	if c := receiptAuditCount(t, f.s, f.pid); c != 0 {
		t.Fatalf("conflicting receipt added audit events: receipt audits = %d, want 0", c)
	}

	// 再次取包仍给出原摘要与待回执状态。
	d2, err := f.s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Digest != originalDigest || d2.Status != ExchangePending {
		t.Fatalf("re-fetch after conflict: digest=%q status=%q", d2.Digest, d2.Status)
	}

	// 使用原摘要正常登记仍应成功——先前的本地修改没有让登记能力失效。
	conf, err := f.s.SubmitReceipt(rcv, x.ID, originalDigest, ReceiptRejected, "接收方核对后真实拒绝")
	if err != nil {
		t.Fatalf("submit with original digest after local tamper: %v", err)
	}
	if conf.Status != ExchangeRejected || conf.Outcome != ReceiptRejected {
		t.Fatalf("bad confirmation: %+v", conf)
	}
	g2, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g2.Status != ExchangeRejected || g2.Receipt == nil ||
		g2.Receipt.Outcome != ReceiptRejected || g2.Receipt.Reason != "接收方核对后真实拒绝" {
		t.Fatalf("genuine receipt not stored: %+v", g2)
	}
	if c := receiptAuditCount(t, f.s, f.pid); c != 1 {
		t.Fatalf("receipt audits = %d, want 1", c)
	}
}

// ---- 已登记拒绝回执：查询结果中改动回执结果/原因/登记时间不影响正式登记 ----

func TestRegisteredReceiptIsDetachedFromQueryResults(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag, f.ord}, "req-iso-receipt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "实际登记的拒绝原因"); err != nil {
		t.Fatal(err)
	}
	f.clk.t = f.clk.t.Add(time.Hour)

	g1, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotExchange(g1)
	if want.Receipt == nil || want.Status != ExchangeRejected ||
		want.Receipt.Outcome != ReceiptRejected || want.Receipt.Reason != "实际登记的拒绝原因" {
		t.Fatalf("baseline receipt wrong: %+v", want)
	}
	registeredAt := want.Receipt.RegisteredAt

	auditBefore := len(mustAudit(t, f.s, f.pid))

	// 在查询结果中改动回执结果、拒绝原因、登记时间与状态。
	g1.Status = ExchangeAccepted
	g1.Receipt.Outcome = ReceiptAccepted
	g1.Receipt.Reason = "本地改过的原因"
	g1.Receipt.RegisteredAt = time.Date(1999, 9, 9, 9, 9, 9, 0, time.UTC)

	// 另一份查询结果保留实际登记的回执。
	g2, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, g2, want, "second GetExchange after receipt tampered in first")
	if !g2.Receipt.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("registered-at changed: %v, want %v", g2.Receipt.RegisteredAt, registeredAt)
	}

	// 列表中的同一份交换也保留实际登记回执。
	lst, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(lst) != 1 {
		t.Fatalf("list len = %d, want 1", len(lst))
	}
	assertExchangeMatches(t, lst[0], want, "ListExchanges item after receipt tampered in Get result")

	// 再改列表项中的回执，正式交换仍不变。
	lst[0].Receipt.Outcome = ReceiptAccepted
	lst[0].Receipt.Reason = "列表里本地改的原因"
	g3, _ := f.s.GetExchange(doc, f.pid, x.ID)
	assertExchangeMatches(t, g3, want, "GetExchange after receipt tampered in list item")

	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("local receipt tamper changed audit count: before=%d after=%d", auditBefore, got)
	}
}

// ---- 待回执交换：在手中结果里补造假回执，重新查询仍没有回执 ----

func TestFabricatedReceiptOnPendingExchangeDoesNotPersist(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag, f.ord}, "req-iso-noreceipt")
	if err != nil {
		t.Fatal(err)
	}

	g, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != ExchangePending || g.Receipt != nil {
		t.Fatalf("new exchange should have no receipt: %+v", g)
	}

	// 调用方在手中的结果里补上回执信息。
	g.Status = ExchangeRejected
	g.Receipt = &Receipt{
		Outcome:      ReceiptRejected,
		Reason:       "手中补造的拒绝",
		RegisteredAt: time.Date(2003, 5, 6, 7, 8, 9, 0, time.UTC),
	}

	// 重新查询：仍无回执、仍待回执。
	g2, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g2.Status != ExchangePending || g2.Receipt != nil {
		t.Fatalf("fabricated receipt leaked into store: %+v", g2)
	}

	// 列表结果中补造假回执同样无效。
	lst, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(lst) != 1 || lst[0].Receipt != nil || lst[0].Status != ExchangePending {
		t.Fatalf("list reflects fabricated receipt: %+v", lst)
	}
	lst[0].Status = ExchangeAccepted
	lst[0].Receipt = &Receipt{Outcome: ReceiptAccepted, RegisteredAt: time.Now()}

	g3, _ := f.s.GetExchange(doc, f.pid, x.ID)
	if g3.Status != ExchangePending || g3.Receipt != nil {
		t.Fatalf("fabricated receipt via list leaked into store: %+v", g3)
	}

	// 接收方取包看到的仍是待回执。
	d, err := f.s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != ExchangePending {
		t.Fatalf("delivery status = %q, want %q", d.Status, ExchangePending)
	}

	// 自始至终没有任何回执审计。
	if c := receiptAuditCount(t, f.s, f.pid); c != 0 {
		t.Fatalf("receipt audits = %d, want 0", c)
	}
}

// ---- 端到端：从各入口取得并篡改的结果，即使触发一次真实原子写盘并重开，
//      库内交换、摘要、回执与审计仍保持原样 ----

func TestLocalMutationsDoNotPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "重开隔离患者")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	recD, _ := s.CreateDraft(doc, p.ID, enc.ID, Diagnosis, "诊断内容")
	if _, err := s.ActivateRecord(doc, recD.ID); err != nil {
		t.Fatal(err)
	}
	recO, _ := s.CreateDraft(doc, p.ID, enc.ID, Order, "医嘱内容")
	if _, err := s.ActivateRecord(doc, recO.ID); err != nil {
		t.Fatal(err)
	}
	a, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: enc.ID, Category: Diagnosis}, {EncounterID: enc.ID, Category: Order}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID,
		[]ID{recD.ID, recO.ID}, "req-reopen-iso")
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotExchange(x)

	// 从每一个返回入口取得结果并全部在本地篡改。
	g, err := s.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	tamperExchangeResult(&g)

	lst, err := s.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	tamperExchangeResult(&lst[0])

	retry, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID,
		[]ID{recO.ID, recD.ID}, "req-reopen-iso")
	if err != nil {
		t.Fatal(err)
	}
	tamperExchangeResult(&retry)

	d, err := s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	tamperDeliveryResult(&d)

	// 触发一次真实的整体原子写盘（幂等重试会克隆当前状态并落盘）。
	again, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID,
		[]ID{recD.ID, recO.ID}, "req-reopen-iso")
	if err != nil {
		t.Fatalf("retry that forces persist: %v", err)
	}
	assertExchangeMatches(t, again, want, "force-persist retry still returns original")

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	assertExchangeMatches(t, got, want, "exchange after reopen")

	gotList, err := s2.ListExchanges(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotList) != 1 {
		t.Fatalf("list after reopen len = %d, want 1", len(gotList))
	}
	assertExchangeMatches(t, gotList[0], want, "list after reopen")

	acts := auditActions(mustAudit(t, s2, p.ID))
	if acts[ActionExchanged] != 1 {
		t.Fatalf("exchange_created audits after reopen = %d, want 1", acts[ActionExchanged])
	}
	if acts[ActionReceipted] != 0 {
		t.Fatalf("receipt audits after reopen = %d, want 0", acts[ActionReceipted])
	}
}
