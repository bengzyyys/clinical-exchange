package clinical

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// ---- 交换场景夹具 ----

type exchangeFixture struct {
	s      *Store
	clk    *fakeClock
	pid    ID
	e1, e2 ID
	diag   ID // 已生效并更正过一次（当前 v2）
	diagV1 Version
	diagV2 Version
	ord    ID // 已生效 v1
	draft  ID // 仅草稿
	auth   Authorization
	start  time.Time
	end    time.Time
}

func setupExchange(t *testing.T) exchangeFixture {
	t.Helper()
	s, clk := newTestStore(t)
	pid, e1 := setupPatientEncounter(t, s)
	e2enc, err := s.AddEncounter(doc, pid, clk.t.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e2 := e2enc.ID

	d, _ := s.CreateDraft(doc, pid, e1, Diagnosis, "诊断v1内容")
	v1, err := s.ActivateRecord(doc, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.CorrectRecord(doc, d.ID, 1, "诊断v2内容", "录入笔误")
	if err != nil {
		t.Fatal(err)
	}

	o, _ := s.CreateDraft(doc, pid, e1, Order, "医嘱v1内容")
	if _, err := s.ActivateRecord(doc, o.ID); err != nil {
		t.Fatal(err)
	}

	dr, _ := s.CreateDraft(doc, pid, e1, Diagnosis, "未生效草稿")

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)
	a, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{
			{EncounterID: e1, Category: Diagnosis},
			{EncounterID: e1, Category: Order},
		}, start, end)
	if err != nil {
		t.Fatal(err)
	}

	return exchangeFixture{
		s: s, clk: clk, pid: pid, e1: e1, e2: e2,
		diag: d.ID, diagV1: v1, diagV2: v2, ord: o.ID, draft: dr.ID,
		auth: a, start: start, end: end,
	}
}

// ---- 创建交换：成功打包、摘要、包内容 ----

func TestCreateExchangePackagesCurrentEffectiveVersions(t *testing.T) {
	f := setupExchange(t)

	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag, f.ord, f.diag}, "req-1")
	if err != nil {
		t.Fatalf("create exchange: %v", err)
	}
	if x.ID == "" || x.Status != ExchangePending || x.Receipt != nil {
		t.Fatalf("bad exchange: %+v", x)
	}
	if x.Digest == "" {
		t.Fatal("digest empty")
	}
	if !x.CreatedAt.Equal(f.clk.t) {
		t.Fatalf("created at %v != %v", x.CreatedAt, f.clk.t)
	}
	if x.Package.PatientID != f.pid || x.Package.ReceiverID != rcv.ID {
		t.Fatalf("package header wrong: %+v", x.Package)
	}
	// 重复记录标识合并，两条记录按标识排序。
	if len(x.Package.Records) != 2 {
		t.Fatalf("records = %d, want 2 (duplicates merged)", len(x.Package.Records))
	}
	byID := map[ID]PackagedRecord{}
	for _, pr := range x.Package.Records {
		byID[pr.RecordID] = pr
	}
	gotDiag := byID[f.diag]
	if gotDiag.VersionID != f.diagV2.ID || gotDiag.Version != 2 || gotDiag.Content != "诊断v2内容" {
		t.Fatalf("diagnosis not current effective version: %+v", gotDiag)
	}
	if gotDiag.EncounterID != f.e1 || gotDiag.Category != Diagnosis {
		t.Fatalf("diagnosis header wrong: %+v", gotDiag)
	}
	if !gotDiag.EffectiveAt.Equal(f.diagV2.CreatedAt) {
		t.Fatalf("effective time %v != %v", gotDiag.EffectiveAt, f.diagV2.CreatedAt)
	}
	gotOrd := byID[f.ord]
	if gotOrd.Version != 1 || gotOrd.Content != "医嘱v1内容" || gotOrd.Category != Order {
		t.Fatalf("order wrong: %+v", gotOrd)
	}

	// 审计：新建交换事件。
	evs := mustAudit(t, f.s, f.pid)
	last := evs[len(evs)-1]
	if last.Action != ActionExchanged || last.ObjectType != "exchange" ||
		last.ObjectID != x.ID || last.ActorID != doc.ID || last.PatientID != f.pid {
		t.Fatalf("exchange audit wrong: %+v", last)
	}
}

// ---- 创建交换：参数校验失败不留交换、不留审计 ----

func TestCreateExchangeValidationLeavesNothing(t *testing.T) {
	f := setupExchange(t)
	other, _ := f.s.RegisterPatient(doc, "合成患者乙")
	encOther, _ := f.s.AddEncounter(doc, other.ID, time.Time{})
	recOther, _ := f.s.CreateDraft(doc, other.ID, encOther.ID, Diagnosis, "他人记录")
	vOther, _ := f.s.ActivateRecord(doc, recOther.ID)
	_ = vOther

	// 授权给 rcvB，与接收方 rcv 不符。
	authB, err := f.s.Grant(doc, f.pid, rcvB.ID,
		[]Scope{{EncounterID: f.e1, Category: Diagnosis}}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	// 授权范围只覆盖 e2 的诊断，不覆盖 e1。
	authE2, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e2, Category: Diagnosis}}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	// e2 下一条已生效诊断：被 authE2 覆盖，可用于构造“一条覆盖、一条未覆盖”的混合集合。
	recE2, _ := f.s.CreateDraft(doc, f.pid, f.e2, Diagnosis, "二诊诊断内容")
	if _, err := f.s.ActivateRecord(doc, recE2.ID); err != nil {
		t.Fatal(err)
	}
	// 尚未开始的授权。
	authFuture, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Diagnosis}}, f.clk.t.Add(time.Hour), f.clk.t.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 已撤回的授权。
	authRevoked, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Order}, {EncounterID: f.e1, Category: Diagnosis}}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Revoke(doc, f.pid, authRevoked.ID); err != nil {
		t.Fatal(err)
	}

	auditBefore := len(mustAudit(t, f.s, f.pid))

	mustFail := func(name string, want error, actor Actor, pid ID, receiver string, authID ID, recs []ID, req string) {
		t.Helper()
		if _, err := f.s.CreateExchange(actor, pid, receiver, authID, recs, req); !errors.Is(err, want) {
			t.Fatalf("%s: err = %v, want %v", name, err, want)
		}
	}

	mustFail("receiver actor", ErrAccessDenied, rcv, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "r")
	mustFail("empty record set", ErrInvalidArgument, doc, f.pid, rcv.ID, f.auth.ID, nil, "r")
	mustFail("blank record id", ErrInvalidArgument, doc, f.pid, rcv.ID, f.auth.ID, []ID{"  "}, "r")
	mustFail("blank request id", ErrInvalidArgument, doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "  ")
	mustFail("blank receiver", ErrInvalidArgument, doc, f.pid, " ", f.auth.ID, []ID{f.diag}, "r")
	mustFail("blank auth", ErrInvalidArgument, doc, f.pid, rcv.ID, " ", []ID{f.diag}, "r")
	mustFail("missing patient", ErrNotFound, doc, "pat_missing", rcv.ID, f.auth.ID, []ID{f.diag}, "r")
	mustFail("missing auth", ErrNotFound, doc, f.pid, rcv.ID, "auth_missing", []ID{f.diag}, "r")
	mustFail("auth of other patient", ErrMismatchedPatient, doc, other.ID, rcv.ID, f.auth.ID, []ID{f.diag}, "r")
	mustFail("auth bound to other receiver", ErrInvalidArgument, doc, f.pid, rcv.ID, authB.ID, []ID{f.diag}, "r")
	mustFail("missing record", ErrNotFound, doc, f.pid, rcv.ID, f.auth.ID, []ID{"rec_missing"}, "r")
	mustFail("cross-patient record", ErrMismatchedPatient, doc, f.pid, rcv.ID, f.auth.ID, []ID{recOther.ID}, "r")
	mustFail("draft record", ErrInvalidArgument, doc, f.pid, rcv.ID, f.auth.ID, []ID{f.draft}, "r")
	mustFail("scope not covered", ErrAccessDenied, doc, f.pid, rcv.ID, authE2.ID, []ID{f.diag}, "r")
	mustFail("future auth", ErrAccessDenied, doc, f.pid, rcv.ID, authFuture.ID, []ID{f.diag}, "r")
	mustFail("revoked auth", ErrAccessDenied, doc, f.pid, rcv.ID, authRevoked.ID, []ID{f.diag}, "r")
	// 混合集合：recE2 被 authE2 覆盖、diag 不被覆盖，整包拒绝。
	mustFail("one uncovered record", ErrAccessDenied, doc, f.pid, rcv.ID, authE2.ID, []ID{f.diag, recE2.ID}, "r")

	// 已到期授权：推进时钟。
	f.clk.t = f.end.Add(time.Minute)
	mustFail("expired auth", ErrAccessDenied, doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "r")
	f.clk.t = f.start.Add(-2 * time.Hour)
	mustFail("not started auth", ErrAccessDenied, doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "r")
	f.clk.t = f.start.Add(time.Hour)

	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("failed creates changed audit count: before=%d after=%d", auditBefore, got)
	}
	if xs, _ := f.s.ListExchanges(doc, f.pid); len(xs) != 0 {
		t.Fatalf("failed creates persisted exchanges: %d", len(xs))
	}

	// 患者停用后不能新建交换。
	if err := f.s.DeactivatePatient(doc, f.pid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "r"); !errors.Is(err, ErrDeactivated) {
		t.Fatalf("deactivated create err = %v, want ErrDeactivated", err)
	}
}

// ---- 幂等重试：相同参数返回原交换，不重新取内容、不新增审计 ----

func TestCreateExchangeIdempotentRetryAndFreeze(t *testing.T) {
	f := setupExchange(t)

	x1, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.ord, f.diag}, "req-42")
	if err != nil {
		t.Fatal(err)
	}

	// 打包后更正诊断到 v3；包应固定在 v2，Read 仍返回当前版本。
	v3, err := f.s.CorrectRecord(doc, f.diag, 2, "诊断v3内容", "复核更新")
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Content != "诊断v3内容" || res.Records[0].VersionID != v3.ID {
		t.Fatalf("Read must still return current version: %+v", res.Records)
	}

	auditBefore := len(mustAudit(t, f.s, f.pid))

	// 顺序不同、重复标识、接收方以相同请求重试（仍由内部使用者发起）。
	x2, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
		[]ID{f.diag, f.ord, f.diag}, "req-42")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if x2.ID != x1.ID || x2.Digest != x1.Digest || !x2.CreatedAt.Equal(x1.CreatedAt) {
		t.Fatalf("retry did not return original exchange:\n%+v\n%+v", x1, x2)
	}
	if len(x2.Package.Records) != 2 {
		t.Fatalf("retry package records = %d, want 2", len(x2.Package.Records))
	}
	for _, pr := range x2.Package.Records {
		if pr.RecordID == f.diag {
			if pr.VersionID != f.diagV2.ID || pr.Content != "诊断v2内容" {
				t.Fatalf("package refroze to new version: %+v", pr)
			}
		}
	}
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("idempotent retry added audit events: %d -> %d", auditBefore, got)
	}
	if xs, _ := f.s.ListExchanges(doc, f.pid); len(xs) != 1 {
		t.Fatalf("exchange count = %d, want 1", len(xs))
	}

	// 其他参数变化 → ErrConflict。
	otherRec, _ := f.s.CreateDraft(doc, f.pid, f.e2, Diagnosis, "二诊诊断")
	if _, err := f.s.ActivateRecord(doc, otherRec.ID); err != nil {
		t.Fatal(err)
	}
	authE2, _ := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e2, Category: Diagnosis}}, f.start, f.end)

	conflictCases := []struct {
		name     string
		pid      ID
		receiver string
		authID   ID
		recs     []ID
	}{
		{"record set differs", f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord, otherRec.ID}},
		{"record set subset", f.pid, rcv.ID, f.auth.ID, []ID{f.diag}},
		{"receiver differs", f.pid, rcvB.ID, f.auth.ID, []ID{f.diag, f.ord}},
		{"authorization differs", f.pid, rcv.ID, authE2.ID, []ID{f.diag, f.ord}},
		{"patient differs", otherPID(t, f.s), rcv.ID, f.auth.ID, []ID{f.diag, f.ord}},
	}
	for _, c := range conflictCases {
		if _, err := f.s.CreateExchange(doc, c.pid, c.receiver, c.authID, c.recs, "req-42"); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: err = %v, want ErrConflict", c.name, err)
		}
	}

	// 另一内部使用者用相同请求号：互不干扰，各自一份。
	xOther, err := f.s.CreateExchange(doc2, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "req-42")
	if err != nil {
		t.Fatalf("other actor same request id: %v", err)
	}
	if xOther.ID == x1.ID {
		t.Fatal("request id must be scoped per internal actor")
	}
}

func otherPID(t *testing.T, s *Store) ID {
	t.Helper()
	p, err := s.RegisterPatient(doc, "合成患者乙")
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// 首次失败不占用请求号。
func TestFailedFirstAttemptDoesNotConsumeRequestID(t *testing.T) {
	f := setupExchange(t)

	// 用夹带草稿的非法集合先失败。
	if _, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.draft}, "req-9"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("first attempt err = %v", err)
	}
	// 同请求号合法请求应成功，而不是被判为冲突。
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "req-9")
	if err != nil {
		t.Fatalf("second attempt after failure: %v", err)
	}
	if x.ID == "" {
		t.Fatal("exchange not created after failed first attempt")
	}
}

// ---- 接收方取包：重新检查绑定授权与患者状态 ----

func TestFetchPackageRechecksBinding(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-fetch")
	if err != nil {
		t.Fatal(err)
	}

	del, err := f.s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if del.ExchangeID != x.ID || del.Digest != x.Digest || del.Status != ExchangePending {
		t.Fatalf("bad delivery: %+v", del)
	}
	if len(del.Package.Records) != 2 || del.Package.PatientID != f.pid || del.Package.ReceiverID != rcv.ID {
		t.Fatalf("bad package: %+v", del.Package)
	}
	if !del.CreatedAt.Equal(x.CreatedAt) {
		t.Fatal("delivery created-at mismatch")
	}

	// 其他接收方、内部使用者、不存在的交换统一拒绝。
	if _, err := f.s.FetchPackage(rcvB, x.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other receiver err = %v", err)
	}
	if _, err := f.s.FetchPackage(doc, x.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("internal actor err = %v", err)
	}
	if _, err := f.s.FetchPackage(rcv, "exch_missing"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("missing exchange err = %v", err)
	}

	// 绑定授权到期：即便存在另一条覆盖同范围的有效授权也不能替代。
	f.clk.t = f.end.Add(time.Minute)
	otherAuth, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Diagnosis}, {EncounterID: f.e1, Category: Order}},
		f.clk.t.Add(-time.Hour), f.clk.t.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_ = otherAuth
	if del, err := f.s.FetchPackage(rcv, x.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expired binding err = %v, delivery = %+v", err, del)
	}

	// 回到窗口内：撤回绑定授权，另一条有效仍不能替代。
	f.clk.t = f.start.Add(2 * time.Hour)
	if err := f.s.Revoke(doc, f.pid, f.auth.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.FetchPackage(rcv, x.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked binding err = %v", err)
	}

	// 患者停用：拒绝。
	if err := f.s.DeactivatePatient(doc, f.pid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.FetchPackage(rcv, x.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("deactivated fetch err = %v", err)
	}
}

// ---- 回执登记 ----

func TestSubmitReceiptAcceptAndReject(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "req-rcpt")
	if err != nil {
		t.Fatal(err)
	}

	// 非法结果、空白拒绝原因。
	if _, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, "maybe", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad outcome err = %v", err)
	}
	if _, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "  "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank reject reason err = %v", err)
	}
	auditBefore := len(mustAudit(t, f.s, f.pid))

	conf, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if conf.Status != ExchangeAccepted || conf.Outcome != ReceiptAccepted || conf.ExchangeID != x.ID || conf.RegisteredAt.IsZero() {
		t.Fatalf("bad confirmation: %+v", conf)
	}
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore+1 {
		t.Fatalf("receipt audit count = %d, want %d", got, auditBefore+1)
	}
	ev := mustAudit(t, f.s, f.pid)[auditBefore]
	if ev.Action != ActionReceipted || ev.ObjectID != x.ID || ev.ActorID != rcv.ID {
		t.Fatalf("receipt audit wrong: %+v", ev)
	}

	// 相同回执重交：原确认、无新审计。
	conf2, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, "ignored reason")
	if err != nil {
		t.Fatalf("duplicate accept: %v", err)
	}
	if conf2.Status != conf.Status || !conf2.RegisteredAt.Equal(conf.RegisteredAt) {
		t.Fatalf("duplicate receipt changed confirmation: %+v vs %+v", conf, conf2)
	}
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore+1 {
		t.Fatalf("duplicate receipt added audit: %d", got)
	}

	// 结果改变：冲突。
	if _, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "改主意"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed outcome err = %v", err)
	}

	// 内部视图反映已接受状态与回执。
	got, err := f.s.GetExchange(doc, f.pid, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ExchangeAccepted || got.Receipt == nil || got.Receipt.Outcome != ReceiptAccepted {
		t.Fatalf("internal view not updated: %+v", got)
	}
}

func TestSubmitReceiptRejectFlowAndDigestGuard(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-rej")
	if err != nil {
		t.Fatal(err)
	}

	// 摘要不符：冲突，状态不变。
	if _, err := f.s.SubmitReceipt(rcv, x.ID, "deadbeef", ReceiptRejected, "内容对不上"); !errors.Is(err, ErrConflict) {
		t.Fatalf("bad digest err = %v", err)
	}
	// 其他接收方、内部使用者、不存在的交换不能代交。
	if _, err := f.s.SubmitReceipt(rcvB, x.ID, x.Digest, ReceiptAccepted, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other receiver err = %v", err)
	}
	if _, err := f.s.SubmitReceipt(doc, x.ID, x.Digest, ReceiptAccepted, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("internal actor err = %v", err)
	}
	if _, err := f.s.SubmitReceipt(rcv, "exch_missing", x.Digest, ReceiptAccepted, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("missing exchange err = %v", err)
	}

	conf, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "记录不完整")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if conf.Status != ExchangeRejected || conf.Outcome != ReceiptRejected {
		t.Fatalf("bad confirmation: %+v", conf)
	}
	got, _ := f.s.GetExchange(doc, f.pid, x.ID)
	if got.Status != ExchangeRejected || got.Receipt.Reason != "记录不完整" {
		t.Fatalf("reject not stored: %+v", got.Receipt)
	}

	// 相同拒绝（含原因）重交返回原结果；原因变化冲突。
	if _, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "记录不完整"); err != nil {
		t.Fatalf("duplicate reject: %v", err)
	}
	if _, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "另一个原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed reason err = %v", err)
	}
}

// 授权失效或患者停用后仍允许登记此前包的回执，但不提供受保护内容。
func TestReceiptAllowedAfterAccessLost(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "req-late")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Revoke(doc, f.pid, f.auth.ID); err != nil {
		t.Fatal(err)
	}
	// 取包已被拒绝。
	if _, err := f.s.FetchPackage(rcv, x.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("fetch after revoke err = %v", err)
	}
	// 回执仍可登记，确认中不含包内容。
	conf, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("receipt after revoke: %v", err)
	}
	if conf.Status != ExchangeAccepted {
		t.Fatalf("status = %q", conf.Status)
	}

	// 停用后再对第二份包登记回执。
	x2, err := f.s.CreateExchange(doc, f.pid, rcv.ID, mustGrant(t, f), []ID{f.ord}, "req-late-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeactivatePatient(doc, f.pid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.FetchPackage(rcv, x2.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("fetch after deactivate err = %v", err)
	}
	conf2, err := f.s.SubmitReceipt(rcv, x2.ID, x2.Digest, ReceiptRejected, "停用前已收到")
	if err != nil {
		t.Fatalf("receipt after deactivate: %v", err)
	}
	if conf2.Status != ExchangeRejected {
		t.Fatalf("status = %q", conf2.Status)
	}
}

func mustGrant(t *testing.T, f exchangeFixture) ID {
	t.Helper()
	a, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Order}}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// ---- 内部使用者查询 ----

func TestInternalExchangeQueries(t *testing.T) {
	f := setupExchange(t)
	xs1, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "req-q1")
	if err != nil {
		t.Fatal(err)
	}
	f.clk.t = f.clk.t.Add(time.Minute)
	authB, err := f.s.Grant(doc, f.pid, rcvB.ID,
		[]Scope{{EncounterID: f.e1, Category: Order}}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	xs2, err := f.s.CreateExchange(doc, f.pid, rcvB.ID, authB.ID, []ID{f.ord}, "req-q2")
	if err != nil {
		t.Fatalf("create x2: %v", err)
	}

	got, err := f.s.GetExchange(doc, f.pid, xs1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != xs1.ID || len(got.Package.Records) != 1 {
		t.Fatalf("get exchange bad: %+v", got)
	}
	if _, err := f.s.GetExchange(doc, f.pid, "exch_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	other := otherPID(t, f.s)
	if _, err := f.s.GetExchange(doc, other, xs1.ID); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient err = %v", err)
	}
	if _, err := f.s.GetExchange(rcv, f.pid, xs1.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver err = %v", err)
	}

	xs, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 2 || xs[0].ID != xs1.ID || xs[1].ID != xs2.ID {
		t.Fatalf("list order wrong: %+v", xs)
	}
	if xs2, _ := f.s.ListExchanges(doc, other); len(xs2) != 0 {
		t.Fatalf("exchanges leaked across patients: %d", len(xs2))
	}
	if _, err := f.s.ListExchanges(rcv, f.pid); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver list err = %v", err)
	}

	// 停用后仍可查原包与回执。
	if err := f.s.DeactivatePatient(doc, f.pid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetExchange(doc, f.pid, xs1.ID); err != nil {
		t.Fatalf("get after deactivate: %v", err)
	}
	if xs, err := f.s.ListExchanges(doc, f.pid); err != nil || len(xs) != 2 {
		t.Fatalf("list after deactivate: %d %v", len(xs), err)
	}
}

// ---- 摘要稳定性：与入参顺序无关 ----

func TestPackageDigestOrderIndependent(t *testing.T) {
	f := setupExchange(t)
	x1, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.ord, f.diag}, "req-d1")
	if err != nil {
		t.Fatal(err)
	}
	// 另一名使用者、另一请求号、相反顺序：包记录相同则摘要相同。
	x2, err := f.s.CreateExchange(doc2, f.pid, rcv.ID, f.auth.ID, []ID{f.diag, f.ord}, "req-d2")
	if err != nil {
		t.Fatal(err)
	}
	if x1.Digest != x2.Digest {
		t.Fatalf("digest depends on input order:\n%s\n%s", x1.Digest, x2.Digest)
	}
	// 取包得到的摘要与创建时一致，供接收方核对。
	del, err := f.s.FetchPackage(rcv, x1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if del.Digest != x1.Digest {
		t.Fatal("delivery digest mismatch")
	}
}

// ---- 并发重试只产生一份交换 ----

func TestConcurrentCreateExchangeProducesOne(t *testing.T) {
	f := setupExchange(t)
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	ids := make([]ID, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID,
				[]ID{f.diag, f.ord}, "req-concurrent")
			errs[i] = err
			ids[i] = x.ID
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("goroutine %d got %q, want %q", i, ids[i], ids[0])
		}
	}
	xs, _ := f.s.ListExchanges(doc, f.pid)
	if len(xs) != 1 {
		t.Fatalf("exchanges = %d, want 1", len(xs))
	}
	var createEvents int
	for _, ev := range mustAudit(t, f.s, f.pid) {
		if ev.Action == ActionExchanged {
			createEvents++
		}
	}
	if createEvents != 1 {
		t.Fatalf("exchange_created events = %d, want 1", createEvents)
	}
}

// ---- 重开恢复：原包、重试结果、回执、审计全部保留 ----

func TestExchangePersistenceAcrossReopen(t *testing.T) {
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
	p, _ := s.RegisterPatient(doc, "持久化交换患者")
	e, _ := s.AddEncounter(doc, p.ID, time.Time{})
	rec, _ := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "包内v1")
	v1, _ := s.ActivateRecord(doc, rec.ID)
	a, _ := s.Grant(doc, p.ID, rcv.ID, []Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour))

	x, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rec.ID}, "req-persist")
	if err != nil {
		t.Fatal(err)
	}
	// 打包后更正：包固定在 v1。
	if _, err := s.CorrectRecord(doc, rec.ID, 1, "当前v2", "事后更正"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "重开前拒绝"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	// 原包内容固定为 v1，回执保留。
	got, err := s2.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.Status != ExchangeRejected || got.Receipt == nil || got.Receipt.Reason != "重开前拒绝" {
		t.Fatalf("receipt lost: %+v", got)
	}
	if len(got.Package.Records) != 1 || got.Package.Records[0].VersionID != v1.ID ||
		got.Package.Records[0].Content != "包内v1" {
		t.Fatalf("frozen package lost: %+v", got.Package.Records)
	}

	// 同请求号重试仍返回原交换，不新增审计。
	auditBefore := len(mustAudit(t, s2, p.ID))
	retry, err := s2.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rec.ID}, "req-persist")
	if err != nil {
		t.Fatalf("retry after reopen: %v", err)
	}
	if retry.ID != x.ID || retry.Status != ExchangeRejected {
		t.Fatalf("retry after reopen = %+v", retry)
	}
	if retry.Package.Records[0].Content != "包内v1" {
		t.Fatalf("retry refetched current content: %+v", retry.Package.Records)
	}
	if n := len(mustAudit(t, s2, p.ID)); n != auditBefore {
		t.Fatalf("retry after reopen added audit: %d", n)
	}

	// 审计两类事件都在。
	var creates, receipts int
	for _, ev := range mustAudit(t, s2, p.ID) {
		switch ev.Action {
		case ActionExchanged:
			creates++
		case ActionReceipted:
			receipts++
		}
	}
	if creates != 1 || receipts != 1 {
		t.Fatalf("audit after reopen: creates=%d receipts=%d", creates, receipts)
	}

	// 取包路径在重开后同样按当前时间与绑定授权判断（窗口内可取）。
	del, err := s2.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatalf("fetch after reopen: %v", err)
	}
	if del.Digest != x.Digest || del.Status != ExchangeRejected {
		t.Fatalf("delivery after reopen wrong: %+v", del)
	}
}

// 关闭后调用返回 ErrClosed。
func TestExchangeOperationsAfterClose(t *testing.T) {
	f := setupExchange(t)
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "req-closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreateExchange(doc, f.pid, rcv.ID, f.auth.ID, []ID{f.diag}, "req-closed-2"); !errors.Is(err, ErrClosed) {
		t.Fatalf("create after close err = %v", err)
	}
	if _, err := f.s.FetchPackage(rcv, x.ID); !errors.Is(err, ErrClosed) {
		t.Fatalf("fetch after close err = %v", err)
	}
	if _, err := f.s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptAccepted, ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("receipt after close err = %v", err)
	}
	if _, err := f.s.GetExchange(doc, f.pid, x.ID); !errors.Is(err, ErrClosed) {
		t.Fatalf("get after close err = %v", err)
	}
}
