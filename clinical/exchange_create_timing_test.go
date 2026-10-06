package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件回归保障“首次创建交换”的时间判定：内部使用者提交打包请求后，即使因
// 同一存储上的其他操作而等待，能否创建也按真正开始核对本次绑定授权的时刻
// （进入 mutate 临界区后才取样）决定，而不是请求发出或调用方拿到返回结果的
// 时刻。时间窗沿用半开区间 [开始, 截止)：开始时刻允许，恰好截止时刻拒绝。
//
// 为了不把授权时间问题混成别的拒绝原因，全部用例都满足：
//   - 使用从未成功创建过交换的请求号（首次被拒不占用请求号，在首个用例中
//     再用同一请求号于窗口内重试成功来显式验证）；
//   - 患者始终未停用；
//   - 所选诊断与医嘱都已经生效；
//   - 两条记录都确由请求绑定的那一条授权自身覆盖（整类范围），不借其他授权；
//   - 发起身份为内部使用者，患者/接收方/授权/记录归属与其余参数均合法。

// createExchangeTimingFixture 是时间判定专用夹具：未停用患者、一次就诊、
// 一条已生效诊断、一条已生效医嘱，以及一条同时覆盖两条记录的整类授权。
type createExchangeTimingFixture struct {
	s               *Store
	clk             *fakeClock
	pid, eid        ID
	diag, ord       ID
	diagVer, ordVer ID
	bound           ID
	start, end      time.Time
}

// timingBase 与 newTestStore 的固定时钟起点一致：2026-01-01 09:00 UTC。
var timingBase = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

func setupCreateExchangeTiming(t *testing.T, start, end time.Time) createExchangeTimingFixture {
	t.Helper()
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	dr, err := s.CreateDraft(doc, pid, eid, Diagnosis, "时间判定诊断v1")
	if err != nil {
		t.Fatalf("create diagnosis draft: %v", err)
	}
	dv, err := s.ActivateRecord(doc, dr.ID)
	if err != nil {
		t.Fatalf("activate diagnosis: %v", err)
	}
	orr, err := s.CreateDraft(doc, pid, eid, Order, "时间判定医嘱v1")
	if err != nil {
		t.Fatalf("create order draft: %v", err)
	}
	ov, err := s.ActivateRecord(doc, orr.ID)
	if err != nil {
		t.Fatalf("activate order: %v", err)
	}

	a, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{
			{EncounterID: eid, Category: Diagnosis},
			{EncounterID: eid, Category: Order},
		}, start, end)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	return createExchangeTimingFixture{
		s: s, clk: clk, pid: pid, eid: eid,
		diag: dr.ID, ord: orr.ID, diagVer: dv.ID, ordVer: ov.ID,
		bound: a.ID, start: start.UTC(), end: end.UTC(),
	}
}

// createExchangeWhileStoreBusy 模拟创建请求发出后被同一存储上的其他操作阻塞：
// 先占用存储互斥锁，待创建 goroutine 发出请求并阻塞后，把时钟推进到 advanceTo
// （即“等待结束、真正开始核对本次授权”的时刻），再放行。返回创建结果与错误。
func createExchangeWhileStoreBusy(f *createExchangeTimingFixture, advanceTo time.Time, authID ID, req string) (Exchange, error) {
	f.s.mu.Lock()
	type outcome struct {
		x   Exchange
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, authID, []ID{f.diag, f.ord}, req)
		done <- outcome{x, err}
	}()
	// 给创建 goroutine 留出进入 CreateExchange 并阻塞在锁上的时间，确保请求
	// 是在时钟推进之前发出的——核对只能采用 advanceTo 这一时刻。
	time.Sleep(50 * time.Millisecond)
	f.clk.t = advanceTo
	f.s.mu.Unlock()
	o := <-done
	return o.x, o.err
}

// setClock 直接改写夹具存储注入时钟的当前时刻。
func (f *createExchangeTimingFixture) setClock(t time.Time) {
	f.clk.t = t
}

// assertTimingDenied 断言被时间条件拒绝：错误为 ErrAccessDenied，返回值为空，
// 不携带交换标识、状态、摘要、创建时间或任何包内容。
func assertTimingDenied(t *testing.T, x Exchange, err error) {
	t.Helper()
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
	if x.ID != "" || x.Status != "" || x.Digest != "" || x.AuthorizationID != "" ||
		x.CreatorID != "" || x.RequestID != "" || !x.CreatedAt.IsZero() ||
		x.Package.PatientID != "" || x.Package.ReceiverID != "" || len(x.Package.Records) != 0 {
		t.Fatalf("denied create must return an empty exchange, got %+v", x)
	}
}

// assertTimingSucceeded 断言允许创建时的完整结果：
// 待回执交换、包内固化所选记录的当前生效版本、可从患者交换列表查到同一份
// 正式交换，且交换创建审计对应这份交换、发生时间与交换创建时间一致。
func assertTimingSucceeded(t *testing.T, f *createExchangeTimingFixture, x Exchange, bound ID, checkAt time.Time) {
	t.Helper()
	if x.ID == "" {
		t.Fatal("exchange was not created")
	}
	if x.Status != ExchangePending || x.Receipt != nil {
		t.Fatalf("new exchange must be pending receipt with no receipt: %+v", x)
	}
	if x.PatientID != f.pid || x.ReceiverID != rcv.ID {
		t.Fatalf("exchange header wrong: %+v", x)
	}
	if x.AuthorizationID != bound {
		t.Fatalf("exchange bound to %q, want %q", x.AuthorizationID, bound)
	}
	if x.Digest == "" {
		t.Fatal("digest must be computed")
	}
	if !x.CreatedAt.Equal(checkAt) {
		t.Fatalf("exchange created at %v, want check time %v", x.CreatedAt, checkAt)
	}
	if x.Package.PatientID != f.pid || x.Package.ReceiverID != rcv.ID {
		t.Fatalf("package header wrong: %+v", x.Package)
	}
	if len(x.Package.Records) != 2 {
		t.Fatalf("package records = %d, want 2", len(x.Package.Records))
	}
	wantRecord := map[ID]struct {
		versionID ID
		content   string
	}{
		f.diag: {f.diagVer, "时间判定诊断v1"},
		f.ord:  {f.ordVer, "时间判定医嘱v1"},
	}
	for _, pr := range x.Package.Records {
		want, ok := wantRecord[pr.RecordID]
		if !ok {
			t.Fatalf("unexpected packaged record %q", pr.RecordID)
		}
		if pr.Version != 1 || pr.VersionID != want.versionID || pr.Content != want.content {
			t.Fatalf("package must freeze the current effective version at create: %+v", pr)
		}
		if pr.EncounterID != f.eid {
			t.Fatalf("packaged record encounter wrong: %+v", pr)
		}
	}

	// 同一份正式交换必须能从患者的交换列表中查到（标识、绑定授权、摘要、
	// 状态、创建时间、包内容均一致）。
	listed, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatalf("list exchanges: %v", err)
	}
	var found *Exchange
	for i := range listed {
		if listed[i].ID == x.ID {
			found = &listed[i]
		}
	}
	if found == nil {
		t.Fatalf("created exchange %q not found in patient exchange list: %+v", x.ID, listed)
	}
	if found.Digest != x.Digest || found.Status != x.Status ||
		!found.CreatedAt.Equal(x.CreatedAt) || found.AuthorizationID != bound ||
		len(found.Package.Records) != 2 {
		t.Fatalf("listed exchange differs from the created one:\n%+v\n%+v", x, found)
	}

	// 交换创建审计：恰好一条对应这份交换，动作、对象类型、对象、身份、患者
	// 正确，发生时间与交换创建时间一致。
	var events []AuditEvent
	for _, ev := range mustAudit(t, f.s, f.pid) {
		if ev.Action == ActionExchanged && ev.ObjectID == x.ID {
			events = append(events, ev)
		}
	}
	if len(events) != 1 {
		t.Fatalf("exchange_created audit events for %q = %d, want 1", x.ID, len(events))
	}
	ev := events[0]
	if ev.ObjectType != "exchange" || ev.ActorID != doc.ID || ev.PatientID != f.pid {
		t.Fatalf("exchange audit wrong: %+v", ev)
	}
	if !ev.OccurredAt.Equal(x.CreatedAt) {
		t.Fatalf("audit occurred at %v, want exchange created at %v", ev.OccurredAt, x.CreatedAt)
	}
}

// storeFingerprint 是比对“被拒前后状态不变”用的完整指纹：已有交换、审计、
// 授权、记录完整历史。各项都来自只读查询的独立副本，可直接深度比较。
type storeFingerprint struct {
	exchanges []Exchange
	audits    []AuditEvent
	grants    []Authorization
	histories []RecordHistory
}

func fingerprintStore(t *testing.T, f *createExchangeTimingFixture) storeFingerprint {
	t.Helper()
	xs, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatalf("list exchanges: %v", err)
	}
	grants, err := f.s.ListAuthorizations(doc, f.pid, "")
	if err != nil {
		t.Fatalf("list authorizations: %v", err)
	}
	hist, err := f.s.EncounterRecords(doc, f.pid, f.eid)
	if err != nil {
		t.Fatalf("encounter records: %v", err)
	}
	return storeFingerprint{
		exchanges: xs,
		audits:    mustAudit(t, f.s, f.pid),
		grants:    grants,
		histories: hist,
	}
}

func assertFingerprintEqual(t *testing.T, before, after storeFingerprint) {
	t.Helper()
	if !reflect.DeepEqual(before.exchanges, after.exchanges) {
		t.Fatalf("exchanges changed after denied create:\nbefore=%+v\nafter=%+v", before.exchanges, after.exchanges)
	}
	if !reflect.DeepEqual(before.audits, after.audits) {
		t.Fatalf("audit events changed after denied create:\nbefore=%+v\nafter=%+v", before.audits, after.audits)
	}
	if !reflect.DeepEqual(before.grants, after.grants) {
		t.Fatalf("authorizations changed after denied create:\nbefore=%+v\nafter=%+v", before.grants, after.grants)
	}
	if !reflect.DeepEqual(before.histories, after.histories) {
		t.Fatalf("record histories changed after denied create:\nbefore=%+v\nafter=%+v", before.histories, after.histories)
	}
}

// grantOtherCoveringSameRecords 再建一条同患者、同接收方、覆盖同样两条记录的
// 整类授权，用于验证绑定授权到期时不会自动改用它。
func grantOtherCoveringSameRecords(t *testing.T, f *createExchangeTimingFixture, start, end time.Time) ID {
	t.Helper()
	a, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{
			{EncounterID: f.eid, Category: Diagnosis},
			{EncounterID: f.eid, Category: Order},
		}, start, end)
	if err != nil {
		t.Fatalf("grant other authorization: %v", err)
	}
	return a.ID
}

// ---- 核对时刻，而不是请求发出时刻 ----

// 请求在截止前一分钟（09:59）发出，等待期间恰好到达截止时刻（10:00）才开始
// 核对：绑定授权按半开区间 [09:00,10:00) 已失效，必须返回 ErrAccessDenied，
// 不能因发出时尚有权限而创建交换。即使同一患者、同一接收方另有一条仍有效且
// 覆盖相同记录的授权，也不能改用它。被拒后没有新交换、没有交换创建审计，
// 授权/记录保持原样。随后把时刻拨回窗口内、以同一请求号重试成功且仍绑定原
// 授权，证明首次被拒没有创建交换/审计，也没有占用请求号。
func TestCreateExchangeWaitPastExpiryDeniedAndRequestIDNotConsumed(t *testing.T) {
	start, end := timingBase, timingBase.Add(time.Hour)
	f := setupCreateExchangeTiming(t, start, end)
	other := grantOtherCoveringSameRecords(t, &f, start, end.Add(time.Hour))

	before := fingerprintStore(t, &f)

	// 请求在 09:59 发出，阻塞；核对时刻被推进到恰好 10:00。
	f.setClock(end.Add(-time.Minute))
	x, err := createExchangeWhileStoreBusy(&f, end, f.bound, "req-create-wait-expire")
	assertTimingDenied(t, x, err)

	// 被时间条件拒绝：没有新交换或交换创建审计，原有记录、授权、已有交换
	// （此处此前没有交换）保持提交前内容。
	assertFingerprintEqual(t, before, fingerprintStore(t, &f))
	if xs, _ := f.s.ListExchanges(doc, f.pid); len(xs) != 0 {
		t.Fatalf("denied create persisted an exchange: %+v", xs)
	}

	// 拨回窗口内，以同一请求号、相同参数重试：应作为首次创建成功，且仍绑定
	// 原来那条已在 10:00 失效过的授权——既没有改用另一条有效授权，也说明首次
	// 被拒未占用请求号（否则会按冲突处理）。
	checkAt := end.Add(-2 * time.Minute)
	f.setClock(checkAt)
	retry, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.bound,
		[]ID{f.diag, f.ord}, "req-create-wait-expire")
	if err != nil {
		t.Fatalf("retry inside window after denied attempt: %v", err)
	}
	assertTimingSucceeded(t, &f, retry, f.bound, checkAt)
	if retry.AuthorizationID == other {
		t.Fatal("create must not substitute the other valid authorization")
	}
}

// 反向边界：请求在开始前一分钟（09:59，授权 [10:00,11:00) 尚未开始）发出，
// 等到 10:00 才开始核对且尚未到截止：应成功创建。发出时授权尚未开始不能成为
// 提前拒绝的依据；交换创建时间取核对时刻 10:00。
func TestCreateExchangeWaitUntilStartAllowed(t *testing.T) {
	start, end := timingBase.Add(time.Hour), timingBase.Add(2*time.Hour)
	f := setupCreateExchangeTiming(t, start, end)

	// 请求在开始前一分钟发出，阻塞；核对时刻被推进到恰好开始时刻。
	f.setClock(start.Add(-time.Minute))
	x, err := createExchangeWhileStoreBusy(&f, start, f.bound, "req-create-wait-start")
	if err != nil {
		t.Fatalf("authorization already started at check time must allow create: %v", err)
	}
	assertTimingSucceeded(t, &f, x, f.bound, start)
}

// 不等待时同样遵守半开边界，并区分截止前一纳秒与恰好截止这两个相邻时刻：
// 截止前一纳秒允许创建，恰好截止拒绝；开始时刻允许，开始前一纳秒拒绝。
// 每次允许都新增一份交换与一条交换创建审计；每次拒绝都不新增。
func TestCreateExchangeWindowBoundariesAtNanosecond(t *testing.T) {
	start, end := timingBase, timingBase.Add(time.Hour)
	f := setupCreateExchangeTiming(t, start, end)

	countExchanges := func() int {
		xs, err := f.s.ListExchanges(doc, f.pid)
		if err != nil {
			t.Fatal(err)
		}
		return len(xs)
	}
	countCreateAudits := func() int {
		n := 0
		for _, ev := range mustAudit(t, f.s, f.pid) {
			if ev.Action == ActionExchanged {
				n++
			}
		}
		return n
	}

	// 截止前一纳秒：允许。
	f.setClock(end.Add(-time.Nanosecond))
	x1, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.bound,
		[]ID{f.diag, f.ord}, "req-end-minus-1ns")
	if err != nil {
		t.Fatalf("one nanosecond before expiry must succeed: %v", err)
	}
	assertTimingSucceeded(t, &f, x1, f.bound, end.Add(-time.Nanosecond))
	if countExchanges() != 1 || countCreateAudits() != 1 {
		t.Fatal("allowed boundary create must add exactly one exchange and audit")
	}

	// 恰好截止：拒绝，不新增交换或审计。
	before := fingerprintStore(t, &f)
	f.setClock(end)
	x2, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.bound,
		[]ID{f.diag, f.ord}, "req-at-end")
	assertTimingDenied(t, x2, err)
	assertFingerprintEqual(t, before, fingerprintStore(t, &f))
	if countExchanges() != 1 || countCreateAudits() != 1 {
		t.Fatal("denied create at expiry must add no exchange or audit")
	}

	// 恰好开始：允许，再新增一份交换与一条审计（请求号不同，互不影响）。
	f.setClock(start)
	x3, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.bound,
		[]ID{f.diag, f.ord}, "req-at-start")
	if err != nil {
		t.Fatalf("at start must succeed: %v", err)
	}
	assertTimingSucceeded(t, &f, x3, f.bound, start)
	if countExchanges() != 2 || countCreateAudits() != 2 {
		t.Fatal("allowed create at start must add one exchange and audit")
	}

	// 开始前一纳秒：拒绝，不新增。
	before = fingerprintStore(t, &f)
	f.setClock(start.Add(-time.Nanosecond))
	x4, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.bound,
		[]ID{f.diag, f.ord}, "req-start-minus-1ns")
	assertTimingDenied(t, x4, err)
	assertFingerprintEqual(t, before, fingerprintStore(t, &f))
	if countExchanges() != 2 || countCreateAudits() != 2 {
		t.Fatal("denied create before start must add no exchange or audit")
	}
}

// 被时间条件拒绝时，已存在的交换、记录与授权保持提交前内容：先在窗口内成功
// 创建一份交换（待回执），再让一个全新请求号在等待中跨过截止时刻被拒；随后
// 比对交换（含原包摘要与内容）、审计、授权和记录完整历史逐项不变。另一条仍
// 有效且覆盖相同记录的授权在场，绑定授权到期仍必须拒绝、不得替代。
func TestCreateExchangeDenialPreservesExistingState(t *testing.T) {
	start, end := timingBase, timingBase.Add(time.Hour)
	f := setupCreateExchangeTiming(t, start, end)
	grantOtherCoveringSameRecords(t, &f, start, end.Add(2*time.Hour))

	// 先成功创建一份交换，作为“已有交换”。
	f.setClock(start.Add(30 * time.Minute))
	existing, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.bound,
		[]ID{f.diag, f.ord}, "req-existing-ok")
	if err != nil {
		t.Fatalf("seed exchange: %v", err)
	}
	gotExisting, err := f.s.GetExchange(doc, f.pid, existing.ID)
	if err != nil {
		t.Fatalf("get seed exchange: %v", err)
	}

	before := fingerprintStore(t, &f)

	// 新请求在 09:59 发出，等待期间到达 10:00：核对时绑定授权已失效。
	f.setClock(end.Add(-time.Minute))
	x, err := createExchangeWhileStoreBusy(&f, end, f.bound, "req-denied-keeps-state")
	assertTimingDenied(t, x, err)

	// 交换、审计、授权、记录完整历史逐项保持提交前内容。
	assertFingerprintEqual(t, before, fingerprintStore(t, &f))

	// 已有的那份交换仍可逐字取回，状态/摘要/包内容不变。
	gotAfter, err := f.s.GetExchange(doc, f.pid, existing.ID)
	if err != nil {
		t.Fatalf("get seed exchange after denial: %v", err)
	}
	if !reflect.DeepEqual(gotExisting, gotAfter) {
		t.Fatalf("existing exchange changed:\nbefore=%+v\nafter=%+v", gotExisting, gotAfter)
	}
}
