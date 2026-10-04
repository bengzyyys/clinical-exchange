package clinical

import (
	"errors"
	"testing"
	"time"
)

// ---- 限定授权夹具 ----

type selectiveFixture struct {
	s      *Store
	clk    *fakeClock
	pid    ID
	e1, e2 ID
	r1, r2 ID // e1 下已生效诊断
	r1v1   Version
	r3     ID // e1 下已生效医嘱
	draft  ID // e1 下仅草稿诊断
	e2rec  ID // e2 下已生效诊断
	start  time.Time
	end    time.Time
}

func setupSelective(t *testing.T) selectiveFixture {
	t.Helper()
	s, clk := newTestStore(t)
	pid, e1 := setupPatientEncounter(t, s)
	e2enc, err := s.AddEncounter(doc, pid, clk.t.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	mkDiag := func(eid ID, content string) ID {
		t.Helper()
		r, err := s.CreateDraft(doc, pid, eid, Diagnosis, content)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ActivateRecord(doc, r.ID); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	r1 := mkDiag(e1, "诊断1")
	r2 := mkDiag(e1, "诊断2")
	e2rec := mkDiag(e2enc.ID, "二诊诊断")

	o, err := s.CreateDraft(doc, pid, e1, Order, "医嘱1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, o.ID); err != nil {
		t.Fatal(err)
	}
	dr, err := s.CreateDraft(doc, pid, e1, Diagnosis, "未生效草稿")
	if err != nil {
		t.Fatal(err)
	}

	chart, _ := s.Chart(doc, pid)
	var r1v1 Version
	for _, h := range chart.Records {
		if h.Record.ID == r1 {
			r1v1 = *h.CurrentVersion
		}
	}

	return selectiveFixture{
		s: s, clk: clk, pid: pid, e1: e1, e2: e2enc.ID,
		r1: r1, r2: r2, r1v1: r1v1, r3: o.ID, draft: dr.ID, e2rec: e2rec,
		start: clk.t.Add(-time.Hour), end: clk.t.Add(24 * time.Hour),
	}
}

func sel(eid ID, category string, rid ID) RecordSelection {
	return RecordSelection{EncounterID: eid, Category: category, RecordID: rid}
}

func readRecordIDs(res ReadResult) map[ID]bool {
	m := map[ID]bool{}
	for _, r := range res.Records {
		m[r.RecordID] = true
	}
	return m
}

// ---- 建立限定授权：参数校验 ----

func TestGrantSelectiveValidation(t *testing.T) {
	f := setupSelective(t)
	other, _ := f.s.RegisterPatient(doc, "合成患者乙")
	encOther, _ := f.s.AddEncounter(doc, other.ID, time.Time{})
	recOther, _ := f.s.CreateDraft(doc, other.ID, encOther.ID, Diagnosis, "他人记录")
	if _, err := f.s.ActivateRecord(doc, recOther.ID); err != nil {
		t.Fatal(err)
	}

	st, en := f.start, f.end

	// 空选择不能被当成整类授权。
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil, nil, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty both err = %v", err)
	}
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil, []RecordSelection{}, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty selections err = %v", err)
	}
	// 空白标识、非法类别。
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, "  ")}, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank record id err = %v", err)
	}
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel("  ", Diagnosis, f.r1)}, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank encounter id err = %v", err)
	}
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, "bogus", f.r1)}, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad category err = %v", err)
	}
	// 不存在的记录。
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, "rec_missing")}, st, en); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing record err = %v, want ErrNotFound", err)
	}
	// 跨患者记录。
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, recOther.ID)}, st, en); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient record err = %v, want ErrMismatchedPatient", err)
	}
	// 尚未生效的草稿。
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, f.draft)}, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("draft selection err = %v, want ErrInvalidArgument", err)
	}
	// 记录与声明的就诊不符。
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e2, Diagnosis, f.r1)}, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("encounter mismatch err = %v, want ErrInvalidArgument", err)
	}
	// 记录与声明的类别不符。
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Order, f.r1)}, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("category mismatch err = %v, want ErrInvalidArgument", err)
	}
	// 跨患者的声明就诊。
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: encOther.ID, Category: Diagnosis}}, nil, st, en); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient scope err = %v", err)
	}
	// 时间窗规则沿用。
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, f.r1)}, en, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("time window err = %v", err)
	}

	// 任一选择不合法就拒绝整条授权：不保存合法部分，也不新增审计。
	auditBefore := len(mustAudit(t, f.s, f.pid))
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{
			sel(f.e1, Diagnosis, f.r1),    // 合法
			sel(f.e1, Diagnosis, f.draft), // 非法：草稿
		}, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("one invalid selection err = %v", err)
	}
	if auths, _ := f.s.ListAuthorizations(doc, f.pid, ""); len(auths) != 0 {
		t.Fatalf("rejected grant persisted valid part: %d authorizations", len(auths))
	}
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("rejected grant added audit: before=%d after=%d", auditBefore, got)
	}
}

// ---- 建立限定授权：去重、整类并存、内部视图 ----

func TestGrantSelectiveDedupAndInternalView(t *testing.T) {
	f := setupSelective(t)

	a, err := f.s.GrantSelective(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e2, Category: Diagnosis}},
		[]RecordSelection{
			sel(f.e1, Diagnosis, f.r2),
			sel(f.e1, Diagnosis, f.r1),
			sel(f.e1, Diagnosis, f.r1), // 重复选择同一记录只算一次
			sel(f.e1, Order, f.r3),
		},
		f.start, f.end)
	if err != nil {
		t.Fatalf("grant selective: %v", err)
	}
	if len(a.Scopes) != 1 || a.Scopes[0] != (Scope{EncounterID: f.e2, Category: Diagnosis}) {
		t.Fatalf("scopes wrong: %+v", a.Scopes)
	}
	if len(a.Selections) != 3 {
		t.Fatalf("selections = %d, want 3 (duplicate merged)", len(a.Selections))
	}
	// 选定记录按记录标识稳定排序（标识为随机生成，按集合核对内容与顺序）。
	gotSels := map[RecordSelection]bool{}
	for _, sc := range a.Selections {
		gotSels[sc] = true
	}
	for _, want := range []RecordSelection{
		sel(f.e1, Diagnosis, f.r1),
		sel(f.e1, Diagnosis, f.r2),
		sel(f.e1, Order, f.r3),
	} {
		if !gotSels[want] {
			t.Fatalf("selection %+v missing from %+v", want, a.Selections)
		}
	}
	for i := 1; i < len(a.Selections); i++ {
		if a.Selections[i-1].RecordID > a.Selections[i].RecordID {
			t.Fatalf("selections not sorted by record id: %+v", a.Selections)
		}
	}
	got, err := f.s.GetAuthorization(doc, f.pid, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Selections) != 3 {
		t.Fatalf("get selections = %d", len(got.Selections))
	}
	if listed, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID); err != nil || len(listed) != 1 || len(listed[0].Selections) != 3 {
		t.Fatalf("list selections wrong: %+v %v", listed, err)
	}

	// 旧形式 Grant 仍只有整类范围。
	old, err := f.s.Grant(doc, f.pid, rcvB.ID,
		[]Scope{{EncounterID: f.e1, Category: Diagnosis}}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	if len(old.Selections) != 0 || len(old.Scopes) != 1 {
		t.Fatalf("legacy Grant must stay whole-category only: %+v", old)
	}
}

// ---- Read：限定范围只放行选中记录的当前版本 ----

func TestReadSelectiveOnlySelectedAndTracksCorrection(t *testing.T) {
	f := setupSelective(t)

	a, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, f.r1)}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	_ = a

	res, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != f.r1 {
		t.Fatalf("selective read = %+v, want only r1", res.Records)
	}

	// 后来新增并生效的记录不自动进入限定范围。
	newRec, _ := f.s.CreateDraft(doc, f.pid, f.e1, Diagnosis, "后加诊断")
	if _, err := f.s.ActivateRecord(doc, newRec.ID); err != nil {
		t.Fatal(err)
	}
	res, _ = f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if len(res.Records) != 1 || res.Records[0].RecordID != f.r1 {
		t.Fatalf("new effective record leaked into limited scope: %+v", res.Records)
	}

	// 同类的既有 r2 也始终不在范围内。
	if readRecordIDs(res)[f.r2] {
		t.Fatal("r2 must not be visible under limited grant")
	}

	// 被选记录更正后：授权覆盖它的当前版本。
	v2, err := f.s.CorrectRecord(doc, f.r1, 1, "诊断1-更正", "复核")
	if err != nil {
		t.Fatal(err)
	}
	res, _ = f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if len(res.Records) != 1 || res.Records[0].VersionID != v2.ID || res.Records[0].Content != "诊断1-更正" {
		t.Fatalf("corrected selection not current: %+v", res.Records)
	}

	// 选定 e1 的记录不构成对 e2 的授权。
	if _, err := f.s.Read(rcv, f.pid, f.e2, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("e2 read err = %v, want ErrAccessDenied", err)
	}
	// 选中的是诊断：医嘱仍被拒绝。
	if _, err := f.s.Read(rcv, f.pid, f.e1, Order); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("order read err = %v, want ErrAccessDenied", err)
	}
}

// ---- Read：多授权合集、去重、整类撤回后只剩明确允许 ----

func TestReadUnionOfGrantsAndWholeCategoryRevoke(t *testing.T) {
	f := setupSelective(t)

	// 一条限定授权选 r1；一条整类授权覆盖 e1 诊断（含 r1、r2）。
	limited, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, f.r1)}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Diagnosis}}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}

	// 整类与限定重叠：可以看到整类内容，每条记录只出现一次。
	res, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	ids := readRecordIDs(res)
	if len(res.Records) != 2 || !ids[f.r1] || !ids[f.r2] {
		t.Fatalf("overlapping grants read = %+v", res.Records)
	}

	// 撤回整类授权：只剩限定授权明确允许的 r1。
	if err := f.s.Revoke(doc, f.pid, whole.ID); err != nil {
		t.Fatal(err)
	}
	res, err = f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("read after whole revoke: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != f.r1 {
		t.Fatalf("after whole revoke = %+v, want only selected r1", res.Records)
	}

	// 再撤回限定授权：没有有效授权，明确拒绝，不泄露记录标识或数量。
	if err := f.s.Revoke(doc, f.pid, limited.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis); !errors.Is(err, ErrAccessDenied) || len(res.Records) != 0 {
		t.Fatalf("after all revoke: res=%+v err=%v", res, err)
	}
}

// ---- Read：两条限定授权取合集，每条只出现一次；时间窗独立 ----

func TestReadUnionOfSelectiveGrantsAndTimeWindow(t *testing.T) {
	f := setupSelective(t)

	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, f.r1)}, f.start, f.end); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{
			sel(f.e1, Diagnosis, f.r1), // 与上一条重叠
			sel(f.e1, Diagnosis, f.r2),
		}, f.start, f.end); err != nil {
		t.Fatal(err)
	}
	res, _ := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	ids := readRecordIDs(res)
	if len(res.Records) != 2 || !ids[f.r1] || !ids[f.r2] {
		t.Fatalf("union read = %+v", res.Records)
	}
	for i := 1; i < len(res.Records); i++ {
		if res.Records[i-1].RecordID > res.Records[i].RecordID {
			t.Fatalf("union not in stable id order: %+v", res.Records)
		}
	}

	// 未来才开始的限定授权不参与合并。
	future, err := f.s.GrantSelective(doc, f.pid, rcvB.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, f.r1)},
		f.clk.t.Add(time.Hour), f.clk.t.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Read(rcvB, f.pid, f.e1, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("future grant err = %v", err)
	}
	// 到窗口内：只看到选中的 r1。
	f.clk.t = f.clk.t.Add(90 * time.Minute)
	res, err = f.s.Read(rcvB, f.pid, f.e1, Diagnosis)
	if err != nil || len(res.Records) != 1 || res.Records[0].RecordID != f.r1 {
		t.Fatalf("future grant now active: %+v %v", res, err)
	}
	// 到期后又拒绝。
	_ = future
	f.clk.t = f.end.Add(time.Hour)
	if _, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expired grants err = %v", err)
	}
}

// ---- 交换：每条记录必须被绑定的那一条授权自身覆盖 ----

func TestCreateExchangeBoundToSelectiveAuthorization(t *testing.T) {
	f := setupSelective(t)

	// 绑定授权只选定 r1。
	bound, err := f.s.GrantSelective(doc, f.pid, rcv.ID, nil,
		[]RecordSelection{sel(f.e1, Diagnosis, f.r1)}, f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	// 同一接收方的另一条整类授权覆盖整个 e1 诊断（含 r2），但不能补足绑定授权。
	if _, err := f.s.Grant(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Diagnosis}}, f.start, f.end); err != nil {
		t.Fatal(err)
	}

	// 选中记录打包成功并固化创建时版本。
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, bound.ID, []ID{f.r1}, "req-sel-1")
	if err != nil {
		t.Fatalf("create with selected record: %v", err)
	}
	if len(x.Package.Records) != 1 || x.Package.Records[0].VersionID != f.r1v1.ID {
		t.Fatalf("package wrong: %+v", x.Package.Records)
	}

	// 更正后包不变。
	v2, err := f.s.CorrectRecord(doc, f.r1, 1, "诊断1-v2", "更正")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := f.s.CreateExchange(doc, f.pid, rcv.ID, bound.ID, []ID{f.r1}, "req-sel-1")
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != x.ID || retry.Package.Records[0].VersionID != f.r1v1.ID {
		t.Fatalf("retry refroze package: %+v", retry.Package.Records)
	}
	_ = v2

	// 夹带一条只有其他授权覆盖的 r2：整包拒绝，不留交换或审计，也不占用请求号。
	auditBefore := len(mustAudit(t, f.s, f.pid))
	if _, err := f.s.CreateExchange(doc, f.pid, rcv.ID, bound.ID,
		[]ID{f.r1, f.r2}, "req-sel-2"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("mixed set err = %v, want ErrAccessDenied", err)
	}
	if _, err := f.s.CreateExchange(doc, f.pid, rcv.ID, bound.ID,
		[]ID{f.r2}, "req-sel-2"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("uncovered record err = %v, want ErrAccessDenied", err)
	}
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("failed exchange added audit: %d -> %d", auditBefore, got)
	}
	if xs, _ := f.s.ListExchanges(doc, f.pid); len(xs) != 1 {
		t.Fatalf("failed exchange persisted: %d", len(xs))
	}
	// 请求号未被占用：同请求号改为合法集合应成功。
	x2, err := f.s.CreateExchange(doc, f.pid, rcv.ID, bound.ID, []ID{f.r1}, "req-sel-2")
	if err != nil || x2.ID == x.ID {
		t.Fatalf("request id should not be consumed: x2=%+v err=%v", x2, err)
	}
}

// ---- 交换：混合授权中整类与选定记录都可打包 ----

func TestCreateExchangeMixedAuthorizationScopes(t *testing.T) {
	f := setupSelective(t)

	// 同一条授权：整类覆盖 e1 医嘱，选定 e1 诊断 r1。
	mixed, err := f.s.GrantSelective(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Order}},
		[]RecordSelection{sel(f.e1, Diagnosis, f.r1)},
		f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}
	x, err := f.s.CreateExchange(doc, f.pid, rcv.ID, mixed.ID,
		[]ID{f.r3, f.r1}, "req-mix")
	if err != nil {
		t.Fatalf("mixed exchange: %v", err)
	}
	if len(x.Package.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(x.Package.Records))
	}
	// r2 既不在整类范围（医嘱），也未被选定：拒绝。
	if _, err := f.s.CreateExchange(doc, f.pid, rcv.ID, mixed.ID,
		[]ID{f.r2}, "req-mix-2"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("uncovered diagnosis err = %v", err)
	}
}

// ---- 同一条授权在“接收方查阅”与“创建交换”中的覆盖含义必须一致 ----
//
// 两个功能共用同一套范围解释：任一记录要么两处都覆盖，要么两处都不覆盖；
// 限定范围不外溢、整类范围包含授权后生效记录、覆盖不串到其他就诊，这些结论
// 在查阅与打包两条路径上必须相同。

func TestCoverageConsistentAcrossReadAndExchange(t *testing.T) {
	f := setupSelective(t)

	// 同一条混合授权：整类覆盖 e1 医嘱；选定 e1 诊断 r1。
	grant, err := f.s.GrantSelective(doc, f.pid, rcv.ID,
		[]Scope{{EncounterID: f.e1, Category: Order}},
		[]RecordSelection{sel(f.e1, Diagnosis, f.r1)},
		f.start, f.end)
	if err != nil {
		t.Fatal(err)
	}

	// 查阅：诊断只见明确选中的 r1，同类的 r2 不出现。
	diag, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("read diagnosis: %v", err)
	}
	if ids := readRecordIDs(diag); len(ids) != 1 || !ids[f.r1] {
		t.Fatalf("diagnosis read = %+v, want only selected r1", diag.Records)
	}
	// 查阅：整类医嘱见 r3。
	ord, err := f.s.Read(rcv, f.pid, f.e1, Order)
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	if ids := readRecordIDs(ord); len(ids) != 1 || !ids[f.r3] {
		t.Fatalf("order read = %+v, want whole-category r3", ord.Records)
	}

	// 与查阅一致：r1、r3 各自由这条绑定授权打包成功；r2 两处都不覆盖。
	packagable := func(rid ID) bool {
		_, perr := f.s.CreateExchange(doc, f.pid, rcv.ID, grant.ID,
			[]ID{rid}, "req-"+rid)
		return perr == nil
	}
	if !packagable(f.r1) {
		t.Fatal("selected r1 must be packageable by the bound grant")
	}
	if !packagable(f.r3) {
		t.Fatal("whole-category r3 must be packageable by the bound grant")
	}
	if packagable(f.r2) {
		t.Fatal("non-selected sibling r2 must not be packageable, matching read")
	}

	// 授权建立后新增并生效：限定诊断不外溢，整类医嘱自然包含——
	// 查阅与打包两条路径必须给出同一结论。
	newDiag, _ := f.s.CreateDraft(doc, f.pid, f.e1, Diagnosis, "授权后诊断")
	if _, err := f.s.ActivateRecord(doc, newDiag.ID); err != nil {
		t.Fatal(err)
	}
	newOrder, _ := f.s.CreateDraft(doc, f.pid, f.e1, Order, "授权后医嘱")
	if _, err := f.s.ActivateRecord(doc, newOrder.ID); err != nil {
		t.Fatal(err)
	}
	diag, _ = f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if ids := readRecordIDs(diag); ids[newDiag.ID] {
		t.Fatalf("later diagnosis leaked into selected scope: %+v", diag.Records)
	}
	ord, _ = f.s.Read(rcv, f.pid, f.e1, Order)
	if ids := readRecordIDs(ord); !ids[newOrder.ID] {
		t.Fatalf("later order missing from whole scope: %+v", ord.Records)
	}
	if packagable(newDiag.ID) {
		t.Fatal("later diagnosis must not be packageable under selected scope")
	}
	if !packagable(newOrder.ID) {
		t.Fatal("later order must be packageable under whole scope")
	}

	// 覆盖关系不串到其他就诊：e2 的诊断查阅被拒，也不能由本授权打包。
	if _, err := f.s.Read(rcv, f.pid, f.e2, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other encounter read err = %v, want ErrAccessDenied", err)
	}
	if packagable(f.e2rec) {
		t.Fatal("record from another encounter must not be packageable")
	}
}

// ---- 重开恢复：限定记录、授权状态、历史交换保留 ----

func TestSelectivePersistenceAcrossReopen(t *testing.T) {
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
	p, _ := s.RegisterPatient(doc, "限定持久化患者")
	e, _ := s.AddEncounter(doc, p.ID, time.Time{})
	d1, _ := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断1")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}
	d2, _ := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}
	a, _ := s.GrantSelective(doc, p.ID, rcv.ID, nil,
		[]RecordSelection{sel(e.ID, Diagnosis, d1.ID)},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour))
	x, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{d1.ID}, "req-persist-sel")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.GetAuthorization(doc, p.ID, a.ID)
	if err != nil {
		t.Fatalf("auth after reopen: %v", err)
	}
	if len(got.Scopes) != 0 || len(got.Selections) != 1 || got.Selections[0].RecordID != d1.ID {
		t.Fatalf("limited scope not preserved: %+v", got)
	}
	// 仍只放行选中记录。
	res, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil || len(res.Records) != 1 || res.Records[0].RecordID != d1.ID {
		t.Fatalf("read after reopen: %+v %v", res, err)
	}
	// 历史交换保留，同请求重试返回原交换。
	retry, err := s2.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{d1.ID}, "req-persist-sel")
	if err != nil || retry.ID != x.ID {
		t.Fatalf("retry after reopen: id=%q err=%v", retry.ID, err)
	}
	// 未选中记录仍不能借该授权打包。
	if _, err := s2.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{d2.ID}, "req-persist-sel-2"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("unselected package after reopen err = %v", err)
	}
}
