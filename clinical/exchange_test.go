package clinical

import (
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"
)

// ---- 测试辅助 ----

// setupExchangeScenario 建立患者、一次就诊、一条诊断（v1→v2 更正）与一条医嘱（v1），
// 并为 rcv 授权该就诊的诊断与医嘱。
func setupExchangeScenario(t *testing.T, s *Store, clk *fakeClock) (pid, eid ID, diagID, ordID ID, authID ID) {
	t.Helper()
	pid, eid = setupPatientEncounter(t, s)

	diag, err := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断v1")
	if err != nil {
		t.Fatalf("create diag draft: %v", err)
	}
	if _, err := s.ActivateRecord(doc, diag.ID); err != nil {
		t.Fatalf("activate diag: %v", err)
	}
	if _, err := s.CorrectRecord(doc, diag.ID, 1, "诊断v2", "笔误"); err != nil {
		t.Fatalf("correct diag: %v", err)
	}

	ord, err := s.CreateDraft(doc, pid, eid, Order, "医嘱v1")
	if err != nil {
		t.Fatalf("create order draft: %v", err)
	}
	if _, err := s.ActivateRecord(doc, ord.ID); err != nil {
		t.Fatalf("activate order: %v", err)
	}

	auth, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}, {EncounterID: eid, Category: Order}},
		clk.t.Add(-time.Hour), clk.t.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return pid, eid, diag.ID, ord.ID, auth.ID
}

func countExchanges(s *Store) int {
	c := 0
	_ = s.view(func(snap *snapshot) error { c = len(snap.Exchanges); return nil })
	return c
}

func findExchange(s *Store, id ID) *Exchange {
	var out *Exchange
	_ = s.view(func(snap *snapshot) error {
		out = snap.Exchanges[id]
		return nil
	})
	return out
}

// ---- 成功路径：创建、取包、回执 ----

func TestCreateExchangePackageContentAndReceipt(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid, diagID, ordID, authID := setupExchangeScenario(t, s, clk)

	ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID, ordID}, "req-1")
	if err != nil {
		t.Fatalf("create exchange: %v", err)
	}
	if ex.ID == "" || ex.CreatedAt.IsZero() {
		t.Fatalf("exchange missing id/time: %+v", ex)
	}
	if ex.Status != ExchangePending {
		t.Fatalf("status = %q, want pending", ex.Status)
	}
	if ex.RequesterID != doc.ID || ex.RequestKey != "req-1" {
		t.Fatalf("request metadata wrong: %+v", ex)
	}
	if ex.Package.PatientID != pid || ex.Package.ReceiverID != rcv.ID {
		t.Fatalf("package parties wrong: %+v", ex.Package)
	}
	if len(ex.Package.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(ex.Package.Records))
	}
	byID := map[ID]ExchangeRecord{}
	for _, rec := range ex.Package.Records {
		byID[rec.RecordID] = rec
	}
	diag, ok1 := byID[diagID]
	ord, ok2 := byID[ordID]
	if !ok1 || !ok2 {
		t.Fatalf("package missing records: %+v", ex.Package.Records)
	}
	if diag.EncounterID != eid || diag.Category != Diagnosis || diag.Version != 2 ||
		diag.Content != "诊断v2" || diag.VersionID == "" || diag.EffectiveAt.IsZero() {
		t.Fatalf("diagnosis snapshot wrong: %+v", diag)
	}
	if ord.EncounterID != eid || ord.Category != Order || ord.Version != 1 || ord.Content != "医嘱v1" {
		t.Fatalf("order snapshot wrong: %+v", ord)
	}
	// 摘要为 64 位十六进制（SHA-256）。
	if len(ex.Summary) != 64 {
		t.Fatalf("summary = %q, want 64 hex chars", ex.Summary)
	}
	if _, err := hex.DecodeString(ex.Summary); err != nil {
		t.Fatalf("summary not hex: %v", err)
	}

	// 审计：创建事件一次，身份与时间正确。
	events := mustAudit(t, s, pid)
	var created *AuditEvent
	for i := range events {
		if events[i].Action == ActionExchangeCreated {
			created = &events[i]
		}
	}
	if created == nil {
		t.Fatal("exchange creation audit missing")
	}
	if created.ActorID != doc.ID || created.ObjectType != "exchange" || created.ObjectID != ex.ID {
		t.Fatalf("bad creation audit: %+v", created)
	}
	if !created.OccurredAt.Equal(ex.CreatedAt) {
		t.Fatalf("audit time %v != exchange time %v", created.OccurredAt, ex.CreatedAt)
	}

	// 接收方取包：内容与摘要一致。
	pkg, summary, err := s.FetchExchangePackage(rcv, ex.ID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if summary != ex.Summary {
		t.Fatalf("fetched summary %q != %q", summary, ex.Summary)
	}
	if len(pkg.Records) != 2 {
		t.Fatalf("fetched package wrong: %+v", pkg)
	}
	fetched := map[ID]ExchangeRecord{}
	for _, rec := range pkg.Records {
		fetched[rec.RecordID] = rec
	}
	if fetched[diagID].Content != "诊断v2" {
		t.Fatalf("fetched package wrong: %+v", pkg)
	}

	// 提交接受回执。
	clk.t = clk.t.Add(time.Hour)
	conf, err := s.SubmitReceipt(rcv, ex.ID, summary, ExchangeAccepted, "")
	if err != nil {
		t.Fatalf("submit receipt: %v", err)
	}
	if conf.ExchangeID != ex.ID || conf.Status != ExchangeAccepted || conf.Result != ExchangeAccepted ||
		conf.ActorID != rcv.ID || !conf.At.Equal(clk.t) {
		t.Fatalf("confirmation wrong: %+v", conf)
	}
	// 响应不含受保护内容字段（结构体本身无内容字段；此处确认关键字段为空/正确）。
	if conf.Reason != "" {
		t.Fatalf("accept reason should be empty: %+v", conf)
	}

	// 内部使用者按患者可查原包与回执。
	list, err := s.ListExchanges(doc, pid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != ex.ID {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Status != ExchangeAccepted || list[0].ReceiptResult != ExchangeAccepted ||
		list[0].ReceiptActorID != rcv.ID || list[0].ReceiptAt == nil {
		t.Fatalf("receipt not recorded: %+v", list[0])
	}
	// 其他患者看不到。
	other, _ := s.RegisterPatient(doc, "合成患者乙")
	if list, _ := s.ListExchanges(doc, other.ID); len(list) != 0 {
		t.Fatalf("exchanges leaked across patients: %d", len(list))
	}
}

func TestSubmitRejectedReceiptRequiresReason(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, diagID, _, authID := setupExchangeScenario(t, s, clk)
	ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID}, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	_, summary, _ := s.FetchExchangePackage(rcv, ex.ID)

	if _, err := s.SubmitReceipt(rcv, ex.ID, summary, ExchangeRejected, "  "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank reject reason err = %v, want ErrInvalidArgument", err)
	}
	conf, err := s.SubmitReceipt(rcv, ex.ID, summary, ExchangeRejected, "与记录不符")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if conf.Status != ExchangeRejected || conf.Result != ExchangeRejected || conf.Reason != "与记录不符" {
		t.Fatalf("reject confirmation wrong: %+v", conf)
	}
	got := findExchange(s, ex.ID)
	if got.Status != ExchangeRejected || got.ReceiptReason != "与记录不符" {
		t.Fatalf("stored receipt wrong: %+v", got)
	}
}

// ---- 创建校验：任何失败都不留交换/审计 ----

func TestCreateExchangeValidationFailuresLeaveNothing(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid, diagID, ordID, authID := setupExchangeScenario(t, s, clk)
	other, _ := s.RegisterPatient(doc, "合成患者乙")
	encOther, _ := s.AddEncounter(doc, other.ID, time.Time{})
	recOther, err := s.CreateDraft(doc, other.ID, encOther.ID, Diagnosis, "乙的诊断")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, recOther.ID); err != nil {
		t.Fatal(err)
	}
	draft, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "未生效草稿")

	// 另一条只覆盖诊断的授权（用于“类别不覆盖”场景）。
	diagOnly, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 属于另一患者的授权。
	otherAuth, err := s.Grant(doc, other.ID, rcv.ID,
		[]Scope{{EncounterID: encOther.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 已到期的授权。
	expiredAuth, err := s.Grant(doc, pid, rcvB.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		clk.t.Add(-48*time.Hour), clk.t.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		act  Actor
		pat  ID
		rcv  string
		auth ID
		recs []ID
		key  string
		want error
	}{
		{"receiver cannot create", rcv, pid, rcv.ID, authID, []ID{diagID}, "k", ErrAccessDenied},
		{"empty record set", doc, pid, rcv.ID, authID, nil, "k", ErrInvalidArgument},
		{"blank request key", doc, pid, rcv.ID, authID, []ID{diagID}, "   ", ErrInvalidArgument},
		{"missing record", doc, pid, rcv.ID, authID, []ID{"rec_missing"}, "k", ErrNotFound},
		{"cross-patient record", doc, pid, rcv.ID, authID, []ID{recOther.ID}, "k", ErrMismatchedPatient},
		{"draft record smuggled", doc, pid, rcv.ID, authID, []ID{diagID, draft.ID}, "k", ErrInvalidArgument},
		{"auth belongs to other patient", doc, pid, rcv.ID, otherAuth.ID, []ID{recOther.ID}, "k", ErrMismatchedPatient},
		{"auth belongs to other receiver", doc, pid, rcvB.ID, authID, []ID{diagID}, "k", ErrInvalidArgument},
		{"expired authorization", doc, pid, rcvB.ID, expiredAuth.ID, []ID{diagID}, "k", ErrAccessDenied},
		{"record category not covered", doc, pid, rcv.ID, diagOnly.ID, []ID{ordID}, "k", ErrAccessDenied},
		{"missing authorization", doc, pid, rcv.ID, "auth_missing", []ID{diagID}, "k", ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countExchanges(s)
			_, err := s.CreateExchange(tc.act, tc.pat, tc.rcv, tc.auth, tc.recs, tc.key)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if countExchanges(s) != before {
				t.Fatal("failed create left an exchange")
			}
		})
	}

	// 患者停用后不能新建交换。
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID}, "k"); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("deactivated patient create err = %v, want ErrDeactivated", err)
	}
	if countExchanges(s) != 0 {
		t.Fatal("deactivated create left an exchange")
	}
	// 全部失败都不产生交换审计。
	for _, ev := range mustAudit(t, s, pid) {
		if ev.Action == ActionExchangeCreated {
			t.Fatalf("failed create produced audit: %+v", ev)
		}
	}
}

// ---- 请求号幂等、冲突与并发 ----

func TestCreateExchangeIdempotentRetryAndConflict(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, diagID, ordID, authID := setupExchangeScenario(t, s, clk)

	ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID, ordID}, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	nAudits := len(mustAudit(t, s, pid))

	// 重试：顺序不同、带重复标识，返回原交换且不新增审计。
	retry, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{ordID, diagID, ordID}, "req-1")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.ID != ex.ID || retry.Summary != ex.Summary {
		t.Fatalf("retry returned different exchange: %+v vs %+v", retry, ex)
	}
	if len(mustAudit(t, s, pid)) != nAudits {
		t.Fatal("idempotent retry added audit")
	}
	if countExchanges(s) != 1 {
		t.Fatalf("exchanges = %d, want 1", countExchanges(s))
	}

	// 任一参数变化 → ErrConflict。
	conflicts := []struct {
		name string
		pat  ID
		rcv  string
		auth ID
		recs []ID
	}{
		{"different patient", "pat_other", rcv.ID, authID, []ID{diagID}},
		{"different receiver", pid, rcvB.ID, authID, []ID{diagID}},
		{"different auth", pid, rcv.ID, "auth_other", []ID{diagID}},
		{"different record set", pid, rcv.ID, authID, []ID{diagID}},
	}
	for _, tc := range conflicts {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CreateExchange(doc, tc.pat, tc.rcv, tc.auth, tc.recs, "req-1"); !errors.Is(err, ErrConflict) {
				t.Fatalf("err = %v, want ErrConflict", err)
			}
		})
	}
	// 不同请求号是新请求：即使其余参数相同也创建新交换。
	if _, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID, ordID}, "req-2"); err != nil {
		t.Fatalf("different key should create new exchange: %v", err)
	}
	// 不同内部使用者即使请求号相同也不冲突（请求号按使用者区分）。
	if _, err := s.CreateExchange(doc2, pid, rcv.ID, authID, []ID{diagID, ordID}, "req-1"); err != nil {
		t.Fatalf("requester-scoped key should not conflict: %v", err)
	}
	if countExchanges(s) != 3 {
		t.Fatalf("exchanges = %d, want 3", countExchanges(s))
	}
}

func TestFailedCreateDoesNotConsumeRequestKey(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid, diagID, _, authID := setupExchangeScenario(t, s, clk)
	draft, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "草稿")

	// 第一次夹带草稿失败。
	if _, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID, draft.ID}, "req-x"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
	// 草稿生效后，同一请求号成功。
	if _, err := s.ActivateRecord(doc, draft.ID); err != nil {
		t.Fatal(err)
	}
	ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID, draft.ID}, "req-x")
	if err != nil {
		t.Fatalf("retry after fix: %v", err)
	}
	if ex.Status != ExchangePending {
		t.Fatalf("status = %q", ex.Status)
	}
}

func TestConcurrentCreateSameKeyProducesOneExchange(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, diagID, ordID, authID := setupExchangeScenario(t, s, clk)

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	ids := make(chan ID, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{ordID, diagID}, "req-concurrent")
			if err != nil {
				errs <- err
				return
			}
			ids <- ex.ID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatalf("concurrent create: %v", err)
	}
	seen := map[ID]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("got %d distinct exchange ids, want 1", len(seen))
	}
	if countExchanges(s) != 1 {
		t.Fatalf("exchanges = %d, want 1", countExchanges(s))
	}
}

// ---- 包内容固化：后续更正不改写包 ----

func TestPackageIsFrozenAtCreation(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, diagID, _, authID := setupExchangeScenario(t, s, clk)

	ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID}, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Package.Records[0].Content != "诊断v2" || ex.Package.Records[0].Version != 2 {
		t.Fatalf("unexpected snapshot: %+v", ex.Package.Records[0])
	}

	// 创建后更正为 v3。
	if _, err := s.CorrectRecord(doc, diagID, 2, "诊断v3", "再次更正"); err != nil {
		t.Fatal(err)
	}

	// 取包仍是创建时的 v2。
	pkg, _, err := s.FetchExchangePackage(rcv, ex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Records[0].Content != "诊断v2" || pkg.Records[0].Version != 2 {
		t.Fatalf("package rewritten after correction: %+v", pkg.Records[0])
	}
	// 现有 Read 仍返回当前版本 v3。
	res, err := s.Read(rcv, pid, pkg.Records[0].EncounterID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Content != "诊断v3" || res.Records[0].Version != 3 {
		t.Fatalf("Read should return current version, got %+v", res.Records[0])
	}
}

// ---- 取包：重新校验授权与患者状态 ----

func TestFetchPackageRechecksBinding(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, diagID, _, authID := setupExchangeScenario(t, s, clk)
	ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID}, "req-1")
	if err != nil {
		t.Fatal(err)
	}

	// 其他接收方、不存在的交换：统一拒绝，不泄露内容。
	if _, _, err := s.FetchExchangePackage(rcvB, ex.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other receiver err = %v, want ErrAccessDenied", err)
	}
	if _, _, err := s.FetchExchangePackage(rcv, "exc_missing"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("missing exchange err = %v, want ErrAccessDenied", err)
	}
	if _, _, err := s.FetchExchangePackage(doc, ex.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("internal actor fetch err = %v, want ErrAccessDenied", err)
	}

	// 撤回绑定授权后拒绝。
	if err := s.Revoke(doc, pid, authID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FetchExchangePackage(rcv, ex.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked binding err = %v, want ErrAccessDenied", err)
	}

	// 重新授权后取包成功；但绑定授权仍是被撤回的 authID，新授权不能替代它。
	auth2, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: ex.Package.Records[0].EncounterID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_ = auth2
	if _, _, err := s.FetchExchangePackage(rcv, ex.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other valid auth must not substitute binding: err = %v", err)
	}

	// 新建一份绑定 auth2 的交换，验证到期拒绝。
	ex2, err := s.CreateExchange(doc, pid, rcv.ID, auth2.ID, []ID{diagID}, "req-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FetchExchangePackage(rcv, ex2.ID); err != nil {
		t.Fatalf("fetch within window: %v", err)
	}
	clk.t = clk.t.Add(2 * time.Hour)
	if _, _, err := s.FetchExchangePackage(rcv, ex2.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expired binding err = %v, want ErrAccessDenied", err)
	}

	// 患者停用后拒绝。
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FetchExchangePackage(rcv, ex2.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("deactivated patient err = %v, want ErrAccessDenied", err)
	}
}

// ---- 回执状态机 ----

func TestSubmitReceiptStateMachine(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, diagID, _, authID := setupExchangeScenario(t, s, clk)
	ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID}, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	_, summary, _ := s.FetchExchangePackage(rcv, ex.ID)

	// 非法结果。
	if _, err := s.SubmitReceipt(rcv, ex.ID, summary, "maybe", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad result err = %v, want ErrInvalidArgument", err)
	}
	// 摘要不符 → ErrConflict。
	if _, err := s.SubmitReceipt(rcv, ex.ID, "deadbeef", ExchangeAccepted, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("bad summary err = %v, want ErrConflict", err)
	}
	// 其他身份不能代交。
	if _, err := s.SubmitReceipt(rcvB, ex.ID, summary, ExchangeAccepted, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other receiver err = %v, want ErrAccessDenied", err)
	}
	if _, err := s.SubmitReceipt(doc, ex.ID, summary, ExchangeAccepted, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("internal actor receipt err = %v, want ErrAccessDenied", err)
	}
	if _, err := s.SubmitReceipt(rcv, "exc_missing", summary, ExchangeAccepted, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("missing exchange receipt err = %v, want ErrAccessDenied", err)
	}

	// 接受成功。
	clk.t = clk.t.Add(time.Hour)
	conf, err := s.SubmitReceipt(rcv, ex.ID, summary, ExchangeAccepted, "")
	if err != nil {
		t.Fatal(err)
	}
	if conf.Status != ExchangeAccepted {
		t.Fatalf("status = %q", conf.Status)
	}
	nAudits := len(mustAudit(t, s, pid))

	// 相同回执重交：原结果、无新审计。
	conf2, err := s.SubmitReceipt(rcv, ex.ID, summary, ExchangeAccepted, "")
	if err != nil {
		t.Fatalf("idempotent resubmit: %v", err)
	}
	if conf2.Status != conf.Status || conf2.Result != conf.Result || !conf2.At.Equal(conf.At) {
		t.Fatalf("resubmit changed result: %+v vs %+v", conf2, conf)
	}
	if len(mustAudit(t, s, pid)) != nAudits {
		t.Fatal("idempotent resubmit added audit")
	}

	// 结果或原因改变 → ErrConflict。
	if _, err := s.SubmitReceipt(rcv, ex.ID, summary, ExchangeRejected, "反悔"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed result err = %v, want ErrConflict", err)
	}
	if _, err := s.SubmitReceipt(rcv, ex.ID, summary, ExchangeAccepted, "补充原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed reason err = %v, want ErrConflict", err)
	}
	// 冲突后状态不变。
	got := findExchange(s, ex.ID)
	if got.Status != ExchangeAccepted || got.ReceiptReason != "" {
		t.Fatalf("state changed after conflict: %+v", got)
	}
}

func TestReceiptAllowedAfterBindingExpiredButReturnsNoContent(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, diagID, ordID, authID := setupExchangeScenario(t, s, clk)
	ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID}, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	_, summary, _ := s.FetchExchangePackage(rcv, ex.ID)

	// 再建一份未回执的交换，用于验证停用后的首次登记。
	ex2, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{ordID}, "req-2")
	if err != nil {
		t.Fatal(err)
	}
	_, summary2, _ := s.FetchExchangePackage(rcv, ex2.ID)

	// 授权到期后：取包被拒，但回执仍可登记。
	clk.t = clk.t.Add(48 * time.Hour)
	if _, _, err := s.FetchExchangePackage(rcv, ex.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("fetch after expiry err = %v", err)
	}
	conf, err := s.SubmitReceipt(rcv, ex.ID, summary, ExchangeAccepted, "")
	if err != nil {
		t.Fatalf("receipt after expiry must be allowed: %v", err)
	}
	if conf.Status != ExchangeAccepted || conf.ExchangeID != ex.ID || conf.Result != ExchangeAccepted ||
		conf.ActorID != rcv.ID || conf.At.IsZero() {
		t.Fatalf("confirmation after expiry wrong: %+v", conf)
	}

	// 患者停用后仍允许登记首次回执。
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatal(err)
	}
	conf2, err := s.SubmitReceipt(rcv, ex2.ID, summary2, ExchangeRejected, "停用后拒收")
	if err != nil {
		t.Fatalf("first receipt after deactivation must be allowed: %v", err)
	}
	if conf2.Status != ExchangeRejected || conf2.Result != ExchangeRejected ||
		conf2.Reason != "停用后拒收" || conf2.ActorID != rcv.ID {
		t.Fatalf("confirmation after deactivation wrong: %+v", conf2)
	}
	// 停用后取包仍被拒绝。
	if _, _, err := s.FetchExchangePackage(rcv, ex2.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("fetch after deactivation err = %v", err)
	}
}

// ---- 内部使用者查看 ----

func TestListExchangesRequiresInternalAndPatient(t *testing.T) {
	s, clk := newTestStore(t)
	pid, _, _, _, _ := setupExchangeScenario(t, s, clk)

	if _, err := s.ListExchanges(rcv, pid); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver list err = %v, want ErrAccessDenied", err)
	}
	if _, err := s.ListExchanges(doc, "pat_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing patient list err = %v, want ErrNotFound", err)
	}
	if list, err := s.ListExchanges(doc, pid); err != nil || len(list) != 0 {
		t.Fatalf("empty list = %+v %v", list, err)
	}
}

// ---- 持久化：重开后原包、回执与审计保留 ----

func TestExchangePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 4, 1, 8, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	pid, _, diagID, _, authID := setupExchangeScenario(t, s, clk)
	ex, err := s.CreateExchange(doc, pid, rcv.ID, authID, []ID{diagID}, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	summary := ex.Summary
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open()
	list, err := s2.ListExchanges(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("exchanges lost: %d", len(list))
	}
	got := list[0]
	if got.ID != ex.ID || got.Summary != summary || got.Status != ExchangePending {
		t.Fatalf("exchange not preserved: %+v", got)
	}
	if len(got.Package.Records) != 1 || got.Package.Records[0].Content != "诊断v2" ||
		got.Package.Records[0].Version != 2 {
		t.Fatalf("package not preserved: %+v", got.Package)
	}
	// 审计保留。
	var found bool
	for _, ev := range mustAudit(t, s2, pid) {
		if ev.Action == ActionExchangeCreated && ev.ObjectID == ex.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("creation audit lost after reopen")
	}

	// 重开后取包与回执正常。
	pkg, fetchedSummary, err := s2.FetchExchangePackage(rcv, ex.ID)
	if err != nil {
		t.Fatalf("fetch after reopen: %v", err)
	}
	if fetchedSummary != summary || len(pkg.Records) != 1 {
		t.Fatalf("fetched after reopen wrong")
	}
	clk.t = clk.t.Add(time.Hour)
	conf, err := s2.SubmitReceipt(rcv, ex.ID, summary, ExchangeAccepted, "")
	if err != nil {
		t.Fatalf("receipt after reopen: %v", err)
	}
	if conf.Status != ExchangeAccepted {
		t.Fatalf("status = %q", conf.Status)
	}

	// 再次重开：回执保留。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })
	list3, _ := s3.ListExchanges(doc, pid)
	if len(list3) != 1 || list3[0].Status != ExchangeAccepted || list3[0].ReceiptAt == nil {
		t.Fatalf("receipt not preserved: %+v", list3)
	}
	nAudits := len(mustAudit(t, s3, pid))
	// 幂等重交不新增审计。
	if _, err := s3.SubmitReceipt(rcv, ex.ID, summary, ExchangeAccepted, ""); err != nil {
		t.Fatalf("idempotent receipt after reopen: %v", err)
	}
	if len(mustAudit(t, s3, pid)) != nAudits {
		t.Fatal("idempotent receipt after reopen added audit")
	}
}
