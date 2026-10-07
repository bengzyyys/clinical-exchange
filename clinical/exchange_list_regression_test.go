package clinical

import (
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"
)

// 本文件为 ListExchanges（内部使用者按患者查看交换清单）补回归保障。
// 核心约定：清单只含所查患者的交换——即使其他患者的交换由同一内部使用者
// 创建、发给同一接收方，也不能混入或造成遗漏；同一患者发给不同接收方的
// 交换一并列出。清单是内部历史视图：待回执、已接受、已拒绝的交换都保留，
// 每项对应实际保存的那份交换（绑定授权、请求号、接收方、摘要、固化包与
// 回执原值），按创建时间旧到新排序，时间戳相同按交换标识升序。

// ---- ListExchanges 回归测试夹具 ----
//
// 同一份本地存储中并存：
//   - 患者甲、患者乙：各有已生效诊断记录，都授权给共用接收方 rcv；
//     患者甲另有一条授权给接收方 rcvB；
//   - 患者丙：只有档案，从未有过任何交换；
//   - 患者甲另有一条仅草稿的记录：其内容不得进入任何交换包。
type listExchangeFixture struct {
	s   *Store
	clk *fakeClock

	pidA ID
	pidB ID
	pidC ID // 已登记但没有任何交换

	recA   ID      // 患者甲已生效诊断（打包后会被更正）
	recAV1 Version // recA 的第 1 版：打包时固化进包的版本
	recB   ID      // 患者乙已生效诊断
	draftA ID      // 患者甲未生效草稿

	authA  Authorization // 患者甲 → rcv（与患者乙共用接收方）
	authAB Authorization // 患者甲 → rcvB（同一患者的另一接收方）
	authB  Authorization // 患者乙 → rcv
}

// 包内容禁区标记：患者姓名、草稿内容、更正原因、更正后的当前临床内容
// 都不得出现在清单项的交换包里。
const (
	listPatientAName   = "列表回归患者甲"
	listPatientBName   = "列表回归患者乙"
	listPackagedV1     = "列表回归：打包时固化的v1内容"
	listDraftContent   = "列表回归：草稿内容不得入包"
	listCorrectReason  = "列表回归：更正原因不得入包"
	listCurrentContent = "列表回归：更正后的当前临床内容"
)

func setupListExchangeFixture(t *testing.T) listExchangeFixture {
	t.Helper()
	s, clk := newTestStore(t)

	mkPatient := func(name string) (ID, ID) {
		t.Helper()
		p, err := s.RegisterPatient(doc, name)
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		e, err := s.AddEncounter(doc, p.ID, time.Time{})
		if err != nil {
			t.Fatalf("add encounter: %v", err)
		}
		return p.ID, e.ID
	}

	pidA, eA := mkPatient(listPatientAName)
	pidB, eB := mkPatient(listPatientBName)
	// 患者丙：只登记档案，没有任何就诊、记录与交换。
	pC, err := s.RegisterPatient(doc, "列表回归患者丙")
	if err != nil {
		t.Fatalf("register patient C: %v", err)
	}

	dA, err := s.CreateDraft(doc, pidA, eA, Diagnosis, listPackagedV1)
	if err != nil {
		t.Fatalf("create draft A: %v", err)
	}
	v1, err := s.ActivateRecord(doc, dA.ID)
	if err != nil {
		t.Fatalf("activate A: %v", err)
	}
	dB, err := s.CreateDraft(doc, pidB, eB, Diagnosis, "患者乙打包内容v1")
	if err != nil {
		t.Fatalf("create draft B: %v", err)
	}
	if _, err := s.ActivateRecord(doc, dB.ID); err != nil {
		t.Fatalf("activate B: %v", err)
	}
	drA, err := s.CreateDraft(doc, pidA, eA, Order, listDraftContent)
	if err != nil {
		t.Fatalf("create draft-only record: %v", err)
	}

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(72 * time.Hour)
	grant := func(pid ID, receiver string, enc ID) Authorization {
		t.Helper()
		a, err := s.Grant(doc, pid, receiver,
			[]Scope{{EncounterID: enc, Category: Diagnosis}}, start, end)
		if err != nil {
			t.Fatalf("grant %s → %s: %v", pid, receiver, err)
		}
		return a
	}

	return listExchangeFixture{
		s: s, clk: clk,
		pidA: pidA, pidB: pidB, pidC: pC.ID,
		recA: dA.ID, recAV1: v1, recB: dB.ID, draftA: drA.ID,
		authA:  grant(pidA, rcv.ID, eA),
		authAB: grant(pidA, rcvB.ID, eA),
		authB:  grant(pidB, rcv.ID, eB),
	}
}

// create 以内部使用者 doc 身份创建一份单记录交换；时钟推进由测试自己控制。
func (f listExchangeFixture) create(t *testing.T, pid ID, receiver string, authID, recID ID, req string) Exchange {
	t.Helper()
	x, err := f.s.CreateExchange(doc, pid, receiver, authID, []ID{recID}, req)
	if err != nil {
		t.Fatalf("create exchange %q: %v", req, err)
	}
	return x
}

func mustListExchanges(t *testing.T, s *Store, pid ID) []Exchange {
	t.Helper()
	lst, err := s.ListExchanges(doc, pid)
	if err != nil {
		t.Fatalf("list exchanges for %q: %v", pid, err)
	}
	return lst
}

// assertExchangeListExactly 断言清单与预期逐项一致（顺序、内容、回执），
// 且每份交换恰好出现一次。
func assertExchangeListExactly(t *testing.T, got, want []Exchange, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: list len = %d, want %d: %+v", label, len(got), len(want), got)
	}
	seen := map[ID]int{}
	for i := range want {
		assertExchangeMatches(t, got[i], want[i], fmt.Sprintf("%s item %d", label, i))
		seen[got[i].ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s: exchange %q appears %d times, want exactly once", label, id, n)
		}
	}
}

// ---- 两名患者交错创建、共用接收方：各自清单完整且不混合 ----

func TestListExchangesReturnsOwnHistoryPerPatient(t *testing.T) {
	f := setupListExchangeFixture(t)

	// 交错创建：甲→rcv、乙→rcv、甲→rcvB、乙→rcv。两人共用接收方 rcv，
	// 且全部由同一内部使用者 doc 发起。
	xA1 := f.create(t, f.pidA, rcv.ID, f.authA.ID, f.recA, "req-list-a1")
	f.clk.t = f.clk.t.Add(time.Minute)
	xB1 := f.create(t, f.pidB, rcv.ID, f.authB.ID, f.recB, "req-list-b1")
	f.clk.t = f.clk.t.Add(time.Minute)
	// 同一患者发给另一接收方的交换，仍应列入该患者的清单。
	xA2 := f.create(t, f.pidA, rcvB.ID, f.authAB.ID, f.recA, "req-list-a2")
	f.clk.t = f.clk.t.Add(time.Minute)
	xB2 := f.create(t, f.pidB, rcv.ID, f.authB.ID, f.recB, "req-list-b2")

	wantA := []Exchange{snapshotExchange(xA1), snapshotExchange(xA2)}
	wantB := []Exchange{snapshotExchange(xB1), snapshotExchange(xB2)}

	listA := mustListExchanges(t, f.s, f.pidA)
	listB := mustListExchanges(t, f.s, f.pidB)

	assertExchangeListExactly(t, listA, wantA, "患者甲清单")
	assertExchangeListExactly(t, listB, wantB, "患者乙清单")

	// 共用接收方不能使清单混合：甲的清单里每项都属于甲，乙同理。
	for _, x := range listA {
		if x.PatientID != f.pidA || x.Package.PatientID != f.pidA {
			t.Fatalf("患者甲清单混入他人交换: %+v", x)
		}
	}
	for _, x := range listB {
		if x.PatientID != f.pidB || x.Package.PatientID != f.pidB {
			t.Fatalf("患者乙清单混入他人交换: %+v", x)
		}
	}
}

// ---- 清单保留全部状态、固化包与本患者回执 ----

func TestListExchangesKeepsStatusesFrozenPackagesAndOwnReceipts(t *testing.T) {
	f := setupListExchangeFixture(t)

	// 三份交换分别走向三种状态：待回执、已接受、已拒绝。
	xPend := f.create(t, f.pidA, rcv.ID, f.authA.ID, f.recA, "req-st-pend")
	f.clk.t = f.clk.t.Add(time.Minute)
	xAcc := f.create(t, f.pidA, rcv.ID, f.authA.ID, f.recA, "req-st-acc")
	f.clk.t = f.clk.t.Add(time.Minute)
	xRej := f.create(t, f.pidA, rcvB.ID, f.authAB.ID, f.recA, "req-st-rej")
	f.clk.t = f.clk.t.Add(time.Minute)

	// 打包后更正记录：当前临床版本变为 v2，但清单里的旧包仍显示原内容。
	v2, err := f.s.CorrectRecord(doc, f.recA, 1, listCurrentContent, listCorrectReason)
	if err != nil {
		t.Fatalf("correct after packing: %v", err)
	}

	// 患者乙的交换登记了拒绝回执：不得带入患者甲的清单。
	xB := f.create(t, f.pidB, rcv.ID, f.authB.ID, f.recB, "req-st-b")
	if _, err := f.s.SubmitReceipt(rcv, xB.ID, xB.Digest, ReceiptRejected, "患者乙的拒绝原因"); err != nil {
		t.Fatalf("submit B receipt: %v", err)
	}

	confAcc, err := f.s.SubmitReceipt(rcv, xAcc.ID, xAcc.Digest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("submit accept: %v", err)
	}
	f.clk.t = f.clk.t.Add(time.Minute)
	confRej, err := f.s.SubmitReceipt(rcvB, xRej.ID, xRej.Digest, ReceiptRejected, "患者甲的拒绝原因")
	if err != nil {
		t.Fatalf("submit reject: %v", err)
	}

	lst := mustListExchanges(t, f.s, f.pidA)
	if len(lst) != 3 || lst[0].ID != xPend.ID || lst[1].ID != xAcc.ID || lst[2].ID != xRej.ID {
		t.Fatalf("unexpected list: %+v", lst)
	}

	// 每项都对应实际保存的那份交换（与 GetExchange 单查结果一致）。
	for i, x := range lst {
		g, err := f.s.GetExchange(doc, f.pidA, x.ID)
		if err != nil {
			t.Fatalf("get exchange %q: %v", x.ID, err)
		}
		assertExchangeMatches(t, x, g, fmt.Sprintf("list item %d vs stored exchange", i))
	}

	// 待回执：仍没有回执，不能用一份空回执替代。
	if lst[0].Status != ExchangePending || lst[0].Receipt != nil {
		t.Fatalf("pending item must have no receipt at all: status=%q receipt=%+v",
			lst[0].Status, lst[0].Receipt)
	}

	// 已接受：展示实际登记的结果与时间；接受不保存原因。
	if lst[1].Status != ExchangeAccepted || lst[1].Receipt == nil ||
		lst[1].Receipt.Outcome != ReceiptAccepted || lst[1].Receipt.Reason != "" ||
		!lst[1].Receipt.RegisteredAt.Equal(confAcc.RegisteredAt) {
		t.Fatalf("accepted item receipt wrong: %+v", lst[1])
	}

	// 已拒绝：展示实际登记的结果、原因与时间；不把另一患者的回执带入。
	if lst[2].Status != ExchangeRejected || lst[2].Receipt == nil ||
		lst[2].Receipt.Outcome != ReceiptRejected || lst[2].Receipt.Reason != "患者甲的拒绝原因" ||
		!lst[2].Receipt.RegisteredAt.Equal(confRej.RegisteredAt) {
		t.Fatalf("rejected item receipt wrong: %+v", lst[2])
	}
	for _, x := range lst {
		if x.Receipt != nil && x.Receipt.Reason == "患者乙的拒绝原因" {
			t.Fatalf("另一患者的回执带入清单: %+v", x.Receipt)
		}
	}

	// 包内记录保留创建时固化的版本与正文：更正后清单里的旧包仍显示
	// v1 原内容，而不是当前临床版本 v2。
	for _, x := range lst {
		if len(x.Package.Records) != 1 {
			t.Fatalf("package records = %+v, want exactly the packed record", x.Package.Records)
		}
		pr := x.Package.Records[0]
		if pr.RecordID != f.recA || pr.VersionID != f.recAV1.ID || pr.Version != 1 ||
			pr.Content != listPackagedV1 || !pr.EffectiveAt.Equal(f.recAV1.CreatedAt) {
			t.Fatalf("package not frozen at creation-time version: %+v (v2=%s)", pr, v2.ID)
		}
		// 患者姓名、草稿内容、更正原因、更正后的当前内容都不得进入交换包。
		for _, banned := range []string{
			listPatientAName, listPatientBName, listDraftContent, listCorrectReason, listCurrentContent,
		} {
			if pr.Content == banned || pr.EncounterID == banned || pr.Category == banned {
				t.Fatalf("package carries protected content %q: %+v", banned, pr)
			}
		}
		if x.Package.PatientID != f.pidA {
			t.Fatalf("package patient wrong: %+v", x.Package)
		}
	}

	// 接收方与绑定授权保持原值：xRej 发给 rcvB、绑定 authAB，其余发给 rcv、绑定 authA。
	if lst[0].ReceiverID != rcv.ID || lst[0].AuthorizationID != f.authA.ID ||
		lst[1].ReceiverID != rcv.ID || lst[1].AuthorizationID != f.authA.ID ||
		lst[2].ReceiverID != rcvB.ID || lst[2].AuthorizationID != f.authAB.ID {
		t.Fatalf("receiver/authorization binding changed: %+v", lst)
	}
}

// ---- 排序：创建时间旧到新；时间戳相同按交换标识升序；
//      其他患者同一时刻的交换不参与，回执先后不重排 ----

func TestListExchangesOrderingCreatedAtThenID(t *testing.T) {
	f := setupListExchangeFixture(t)

	// 同一患者两份交换创建时间完全相同。
	x1 := f.create(t, f.pidA, rcv.ID, f.authA.ID, f.recA, "req-ord-1")
	x2 := f.create(t, f.pidA, rcv.ID, f.authA.ID, f.recA, "req-ord-2")
	if x1.ID == x2.ID || !x1.CreatedAt.Equal(x2.CreatedAt) {
		t.Fatalf("fixture broken: ids %q/%q created %v/%v", x1.ID, x2.ID, x1.CreatedAt, x2.CreatedAt)
	}
	// 其他患者恰好在相同时刻创建的交换：不参与当前清单，也不影响排序。
	xB := f.create(t, f.pidB, rcv.ID, f.authB.ID, f.recB, "req-ord-b")

	f.clk.t = f.clk.t.Add(time.Minute)
	x3 := f.create(t, f.pidA, rcv.ID, f.authA.ID, f.recA, "req-ord-3")

	// 回执登记先后与创建顺序相反：先给最晚创建的 x3 登记，再给 x1 登记。
	if _, err := f.s.SubmitReceipt(rcv, x3.ID, x3.Digest, ReceiptAccepted, ""); err != nil {
		t.Fatalf("receipt x3: %v", err)
	}
	if _, err := f.s.SubmitReceipt(rcv, x1.ID, x1.Digest, ReceiptRejected, "先创建后登记"); err != nil {
		t.Fatalf("receipt x1: %v", err)
	}

	// 时间戳相同的两份按交换标识字符串升序。
	wantTie := []ID{x1.ID, x2.ID}
	sort.Strings(wantTie)

	lst := mustListExchanges(t, f.s, f.pidA)
	if len(lst) != 3 {
		t.Fatalf("list len = %d, want 3: %+v", len(lst), lst)
	}
	if lst[0].ID != wantTie[0] || lst[1].ID != wantTie[1] || lst[2].ID != x3.ID {
		t.Fatalf("order wrong: got [%s %s %s], want [%s %s %s]",
			lst[0].ID, lst[1].ID, lst[2].ID, wantTie[0], wantTie[1], x3.ID)
	}
	if !lst[0].CreatedAt.Equal(lst[1].CreatedAt) || !lst[1].CreatedAt.Before(lst[2].CreatedAt) {
		t.Fatalf("created-at ordering broken: %v %v %v",
			lst[0].CreatedAt, lst[1].CreatedAt, lst[2].CreatedAt)
	}
	for _, x := range lst {
		if x.ID == xB.ID {
			t.Fatalf("其他患者的交换进入当前清单: %+v", x)
		}
	}

	// 再查一次：回执登记之后顺序仍然不变。
	lst2 := mustListExchanges(t, f.s, f.pidA)
	if lst2[0].ID != wantTie[0] || lst2[1].ID != wantTie[1] || lst2[2].ID != x3.ID {
		t.Fatalf("re-listing after receipts reordered: %+v", lst2)
	}
}

// ---- 边界与访问控制：空清单、不存在患者、接收方身份、停用/撤回后仍可查、
//      查看本身无副作用 ----

func TestListExchangesAccessBoundariesAndSideEffects(t *testing.T) {
	f := setupListExchangeFixture(t)

	xA := f.create(t, f.pidA, rcv.ID, f.authA.ID, f.recA, "req-edge-a")
	xB := f.create(t, f.pidB, rcv.ID, f.authB.ID, f.recB, "req-edge-b")
	if _, err := f.s.SubmitReceipt(rcv, xB.ID, xB.Digest, ReceiptAccepted, ""); err != nil {
		t.Fatalf("submit B receipt: %v", err)
	}

	// 已登记但没有交换的患者：成功的空清单。
	lst, err := f.s.ListExchanges(doc, f.pidC)
	if err != nil || len(lst) != 0 {
		t.Fatalf("empty-history patient: list=%+v err=%v, want success with empty list", lst, err)
	}

	// 不存在的患者：ErrNotFound。
	if _, err := f.s.ListExchanges(doc, "pat_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing patient err = %v, want ErrNotFound", err)
	}

	// 接收方身份不能调用内部清单入口——即使它确实收过该患者的包：
	// ErrAccessDenied 且没有清单内容。
	if lst, err := f.s.ListExchanges(rcv, f.pidA); !errors.Is(err, ErrAccessDenied) || lst != nil {
		t.Fatalf("receiver list: err = %v, list = %+v, want ErrAccessDenied with no content", err, lst)
	}
	if lst, err := f.s.ListExchanges(rcvB, f.pidA); !errors.Is(err, ErrAccessDenied) || lst != nil {
		t.Fatalf("other receiver list: err = %v, list = %+v", err, lst)
	}
	if lst, err := f.s.ListExchanges(Actor{}, f.pidA); !errors.Is(err, ErrAccessDenied) || lst != nil {
		t.Fatalf("invalid actor list: err = %v, list = %+v", err, lst)
	}

	// 查看本身不生成交换、回执或审计事件。
	auditABefore := len(mustAudit(t, f.s, f.pidA))
	auditBBefore := len(mustAudit(t, f.s, f.pidB))
	before := mustListExchanges(t, f.s, f.pidA)
	want := make([]Exchange, len(before))
	for i, x := range before {
		want[i] = snapshotExchange(x)
	}

	for i := 0; i < 3; i++ {
		mustListExchanges(t, f.s, f.pidA)
		mustListExchanges(t, f.s, f.pidB)
		mustListExchanges(t, f.s, f.pidC)
	}

	if got := len(mustAudit(t, f.s, f.pidA)); got != auditABefore {
		t.Fatalf("listing added audit events for patient A: %d -> %d", auditABefore, got)
	}
	if got := len(mustAudit(t, f.s, f.pidB)); got != auditBBefore {
		t.Fatalf("listing added audit events for patient B: %d -> %d", auditBBefore, got)
	}
	after := mustListExchanges(t, f.s, f.pidA)
	assertExchangeListExactly(t, after, want, "list after repeated listing")
	// 待回执的交换没有因为反复查看而补出回执。
	got, err := f.s.GetExchange(doc, f.pidA, xA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ExchangePending || got.Receipt != nil {
		t.Fatalf("listing created a receipt: %+v", got)
	}

	// 授权撤回后，内部使用者仍能查看此前保存的交换历史。
	if err := f.s.Revoke(doc, f.pidA, f.authA.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	assertExchangeListExactly(t, mustListExchanges(t, f.s, f.pidA), want, "list after authorization revoked")

	// 患者停用后，此前保存的交换历史仍可查看。
	if err := f.s.DeactivatePatient(doc, f.pidA); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	assertExchangeListExactly(t, mustListExchanges(t, f.s, f.pidA), want, "list after patient deactivated")
}
