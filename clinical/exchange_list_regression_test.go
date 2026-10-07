package clinical

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 本文件为 ListExchanges 补回归保障，重点保护“按患者归属返回完整交换历史”：
// 内部使用者查看某位患者的交换清单时，只能拿到这位患者自己的交换——
// 即使其他患者的交换由同一内部使用者创建、发给同一个接收方，也不能混入，
// 这位患者自己的交换也不能被遗漏。
//
// 夹具在同一份本地存储中安排三名合成患者：
//   - 患者甲（pidA）与患者乙（pidB）都已有交换，且创建过程在时间上交错，
//     两人都给同一接收方 rcv 发过包；患者甲还向另一接收方 rcvB 发过包；
//   - 患者丙（pidC）只有档案、从未有过交换。
//
// 清单是内部历史视图：待回执、已接受、已拒绝三种状态都必须保留；每项对应
// 实际保存的那份交换（绑定授权、请求号、接收方、摘要与创建时固化的包），
// 记录后来更正不影响旧包；待回执项不能被补上空回执。

// listExchangeFixture 是清单回归测试的共用夹具。
type listExchangeFixture struct {
	s   *Store
	clk *fakeClock

	pidA ID // 有交换的患者甲
	pidB ID // 有交换的患者乙
	pidC ID // 已登记但没有任何交换的患者丙
	eA   ID // 患者甲就诊
	eB   ID // 患者乙就诊

	recA1     ID      // eA 下已生效诊断：先打包（固化 v1）再更正到 v2
	recA1V1   Version // recA1 打包时的第 1 版
	recA2     ID      // eA 下已生效诊断
	recAO     ID      // eA 下已生效医嘱
	recADraft ID      // eA 下始终未生效的草稿，任何包都不得包含
	recB1     ID      // eB 下已生效诊断
	recB2     ID      // eB 下已生效诊断
	recBO     ID      // eB 下已生效医嘱

	grantA     Authorization // 患者甲 → rcv：覆盖 eA 诊断与医嘱
	grantARcvB Authorization // 患者甲 → rcvB：覆盖 eA 医嘱
	grantB     Authorization // 患者乙 → rcv：覆盖 eB 诊断与医嘱
}

// 清单回归夹具专用的固定文案：患者姓名、草稿正文与更正原因都不应进入交换包，
// 用可检索的固定字符串在清单结果的 JSON 中逐一排除。
const (
	listExchPatientAName = "清单回归患者甲"
	listExchPatientBName = "清单回归患者乙"
	listExchPatientCName = "清单回归患者丙"

	listExchRecA1V1     = "清单回归-甲诊断1-第1版正文"
	listExchRecA1V2     = "清单回归-甲诊断1-第2版更正正文"
	listExchRecA1Reason = "清单回归-甲诊断1更正原因"
	listExchRecA2       = "清单回归-甲诊断2正文"
	listExchRecAO       = "清单回归-甲医嘱正文"
	listExchDraftText   = "清单回归-甲未生效草稿-不得入包"
	listExchRecB1       = "清单回归-乙诊断1正文"
	listExchRecB2       = "清单回归-乙诊断2正文"
	listExchRecBO       = "清单回归-乙医嘱正文"

	listExchRejectAReason = "清单回归-甲医嘱被接收方拒绝"
	listExchRejectBReason = "清单回归-乙医嘱被接收方拒绝"
)

func setupListExchangeFixture(t *testing.T) listExchangeFixture {
	t.Helper()
	s, clk := newTestStore(t)

	register := func(name string) ID {
		t.Helper()
		p, err := s.RegisterPatient(doc, name)
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		return p.ID
	}
	encounter := func(pid ID) ID {
		t.Helper()
		e, err := s.AddEncounter(doc, pid, clk.t)
		if err != nil {
			t.Fatalf("add encounter for %q: %v", pid, err)
		}
		return e.ID
	}
	effective := func(pid, eid ID, category, content string) (ID, Version) {
		t.Helper()
		r, err := s.CreateDraft(doc, pid, eid, category, content)
		if err != nil {
			t.Fatalf("create draft %q: %v", content, err)
		}
		v, err := s.ActivateRecord(doc, r.ID)
		if err != nil {
			t.Fatalf("activate %q: %v", content, err)
		}
		return r.ID, v
	}

	pidA := register(listExchPatientAName)
	pidB := register(listExchPatientBName)
	pidC := register(listExchPatientCName)
	eA := encounter(pidA)
	eB := encounter(pidB)

	recA1, recA1V1 := effective(pidA, eA, Diagnosis, listExchRecA1V1)
	recA2, _ := effective(pidA, eA, Diagnosis, listExchRecA2)
	recAO, _ := effective(pidA, eA, Order, listExchRecAO)
	recB1, _ := effective(pidB, eB, Diagnosis, listExchRecB1)
	recB2, _ := effective(pidB, eB, Diagnosis, listExchRecB2)
	recBO, _ := effective(pidB, eB, Order, listExchRecBO)

	// 始终保持草稿状态：不生效，任何交换都不得携带它。
	dr, err := s.CreateDraft(doc, pidA, eA, Diagnosis, listExchDraftText)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	// 授权窗宽于全部交换与回执时刻，避免时间窗干扰隔离断言。
	st, en := clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)
	grantA, err := s.Grant(doc, pidA, rcv.ID,
		[]Scope{{EncounterID: eA, Category: Diagnosis}, {EncounterID: eA, Category: Order}},
		st, en)
	if err != nil {
		t.Fatalf("grant A -> rcv: %v", err)
	}
	grantARcvB, err := s.Grant(doc, pidA, rcvB.ID,
		[]Scope{{EncounterID: eA, Category: Order}}, st, en)
	if err != nil {
		t.Fatalf("grant A -> rcvB: %v", err)
	}
	grantB, err := s.Grant(doc, pidB, rcv.ID,
		[]Scope{{EncounterID: eB, Category: Diagnosis}, {EncounterID: eB, Category: Order}},
		st, en)
	if err != nil {
		t.Fatalf("grant B -> rcv: %v", err)
	}

	return listExchangeFixture{
		s: s, clk: clk,
		pidA: pidA, pidB: pidB, pidC: pidC, eA: eA, eB: eB,
		recA1: recA1, recA1V1: recA1V1, recA2: recA2, recAO: recAO, recADraft: dr.ID,
		recB1: recB1, recB2: recB2, recBO: recBO,
		grantA: grantA, grantARcvB: grantARcvB, grantB: grantB,
	}
}

// ---- 清单回归测试自用的小工具 ----

func mustListExchange(t *testing.T, s *Store, actor Actor, pid ID, receiver, authID ID, recs []ID, req string) Exchange {
	t.Helper()
	x, err := s.CreateExchange(actor, pid, receiver, authID, recs, req)
	if err != nil {
		t.Fatalf("create exchange %q: %v", req, err)
	}
	return x
}

func mustSubmitReceipt(t *testing.T, s *Store, actor Actor, x Exchange, outcome, reason string) ReceiptConfirmation {
	t.Helper()
	conf, err := s.SubmitReceipt(actor, x.ID, x.Digest, outcome, reason)
	if err != nil {
		t.Fatalf("submit receipt for %q: %v", x.ID, err)
	}
	return conf
}

func exchangeByID(xs []Exchange, id ID) (Exchange, bool) {
	for _, x := range xs {
		if x.ID == id {
			return x, true
		}
	}
	return Exchange{}, false
}

func packagedRecordByID(p Package, recordID ID) (PackagedRecord, bool) {
	for _, pr := range p.Records {
		if pr.RecordID == recordID {
			return pr, true
		}
	}
	return PackagedRecord{}, false
}

// assertExchangeIDOrder 断言清单恰好按给定标识顺序返回，且每份交换只出现一次。
func assertExchangeIDOrder(t *testing.T, got []Exchange, want []ID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("list length = %d, want %d: %+v", len(got), len(want), got)
	}
	seen := make(map[ID]int, len(want))
	for i, x := range got {
		if x.ID != want[i] {
			t.Fatalf("list position %d = %q, want %q; full order: %+v", i, x.ID, want[i], got)
		}
		seen[x.ID]++
		if seen[x.ID] > 1 {
			t.Fatalf("exchange %q listed more than once", x.ID)
		}
	}
}

// assertExchangesChronologicallyOrdered 独立复核排序规则：创建时间从早到晚；
// 创建时间完全相同时按交换标识的字符串升序。
func assertExchangesChronologicallyOrdered(t *testing.T, xs []Exchange) {
	t.Helper()
	for i := 1; i < len(xs); i++ {
		prev, cur := xs[i-1], xs[i]
		if prev.CreatedAt.After(cur.CreatedAt) {
			t.Fatalf("list not ordered by created time: %q at %v after %q at %v",
				cur.ID, cur.CreatedAt, prev.ID, prev.CreatedAt)
		}
		if prev.CreatedAt.Equal(cur.CreatedAt) && prev.ID >= cur.ID {
			t.Fatalf("same created time %v not tie-broken by exchange id ascending: %q before %q",
				cur.CreatedAt, prev.ID, cur.ID)
		}
	}
}

// assertNoInternalOnlyText 断言这份交换（连同固化包与回执）的序列化结果中
// 不出现患者姓名、草稿正文或更正原因等只属于内部视图的内容。
func assertNoInternalOnlyText(t *testing.T, x Exchange, forbidden ...string) {
	t.Helper()
	raw, err := json.Marshal(x)
	if err != nil {
		t.Fatalf("marshal exchange %q: %v", x.ID, err)
	}
	for _, text := range forbidden {
		if strings.Contains(string(raw), text) {
			t.Fatalf("exchange %q leaks internal-only text %q: %s", x.ID, text, raw)
		}
	}
}

func totalExchanges(s *Store) int {
	var n int
	_ = s.view(func(snap *snapshot) error { n = len(snap.Exchanges); return nil })
	return n
}

// ---- 主场景：两名患者交错创建、共用接收方，清单仍按患者归属返回各自完整历史 ----

func TestListExchangesScopedToPatientWithCompleteHistory(t *testing.T) {
	f := setupListExchangeFixture(t)
	base := f.clk.t

	// t0：A→rcv、B→rcv、A→rcv 连续创建，三份交换创建时刻完全相同（交错）。
	// 患者乙的 b1 与患者甲两份同刻存在，但不参与患者甲清单的排序。
	a1 := mustListExchange(t, f.s, doc, f.pidA, rcv.ID, f.grantA.ID, []ID{f.recA1}, "req-A1")
	b1 := mustListExchange(t, f.s, doc, f.pidB, rcv.ID, f.grantB.ID, []ID{f.recB1}, "req-B1")
	a3 := mustListExchange(t, f.s, doc, f.pidA, rcv.ID, f.grantA.ID, []ID{f.recA2}, "req-A3")

	// t1：患者甲发给另一接收方 rcvB——同一患者发给不同接收方的交换仍应一并列出。
	f.clk.t = base.Add(time.Hour)
	a2 := mustListExchange(t, f.s, doc, f.pidA, rcvB.ID, f.grantARcvB.ID, []ID{f.recAO}, "req-A2")

	// t2：患者乙→rcv 第二份（b3，随后被拒绝）。
	f.clk.t = base.Add(2 * time.Hour)
	b3 := mustListExchange(t, f.s, doc, f.pidB, rcv.ID, f.grantB.ID, []ID{f.recBO}, "req-B3")

	// t3：患者乙→rcv 第三份（b2，保持待回执；创建晚于 b3）。
	f.clk.t = base.Add(3 * time.Hour)
	b2 := mustListExchange(t, f.s, doc, f.pidB, rcv.ID, f.grantB.ID, []ID{f.recB2}, "req-B2")

	// 打包 a1 之后才把 recA1 更正到第 2 版：a1 的旧包必须继续显示第 1 版。
	f.clk.t = base.Add(4 * time.Hour)
	recA1V2, err := f.s.CorrectRecord(doc, f.recA1, f.recA1V1.Number, listExchRecA1V2, listExchRecA1Reason)
	if err != nil {
		t.Fatalf("correct recA1 after packaging: %v", err)
	}

	// 回执登记时刻彼此不同，且全部晚于交换创建时刻——登记先后不得重排清单。
	f.clk.t = base.Add(5 * time.Hour)
	confA3 := mustSubmitReceipt(t, f.s, rcv, a3, ReceiptAccepted, "")
	f.clk.t = base.Add(6 * time.Hour)
	confA2 := mustSubmitReceipt(t, f.s, rcvB, a2, ReceiptRejected, listExchRejectAReason)
	f.clk.t = base.Add(7 * time.Hour)
	confB1 := mustSubmitReceipt(t, f.s, rcv, b1, ReceiptAccepted, "")
	f.clk.t = base.Add(8 * time.Hour)
	confB3 := mustSubmitReceipt(t, f.s, rcv, b3, ReceiptRejected, listExchRejectBReason)
	// a1、b2 刻意保持待回执。

	// 临床当前版本确已前进到第 2 版，作为“清单旧包不跟随当前版本”的对照。
	rowsA, err := f.s.EncounterRecords(doc, f.pidA, f.eA)
	if err != nil {
		t.Fatalf("encounter records A: %v", err)
	}
	curA1 := func() *RecordHistory {
		for i := range rowsA {
			if rowsA[i].Record.ID == f.recA1 {
				return &rowsA[i]
			}
		}
		return nil
	}()
	if curA1 == nil || curA1.CurrentVersion == nil ||
		curA1.CurrentVersion.Number != 2 || curA1.CurrentVersion.ID != recA1V2.ID ||
		curA1.CurrentVersion.Content != listExchRecA1V2 {
		t.Fatalf("clinical current version must be v2 after correction: %+v", curA1)
	}

	// 同一时刻的两份患者甲交换按交换标识字符串升序；其余按创建时间。
	aEarly, aLate := a1.ID, a3.ID
	if aEarly > aLate {
		aEarly, aLate = aLate, aEarly
	}

	xsA, err := f.s.ListExchanges(doc, f.pidA)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	assertExchangeIDOrder(t, xsA, []ID{aEarly, aLate, a2.ID})
	assertExchangesChronologicallyOrdered(t, xsA)

	xsB, err := f.s.ListExchanges(doc, f.pidB)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	// b3 创建(t2)早于 b2(t3)：即使 b3 已登记回执、b2 仍待回执，顺序仍是 b1,b3,b2。
	assertExchangeIDOrder(t, xsB, []ID{b1.ID, b3.ID, b2.ID})
	assertExchangesChronologicallyOrdered(t, xsB)

	// ---- 归属隔离：两人清单互不混合、互不遗漏，共用接收方 rcv 不影响归属 ----
	for _, x := range xsA {
		if x.PatientID != f.pidA {
			t.Fatalf("patient B exchange %q leaked into patient A list", x.ID)
		}
		if x.ID == b1.ID || x.ID == b2.ID || x.ID == b3.ID {
			t.Fatalf("patient B exchange leaked into patient A list: %+v", x)
		}
	}
	for _, x := range xsB {
		if x.PatientID != f.pidB {
			t.Fatalf("patient A exchange %q leaked into patient B list", x.ID)
		}
		if x.ID == a1.ID || x.ID == a2.ID || x.ID == a3.ID {
			t.Fatalf("patient A exchange leaked into patient B list: %+v", x)
		}
	}

	// ---- 每项对应实际保存的那份交换：与创建时返回值 / GetExchange 逐一一致 ----
	gotA1, ok := exchangeByID(xsA, a1.ID)
	if !ok {
		t.Fatal("a1 missing from patient A list")
	}
	gotA2, _ := exchangeByID(xsA, a2.ID)
	gotA3, _ := exchangeByID(xsA, a3.ID)
	gotB1, _ := exchangeByID(xsB, b1.ID)
	gotB2, _ := exchangeByID(xsB, b2.ID)
	gotB3, _ := exchangeByID(xsB, b3.ID)

	// 与库内正式保存的交换逐字段一致（GetExchange 独立读取）。
	for _, wantX := range []Exchange{a1, a2, a3} {
		stored, err := f.s.GetExchange(doc, f.pidA, wantX.ID)
		if err != nil {
			t.Fatalf("get stored A exchange %q: %v", wantX.ID, err)
		}
		listed, _ := exchangeByID(xsA, wantX.ID)
		if !reflect.DeepEqual(listed, stored) {
			t.Fatalf("listed A exchange %q diverges from stored:\nlisted=%+v\nstored=%+v",
				wantX.ID, listed, stored)
		}
	}
	for _, wantX := range []Exchange{b1, b2, b3} {
		stored, err := f.s.GetExchange(doc, f.pidB, wantX.ID)
		if err != nil {
			t.Fatalf("get stored B exchange %q: %v", wantX.ID, err)
		}
		listed, _ := exchangeByID(xsB, wantX.ID)
		if !reflect.DeepEqual(listed, stored) {
			t.Fatalf("listed B exchange %q diverges from stored:\nlisted=%+v\nstored=%+v",
				wantX.ID, listed, stored)
		}
	}

	// a1：待回执，整份仍等于创建时固化的交换——绑定授权、请求号、接收方、摘要原样，
	// 包内 recA1 停在第 1 版（版本标识与正文都是创建时的值），没有空回执。
	if !reflect.DeepEqual(gotA1, snapshotExchange(a1)) {
		t.Fatalf("pending a1 changed in list:\n got=%+v\nwant=%+v", gotA1, a1)
	}
	if gotA1.Status != ExchangePending || gotA1.Receipt != nil {
		t.Fatalf("a1 must stay pending with no receipt: %+v", gotA1)
	}
	if gotA1.AuthorizationID != f.grantA.ID || gotA1.RequestID != "req-A1" ||
		gotA1.ReceiverID != rcv.ID || gotA1.CreatorID != doc.ID ||
		gotA1.Digest != a1.Digest {
		t.Fatalf("a1 binding fields altered: %+v", gotA1)
	}
	prA1, ok := packagedRecordByID(gotA1.Package, f.recA1)
	if !ok {
		t.Fatalf("a1 package lost recA1: %+v", gotA1.Package.Records)
	}
	if prA1.Version != 1 || prA1.VersionID != f.recA1V1.ID || prA1.Content != listExchRecA1V1 {
		t.Fatalf("a1 package must keep frozen v1, not current clinical v2: %+v", prA1)
	}
	if !prA1.EffectiveAt.Equal(f.recA1V1.CreatedAt) {
		t.Fatalf("a1 frozen effective-at %v != %v", prA1.EffectiveAt, f.recA1V1.CreatedAt)
	}

	// b2：另一名患者的待回执项同样没有回执——同一接收方 rcv 对 a3、b1 的接受
	// 不能被带到 b2（或 a1）上。
	if gotB2.Status != ExchangePending || gotB2.Receipt != nil {
		t.Fatalf("b2 must stay pending with no receipt: %+v", gotB2)
	}
	if !reflect.DeepEqual(gotB2, snapshotExchange(b2)) {
		t.Fatalf("pending b2 changed in list: %+v", gotB2)
	}

	// 已接受项：状态与实际登记的回执一致（接受不携带原因），登记时间取实际登记时刻。
	if gotA3.Status != ExchangeAccepted || gotA3.Receipt == nil {
		t.Fatalf("a3 must be accepted with receipt: %+v", gotA3)
	}
	if gotA3.Receipt.Outcome != ReceiptAccepted || gotA3.Receipt.Reason != "" ||
		!gotA3.Receipt.RegisteredAt.Equal(confA3.RegisteredAt) {
		t.Fatalf("a3 receipt must be the registered acceptance: list=%+v conf=%+v",
			*gotA3.Receipt, confA3)
	}
	if gotB1.Status != ExchangeAccepted || gotB1.Receipt == nil ||
		gotB1.Receipt.Outcome != ReceiptAccepted ||
		!gotB1.Receipt.RegisteredAt.Equal(confB1.RegisteredAt) {
		t.Fatalf("b1 acceptance receipt not preserved: %+v", gotB1)
	}

	// 已拒绝项：实际登记的结果、原因与时间原样展示。
	if gotA2.Status != ExchangeRejected || gotA2.ReceiverID != rcvB.ID ||
		gotA2.AuthorizationID != f.grantARcvB.ID || gotA2.RequestID != "req-A2" {
		t.Fatalf("a2 header altered: %+v", gotA2)
	}
	if gotA2.Receipt == nil || gotA2.Receipt.Outcome != ReceiptRejected ||
		gotA2.Receipt.Reason != listExchRejectAReason ||
		!gotA2.Receipt.RegisteredAt.Equal(confA2.RegisteredAt) {
		t.Fatalf("a2 rejection receipt not preserved: %+v", gotA2.Receipt)
	}
	if gotB3.Receipt == nil || gotB3.Receipt.Outcome != ReceiptRejected ||
		gotB3.Receipt.Reason != listExchRejectBReason ||
		!gotB3.Receipt.RegisteredAt.Equal(confB3.RegisteredAt) {
		t.Fatalf("b3 rejection receipt not preserved: %+v", gotB3.Receipt)
	}
	// 拒绝原因不能跨交换、跨患者混用。
	if gotA2.Receipt.Reason == gotB3.Receipt.Reason {
		t.Fatal("rejection reasons must stay bound to their own exchanges")
	}
	if gotB1.Receipt.Reason != "" || gotA3.Receipt.Reason != "" {
		t.Fatal("accepted receipts must carry no reason in the list")
	}

	// 三种状态在各自清单中都齐全。
	if !statusesPresent(gotA1, gotA2, gotA3) {
		t.Fatal("patient A list must retain pending, accepted and rejected exchanges")
	}
	if !statusesPresent(gotB1, gotB2, gotB3) {
		t.Fatal("patient B list must retain pending, accepted and rejected exchanges")
	}

	// 患者姓名、草稿正文、更正原因与第 2 版正文都不得进入任何交换包；
	// 旧包仍含第 1 版正文，而不是当前临床版本。
	for _, x := range xsA {
		assertNoInternalOnlyText(t, x,
			listExchPatientAName, listExchPatientBName,
			listExchDraftText, listExchRecA1Reason, listExchRecA1V2)
	}
	for _, x := range xsB {
		assertNoInternalOnlyText(t, x,
			listExchPatientAName, listExchPatientBName,
			listExchDraftText, listExchRecA1Reason, listExchRecA1V2)
	}
	if strings.Contains(marshalOrFail(t, gotA1), listExchRecA1V1) == false {
		t.Fatal("a1 frozen package must still contain the v1 content")
	}
	if _, leaked := packagedRecordByID(gotA1.Package, f.recADraft); leaked {
		t.Fatal("draft record must never be packaged")
	}

	// 已登记但没有交换的患者：成功的空清单（无错误）。
	emptyA, err := f.s.ListExchanges(doc, f.pidC)
	if err != nil {
		t.Fatalf("registered patient without exchanges must yield successful empty list: %v", err)
	}
	if len(emptyA) != 0 {
		t.Fatalf("patient C list must be empty, got %+v", emptyA)
	}

	// 不存在的患者：ErrNotFound，且不返回清单内容。
	if got, err := f.s.ListExchanges(doc, "pat_missing"); !errors.Is(err, ErrNotFound) || len(got) != 0 {
		t.Fatalf("missing patient: list=%d err=%v, want 0, ErrNotFound", len(got), err)
	}

	// 接收方身份即使确实收过该患者的包，也不能调用内部清单入口：
	// ErrAccessDenied 且没有清单内容。
	if got, err := f.s.ListExchanges(rcv, f.pidA); !errors.Is(err, ErrAccessDenied) || len(got) != 0 {
		t.Fatalf("holder receiver listing A: list=%d err=%v, want 0, ErrAccessDenied", len(got), err)
	}
	if got, err := f.s.ListExchanges(rcv, f.pidB); !errors.Is(err, ErrAccessDenied) || len(got) != 0 {
		t.Fatalf("holder receiver listing B: list=%d err=%v, want 0, ErrAccessDenied", len(got), err)
	}
	if got, err := f.s.ListExchanges(rcvB, f.pidA); !errors.Is(err, ErrAccessDenied) || len(got) != 0 {
		t.Fatalf("rcvB listing A: list=%d err=%v, want 0, ErrAccessDenied", len(got), err)
	}
	if _, err := f.s.ListExchanges(ReceiverActor("rcv-c"), f.pidA); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("stranger receiver err = %v, want ErrAccessDenied", err)
	}
	if _, err := f.s.ListExchanges(Actor{ID: "x", Kind: "other"}, f.pidA); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("unknown actor kind err = %v, want ErrAccessDenied", err)
	}
}

func statusesPresent(xs ...Exchange) bool {
	seen := map[string]bool{}
	for _, x := range xs {
		seen[x.Status] = true
	}
	return seen[ExchangePending] && seen[ExchangeAccepted] && seen[ExchangeRejected]
}

func marshalOrFail(t *testing.T, x Exchange) string {
	t.Helper()
	raw, err := json.Marshal(x)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// ---- 患者停用或授权撤回后，内部使用者仍能查看完整历史；查看本身不留痕 ----

func TestListExchangesReadableAfterRevokeAndDeactivationAndCreatesNothing(t *testing.T) {
	f := setupListExchangeFixture(t)

	xA := mustListExchange(t, f.s, doc, f.pidA, rcv.ID, f.grantA.ID, []ID{f.recA1}, "req-hist-A")
	f.clk.t = f.clk.t.Add(time.Hour)
	xB := mustListExchange(t, f.s, doc, f.pidB, rcv.ID, f.grantB.ID, []ID{f.recB1}, "req-hist-B")
	baselineA := snapshotExchange(xA)
	baselineB := snapshotExchange(xB)

	// 撤回患者甲交换绑定的授权，并停用患者甲。
	if err := f.s.Revoke(doc, f.pidA, f.grantA.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// 撤回后接收方取包已被拒（既有规则不变），但这不影响内部历史视图。
	if _, err := f.s.FetchPackage(rcv, xA.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("fetch after revoke err = %v, want ErrAccessDenied", err)
	}
	if err := f.s.DeactivatePatient(doc, f.pidA); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	auditABefore := len(mustAudit(t, f.s, f.pidA))
	auditBBefore := len(mustAudit(t, f.s, f.pidB))
	exchangesBefore := totalExchanges(f.s)

	// 停用与撤回后：患者甲此前保存的交换仍完整可取（原包、待回执、无空回执）。
	xsA, err := f.s.ListExchanges(doc, f.pidA)
	if err != nil {
		t.Fatalf("list A after revoke+deactivate: %v", err)
	}
	if len(xsA) != 1 {
		t.Fatalf("patient A history must survive: got %d exchanges", len(xsA))
	}
	if !reflect.DeepEqual(xsA[0], baselineA) {
		t.Fatalf("A exchange altered after revoke+deactivate:\n got=%+v\nwant=%+v", xsA[0], baselineA)
	}
	if xsA[0].Status != ExchangePending || xsA[0].Receipt != nil {
		t.Fatalf("A exchange must remain pending with no receipt: %+v", xsA[0])
	}
	pr, ok := packagedRecordByID(xsA[0].Package, f.recA1)
	if !ok || pr.Version != 1 || pr.VersionID != f.recA1V1.ID || pr.Content != listExchRecA1V1 {
		t.Fatalf("frozen A package not preserved after revoke+deactivate: %+v", xsA[0].Package)
	}
	// 单个查看入口同样可用。
	if g, err := f.s.GetExchange(doc, f.pidA, xA.ID); err != nil || !reflect.DeepEqual(g, baselineA) {
		t.Fatalf("GetExchange after revoke+deactivate: %+v err=%v", g, err)
	}

	// 患者乙不受牵连。
	xsB, err := f.s.ListExchanges(doc, f.pidB)
	if err != nil {
		t.Fatalf("list B after A deactivated: %v", err)
	}
	if len(xsB) != 1 || !reflect.DeepEqual(xsB[0], baselineB) {
		t.Fatalf("patient B history affected by patient A deactivation: %+v", xsB)
	}

	// 另一名内部使用者同样可以查看停用患者的历史。
	if xs, err := f.s.ListExchanges(doc2, f.pidA); err != nil || len(xs) != 1 {
		t.Fatalf("second internal actor list deactivated patient: %d %v", len(xs), err)
	}

	// 反复查看（成功清单、成功空清单、不存在患者的失败、接收方被拒）都是只读的：
	// 不生成交换、回执或审计事件。
	if _, err := f.s.ListExchanges(doc, f.pidA); err != nil {
		t.Fatalf("relist A: %v", err)
	}
	if _, err := f.s.ListExchanges(doc, f.pidB); err != nil {
		t.Fatalf("relist B: %v", err)
	}
	if _, err := f.s.ListExchanges(doc, f.pidC); err != nil {
		t.Fatalf("list empty C: %v", err)
	}
	if _, err := f.s.ListExchanges(doc, "pat_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing patient must stay ErrNotFound")
	}
	if _, err := f.s.ListExchanges(rcv, f.pidA); !errors.Is(err, ErrAccessDenied) {
		t.Fatal("receiver must stay denied")
	}

	if got := len(mustAudit(t, f.s, f.pidA)); got != auditABefore {
		t.Fatalf("listing added audit events for A: %d -> %d", auditABefore, got)
	}
	if got := len(mustAudit(t, f.s, f.pidB)); got != auditBBefore {
		t.Fatalf("listing added audit events for B: %d -> %d", auditBBefore, got)
	}
	if got := totalExchanges(f.s); got != exchangesBefore {
		t.Fatalf("listing changed exchange count: %d -> %d", exchangesBefore, got)
	}
	if xs, _ := f.s.ListExchanges(doc, f.pidA); xs[0].Receipt != nil {
		t.Fatal("viewing history must not fabricate a receipt")
	}
}

// ---- 重开恢复：清单读取实际落盘的交换——固化旧包与回执原样保留，患者不串 ----

func TestListExchangesPersistFrozenPackagesAndReceiptsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	register := func(name string) ID {
		p, err := s.RegisterPatient(doc, name)
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	pidA := register("重开清单患者甲")
	pidB := register("重开清单患者乙")
	eA, _ := s.AddEncounter(doc, pidA, clk.t)
	eB, _ := s.AddEncounter(doc, pidB, clk.t)
	dr, _ := s.CreateDraft(doc, pidA, eA.ID, Diagnosis, "重开-甲诊断v1")
	v1, err := s.ActivateRecord(doc, dr.ID)
	if err != nil {
		t.Fatal(err)
	}
	br, _ := s.CreateDraft(doc, pidB, eB.ID, Diagnosis, "重开-乙诊断v1")
	if _, err := s.ActivateRecord(doc, br.ID); err != nil {
		t.Fatal(err)
	}
	st, en := clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)
	gA, _ := s.Grant(doc, pidA, rcv.ID, []Scope{{EncounterID: eA.ID, Category: Diagnosis}}, st, en)
	gB, _ := s.Grant(doc, pidB, rcv.ID, []Scope{{EncounterID: eB.ID, Category: Diagnosis}}, st, en)

	// 两名患者发给同一接收方，创建时刻交错、完全相同。
	xA, err := s.CreateExchange(doc, pidA, rcv.ID, gA.ID, []ID{dr.ID}, "req-reopen-A")
	if err != nil {
		t.Fatal(err)
	}
	xB, err := s.CreateExchange(doc, pidB, rcv.ID, gB.ID, []ID{br.ID}, "req-reopen-B")
	if err != nil {
		t.Fatal(err)
	}

	// 打包后更正：重开后的清单旧包仍须显示 v1。
	if _, err := s.CorrectRecord(doc, dr.ID, 1, "重开-甲诊断v2当前版本", "重开后更正"); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	conf, err := s.SubmitReceipt(rcv, xA.ID, xA.Digest, ReceiptRejected, "重开前登记的拒绝原因")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	xsA, err := s2.ListExchanges(doc, pidA)
	if err != nil {
		t.Fatalf("list A after reopen: %v", err)
	}
	if len(xsA) != 1 || xsA[0].ID != xA.ID {
		t.Fatalf("patient A list after reopen = %+v", xsA)
	}
	got := xsA[0]
	if got.Status != ExchangeRejected || got.Receipt == nil ||
		got.Receipt.Outcome != ReceiptRejected || got.Receipt.Reason != "重开前登记的拒绝原因" ||
		!got.Receipt.RegisteredAt.Equal(conf.RegisteredAt) {
		t.Fatalf("registered receipt not preserved across reopen: %+v", got.Receipt)
	}
	if len(got.Package.Records) != 1 {
		t.Fatalf("package records = %d, want 1", len(got.Package.Records))
	}
	pr := got.Package.Records[0]
	if pr.VersionID != v1.ID || pr.Version != 1 || pr.Content != "重开-甲诊断v1" {
		t.Fatalf("frozen v1 package lost across reopen: %+v", pr)
	}
	if got.Digest != xA.Digest || got.AuthorizationID != gA.ID ||
		got.RequestID != "req-reopen-A" || got.ReceiverID != rcv.ID {
		t.Fatalf("exchange binding fields lost across reopen: %+v", got)
	}

	// 患者乙的清单互不串扰：乙仍只有自己的待回执交换。
	xsB, err := s2.ListExchanges(doc, pidB)
	if err != nil {
		t.Fatalf("list B after reopen: %v", err)
	}
	if len(xsB) != 1 || xsB[0].ID != xB.ID || xsB[0].Status != ExchangePending ||
		xsB[0].Receipt != nil {
		t.Fatalf("patient B list after reopen = %+v", xsB)
	}

	// 已登记但没有交换的患者重开后仍是成功空清单；不存在的患者仍是 ErrNotFound。
	pidC, err := s2.RegisterPatient(doc, "重开清单患者丙")
	if err != nil {
		t.Fatal(err)
	}
	if empty, err := s2.ListExchanges(doc, pidC.ID); err != nil || len(empty) != 0 {
		t.Fatalf("empty list after reopen: %+v err=%v", empty, err)
	}
	if _, err := s2.ListExchanges(doc, "pat_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing patient after reopen err = %v, want ErrNotFound", err)
	}
}
