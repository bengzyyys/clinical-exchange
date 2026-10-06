package clinical

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归“创建交换”的授权时间判定：内部使用者提交打包请求后，若因同一
// 存储上的其他操作而等待，能否创建一律按等待结束、真正开始核对本次授权的
// 时刻决定，而不是请求发出的时刻。时间窗沿用半开区间 [开始, 截止)：
// 开始时刻允许，恰好截止时刻拒绝。
//
// 这些用例刻意把除“核对时刻”外的条件都做成合法：请求号从未成功创建过
// 交换、患者未停用、所选诊断已生效且被请求绑定的那一条授权覆盖、身份与
// 其余参数均合法，避免把授权时间问题混成别的拒绝原因。

// exchangeTimingFixture 准备一条已生效诊断，以及同患者、同接收方、覆盖同一
// 条诊断的两条授权：绑定授权窗口为 [09:00,10:00)，另有一条更晚到期的授权
// （[09:00,22:00)），用于证明绑定授权到期时不能自动改用另一条有效授权。
type exchangeTimingFixture struct {
	s          *Store
	clk        *fakeClock
	pid, eid   ID
	rec        ID
	v1         Version
	auth       ID // 绑定授权：[09:00, 10:00)
	otherAuth  ID // 同患者/接收方、覆盖相同记录、更晚到期：[09:00, 22:00)
	start, end time.Time
}

func setupExchangeTiming(t *testing.T) exchangeTimingFixture {
	t.Helper()
	s, clk := newTestStore(t)

	start := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	// 数据准备发生在授权窗口开始之前：授权窗口与建数据时的时钟无关。
	clk.t = start.Add(-time.Hour)

	pid, eid := setupPatientEncounter(t, s)
	draft, err := s.CreateDraft(doc, pid, eid, Diagnosis, "创建交换时刻回归：诊断 v1 内容")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, draft.ID)
	if err != nil {
		t.Fatal(err)
	}

	a, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}}, start, end.Add(12*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	return exchangeTimingFixture{
		s: s, clk: clk, pid: pid, eid: eid, rec: draft.ID, v1: v1,
		auth: a.ID, otherAuth: other.ID, start: start, end: end,
	}
}

// createExchangeWhileStoreBusy 模拟打包请求发出后被同一存储上的其他操作阻塞：
// 先占用存储互斥锁，再在阻塞期间把时钟推进到 checkAt，随后放行创建。
// CreateExchange 进入临界区、真正开始核对本次授权时才取样，因此是否允许
// 创建按 checkAt 判断，与请求发出时刻（调用方开始等待的时刻）无关。
func createExchangeWhileStoreBusy(f *exchangeTimingFixture, requestAt, checkAt time.Time, req string) (Exchange, error) {
	s, clk := f.s, f.clk
	s.mu.Lock()
	// 在持锁状态下把时钟置为“请求发出时刻”：随后 goroutine 进入
	// CreateExchange 后只能阻塞在这把锁上，整个等待期间时钟都停留在
	// requestAt，直到放行前一刻才推进到核对时刻 checkAt。
	clk.t = requestAt
	type outcome struct {
		x   Exchange
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		x, err := s.CreateExchange(doc, f.pid, rcv.ID, f.auth, []ID{f.rec}, req)
		done <- outcome{x, err}
	}()
	// 给创建 goroutine 留出进入 CreateExchange 并阻塞在锁上的时间，确保请求
	// 是在时钟推进（即“其他操作完成、本次核对开始”）之前发出的。
	time.Sleep(50 * time.Millisecond)
	clk.t = checkAt
	s.mu.Unlock()
	o := <-done
	return o.x, o.err
}

func countExchangeCreateAudits(t *testing.T, s *Store, pid ID) int {
	t.Helper()
	n := 0
	for _, ev := range mustAudit(t, s, pid) {
		if ev.Action == ActionExchanged {
			n++
		}
	}
	return n
}

func exchangeCount(t *testing.T, s *Store, pid ID) int {
	t.Helper()
	xs, err := s.ListExchanges(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	return len(xs)
}

// 主回归：核对时刻（而非请求时刻）决定首次创建能否成功。
func TestCreateExchangeJudgesAuthorizationAtCheckTimeNotRequestTime(t *testing.T) {
	f := setupExchangeTiming(t)

	// 初始没有任何交换与交换创建审计。
	if n := exchangeCount(t, f.s, f.pid); n != 0 {
		t.Fatalf("initial exchange count = %d, want 0", n)
	}
	wantExchanges := 0
	if n := countExchangeCreateAudits(t, f.s, f.pid); n != 0 {
		t.Fatalf("initial exchange_created audit count = %d, want 0", n)
	}

	// 情形 A：请求在 09:59（窗口内）发出，等待期间恰好到 10:00 截止时刻才
	// 开始核对——发出时尚有权限，但核对时绑定授权已失效，必须拒绝。
	// 另一条覆盖相同记录、10:00 仍有效的授权不能替代绑定授权。
	x, err := createExchangeWhileStoreBusy(&f,
		f.end.Add(-time.Minute), f.end, "req-expire-during-wait")
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("binding expired during wait: err = %v, want ErrAccessDenied", err)
	}
	if x.ID != "" || !x.CreatedAt.IsZero() {
		t.Fatalf("denied create must return the zero exchange, got %+v", x)
	}
	if n := exchangeCount(t, f.s, f.pid); n != wantExchanges {
		t.Fatalf("denied create persisted an exchange: count = %d, want %d", n, wantExchanges)
	}
	if n := countExchangeCreateAudits(t, f.s, f.pid); n != wantExchanges {
		t.Fatalf("denied create added an exchange_created audit: %d, want %d", n, wantExchanges)
	}

	// 情形 B：请求在 08:59（授权尚未开始）发出，等待到 09:00 开始时刻才
	// 核对，且尚未到截止时刻——发出时无权限不能成为提前拒绝的依据，应成功。
	x, err = createExchangeWhileStoreBusy(&f,
		f.start.Add(-time.Minute), f.start, "req-start-during-wait")
	if err != nil {
		t.Fatalf("check exactly at grant start must succeed: %v", err)
	}
	wantExchanges++
	assertPendingExchangePackedAtCheck(t, f, x, f.start)

	// 内部使用者能从患者的交换列表中查到同一份正式交换（不是另一份）。
	view, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatalf("get created exchange: %v", err)
	}
	if view.ID != x.ID || view.Digest != x.Digest || !view.CreatedAt.Equal(x.CreatedAt) ||
		view.Status != ExchangePending || len(view.Package.Records) != 1 {
		t.Fatalf("stored exchange differs from the one returned:\n%+v\n%+v", x, view)
	}
	var listed bool
	for _, lx := range mustList(t, f.s, f.pid) {
		if lx.ID == x.ID {
			if lx.Digest != x.Digest || !lx.CreatedAt.Equal(x.CreatedAt) {
				t.Fatalf("listed exchange differs from created exchange: %+v", lx)
			}
			listed = true
		}
	}
	if !listed {
		t.Fatal("created exchange not found in patient exchange list")
	}
	// 新增的交换创建审计恰好对应这份交换，发生时间与交换创建时间一致，
	// 也就是核对时刻 09:00（而不是请求发出的 08:59）。
	var createEv AuditEvent
	var foundCreateAudit bool
	for _, ev := range mustAudit(t, f.s, f.pid) {
		if ev.Action == ActionExchanged && ev.ObjectID == x.ID {
			createEv, foundCreateAudit = ev, true
		}
	}
	if !foundCreateAudit {
		t.Fatal("no exchange_created audit corresponds to the new exchange")
	}
	if !createEv.OccurredAt.Equal(x.CreatedAt) || !createEv.OccurredAt.Equal(f.start) {
		t.Fatalf("audit time %v must equal exchange created-at %v (%v)",
			createEv.OccurredAt, x.CreatedAt, f.start)
	}
	if createEv.ObjectType != "exchange" || createEv.ActorID != doc.ID || createEv.PatientID != f.pid {
		t.Fatalf("exchange create audit wrong: %+v", createEv)
	}
	if n := countExchangeCreateAudits(t, f.s, f.pid); n != wantExchanges {
		t.Fatalf("exchange_created audits = %d, want %d", n, wantExchanges)
	}

	// 情形 C：相邻两个时刻的边界——截止前一纳秒允许，恰好截止拒绝（均在
	// 一段等待之后于对应时刻才开始核对）。
	before, err := createExchangeWhileStoreBusy(&f,
		f.end.Add(-2*time.Minute), f.end.Add(-time.Nanosecond), "req-just-before-expiry")
	if err != nil {
		t.Fatalf("check one nanosecond before expiry must succeed: %v", err)
	}
	wantExchanges++
	assertPendingExchangePackedAtCheck(t, f, before, f.end.Add(-time.Nanosecond))

	if _, err := createExchangeWhileStoreBusy(&f,
		f.end.Add(-time.Minute), f.end, "req-exactly-at-expiry"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("check exactly at expiry must be denied, got %v", err)
	}
	if n := exchangeCount(t, f.s, f.pid); n != wantExchanges {
		t.Fatalf("denial at expiry changed exchange count: %d, want %d", n, wantExchanges)
	}
	if n := countExchangeCreateAudits(t, f.s, f.pid); n != wantExchanges {
		t.Fatalf("denial at expiry changed exchange_created audit count: %d, want %d", n, wantExchanges)
	}

	// 情形 D：即使同一患者、同一接收方另有一条仍有效且覆盖相同记录的授权
	// （[09:00,22:00)，此刻仍有效），绑定授权到期的请求也必须拒绝，
	// 不能自动改用那条授权。
	f.clk.t = f.end.Add(time.Minute)
	if _, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth,
		[]ID{f.rec}, "req-no-substitute-auth"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expired binding must not be substituted by another grant: %v", err)
	}
	if n := exchangeCount(t, f.s, f.pid); n != wantExchanges {
		t.Fatalf("attempt with expired binding changed exchange count: %d, want %d", n, wantExchanges)
	}
	if n := countExchangeCreateAudits(t, f.s, f.pid); n != wantExchanges {
		t.Fatalf("attempt with expired binding added an audit: %d, want %d", n, wantExchanges)
	}

	// 被时间条件拒绝后，原有记录、授权与此前已创建的交换都保持提交前内容。
	assertStateUntouchedByDenials(t, f, x)
}

// 无等待的直接调用也遵守同一条半开边界：截止前一纳秒允许、恰好截止拒绝。
// 与排队用例互为印证：判定只看真正开始核对的时刻。
func TestCreateExchangeHalfOpenBoundaryWithoutWaiting(t *testing.T) {
	f := setupExchangeTiming(t)

	f.clk.t = f.end.Add(-time.Nanosecond)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth, []ID{f.rec}, "req-direct-before")
	if err != nil {
		t.Fatalf("create one nanosecond before expiry: %v", err)
	}
	if !x.CreatedAt.Equal(f.end.Add(-time.Nanosecond)) {
		t.Fatalf("created-at %v, want %v", x.CreatedAt, f.end.Add(-time.Nanosecond))
	}

	f.clk.t = f.end
	if _, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth,
		[]ID{f.rec}, "req-direct-at-end"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("create exactly at expiry must be denied, got %v", err)
	}
	if n := exchangeCount(t, f.s, f.pid); n != 1 {
		t.Fatalf("exchange count after boundary = %d, want 1", n)
	}
	if n := countExchangeCreateAudits(t, f.s, f.pid); n != 1 {
		t.Fatalf("exchange_created audits after boundary = %d, want 1", n)
	}
}

// assertPendingExchangePackedAtCheck 断言一次成功创建：得到待回执交换，创建
// 时间等于核对时刻，包内固化所选记录创建时的当前生效版本（这里为 v1）。
func assertPendingExchangePackedAtCheck(t *testing.T, f exchangeTimingFixture, x Exchange, checkAt time.Time) {
	t.Helper()
	if x.ID == "" {
		t.Fatal("exchange id empty on successful create")
	}
	if x.Status != ExchangePending || x.Receipt != nil {
		t.Fatalf("new exchange must be pending_receipt with no receipt: %+v", x)
	}
	if x.Digest == "" {
		t.Fatal("digest empty")
	}
	if !x.CreatedAt.Equal(checkAt) {
		t.Fatalf("created-at %v must be the check time %v, not the request time", x.CreatedAt, checkAt)
	}
	if x.Package.PatientID != f.pid || x.Package.ReceiverID != rcv.ID {
		t.Fatalf("package header wrong: %+v", x.Package)
	}
	if len(x.Package.Records) != 1 {
		t.Fatalf("packaged records = %d, want 1", len(x.Package.Records))
	}
	pr := x.Package.Records[0]
	if pr.RecordID != f.rec || pr.VersionID != f.v1.ID || pr.Version != 1 ||
		pr.Content != f.v1.Content || !pr.EffectiveAt.Equal(f.v1.CreatedAt) ||
		pr.EncounterID != f.eid || pr.Category != Diagnosis {
		t.Fatalf("package must freeze the current effective version at create: %+v (want %+v)", pr, f.v1)
	}
}

// assertStateUntouchedByDenials 断言被时间条件拒绝后：没有凭空新增交换或
// 审计，绑定授权仍是原窗口且未被撤回，所选记录仍是原当前生效版本，此前
// 已成功创建的交换仍原样可查。
func assertStateUntouchedByDenials(t *testing.T, f exchangeTimingFixture, earlier Exchange) {
	t.Helper()
	ga, err := f.s.GetAuthorization(doc, f.pid, f.auth)
	if err != nil {
		t.Fatalf("get binding authorization: %v", err)
	}
	if ga.RevokedAt != nil || !ga.StartsAt.Equal(f.start) || !ga.ExpiresAt.Equal(f.end) {
		t.Fatalf("binding authorization changed after denials: %+v", ga)
	}

	hists, err := f.s.EncounterRecords(doc, f.pid, f.eid)
	if err != nil {
		t.Fatalf("encounter records: %v", err)
	}
	var found bool
	for _, h := range hists {
		if h.Record.ID == f.rec {
			found = true
			if h.CurrentVersion == nil || h.CurrentVersion.ID != f.v1.ID ||
				h.CurrentVersion.Number != 1 || h.CurrentVersion.Content != f.v1.Content {
				t.Fatalf("record changed after denials: %+v", h.CurrentVersion)
			}
		}
	}
	if !found {
		t.Fatal("selected record missing after denials")
	}

	got, err := f.s.GetExchange(doc, f.pid, earlier.ID)
	if err != nil {
		t.Fatalf("previously created exchange no longer readable: %v", err)
	}
	if got.Digest != earlier.Digest || got.Status != ExchangePending ||
		!got.CreatedAt.Equal(earlier.CreatedAt) || len(got.Package.Records) != 1 {
		t.Fatalf("previously created exchange changed after denials: %+v", got)
	}
}

func mustList(t *testing.T, s *Store, pid ID) []Exchange {
	t.Helper()
	xs, err := s.ListExchanges(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	return xs
}
