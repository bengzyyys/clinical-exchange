package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// ---- ListAuthorizations 回归测试夹具 ----
//
// 同一份本地存储中并存：
//   - 两名合成患者（pidA、pidB）授权给同一接收方 rcv；
//   - 患者 pidA 还同时授权给另一接收方 rcvB；
//   - 患者 pidA → rcv 名下覆盖四种授权状态：生效中（整类）、尚未开始
//     （限定记录）、已经到期（整类）、已经撤回（整类与限定并存的混合范围）；
//   - 患者 pidC 只有档案、从未有过授权。
type listAuthFixture struct {
	s   *Store
	clk *fakeClock

	pidA ID
	pidB ID
	pidC ID // 有档案但没有任何授权
	eA1  ID // 患者甲就诊一
	eA2  ID // 患者甲就诊二
	eB1  ID // 患者乙就诊

	recA1 ID // eA1 下已生效诊断
	recA2 ID // eA1 下已生效诊断
	recAO ID // eA1 下已生效医嘱
	recAe ID // eA2 下已生效诊断
	recB1 ID // eB1 下已生效诊断

	active   Authorization // 患者甲 → rcv：已开始未到期，整类 eA1 诊断
	future   Authorization // 患者甲 → rcv：尚未开始，限定 recA1
	expired  Authorization // 患者甲 → rcv：已经到期，整类 eA2 诊断
	revoked  Authorization // 患者甲 → rcv：已撤回，整类 eA1 医嘱 + 限定 recA2
	otherRcv Authorization // 患者甲 → rcvB：生效中，限定 recAO
	grantB   Authorization // 患者乙 → rcv：生效中，限定 recB1
}

func setupListAuthFixture(t *testing.T) listAuthFixture {
	t.Helper()
	s, clk := newTestStore(t)

	addPatient := func(name string) ID {
		t.Helper()
		p, err := s.RegisterPatient(doc, name)
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		return p.ID
	}
	addEncounter := func(pid ID) ID {
		t.Helper()
		e, err := s.AddEncounter(doc, pid, time.Time{})
		if err != nil {
			t.Fatalf("add encounter: %v", err)
		}
		return e.ID
	}
	effRecord := func(pid, eid ID, category, content string) ID {
		t.Helper()
		r, err := s.CreateDraft(doc, pid, eid, category, content)
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if _, err := s.ActivateRecord(doc, r.ID); err != nil {
			t.Fatalf("activate: %v", err)
		}
		return r.ID
	}

	pidA := addPatient("列表回归患者甲")
	pidB := addPatient("列表回归患者乙")
	pidC := addPatient("列表回归患者丙")
	eA1 := addEncounter(pidA)
	eA2 := addEncounter(pidA)
	eB1 := addEncounter(pidB)

	recA1 := effRecord(pidA, eA1, Diagnosis, "甲一诊诊断1")
	recA2 := effRecord(pidA, eA1, Diagnosis, "甲一诊诊断2")
	recAO := effRecord(pidA, eA1, Order, "甲一诊医嘱1")
	recAe := effRecord(pidA, eA2, Diagnosis, "甲二诊诊断1")
	recB1 := effRecord(pidB, eB1, Diagnosis, "乙一诊诊断1")

	now := clk.t
	activeWindow := func() (time.Time, time.Time) { return now.Add(-time.Hour), now.Add(24 * time.Hour) }
	st, en := activeWindow()

	active, err := s.Grant(doc, pidA, rcv.ID,
		[]Scope{{EncounterID: eA1, Category: Diagnosis}}, st, en)
	if err != nil {
		t.Fatalf("active grant: %v", err)
	}
	future, err := s.GrantSelective(doc, pidA, rcv.ID, nil,
		[]RecordSelection{sel(eA1, Diagnosis, recA1)},
		now.Add(24*time.Hour), now.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("future grant: %v", err)
	}
	expired, err := s.Grant(doc, pidA, rcv.ID,
		[]Scope{{EncounterID: eA2, Category: Diagnosis}},
		now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("expired grant: %v", err)
	}
	revoked, err := s.GrantSelective(doc, pidA, rcv.ID,
		[]Scope{{EncounterID: eA1, Category: Order}},
		[]RecordSelection{sel(eA1, Diagnosis, recA2)}, st, en)
	if err != nil {
		t.Fatalf("mixed grant: %v", err)
	}
	if err := s.Revoke(doc, pidA, revoked.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	otherRcv, err := s.GrantSelective(doc, pidA, rcvB.ID, nil,
		[]RecordSelection{sel(eA1, Order, recAO)}, st, en)
	if err != nil {
		t.Fatalf("other receiver grant: %v", err)
	}
	grantB, err := s.GrantSelective(doc, pidB, rcv.ID, nil,
		[]RecordSelection{sel(eB1, Diagnosis, recB1)}, st, en)
	if err != nil {
		t.Fatalf("patient B grant: %v", err)
	}

	return listAuthFixture{
		s: s, clk: clk,
		pidA: pidA, pidB: pidB, pidC: pidC,
		eA1: eA1, eA2: eA2, eB1: eB1,
		recA1: recA1, recA2: recA2, recAO: recAO, recAe: recAe, recB1: recB1,
		active: active, future: future, expired: expired, revoked: revoked,
		otherRcv: otherRcv, grantB: grantB,
	}
}

func assertAuthIDsSorted(t *testing.T, auths []Authorization) {
	t.Helper()
	for i := 1; i < len(auths); i++ {
		if auths[i-1].ID >= auths[i].ID {
			t.Fatalf("authorizations not sorted ascending by id: %q before %q",
				auths[i-1].ID, auths[i].ID)
		}
	}
}

func assertAuthIDSet(t *testing.T, got []Authorization, want ...ID) {
	t.Helper()
	wantSet := map[ID]bool{}
	for _, id := range want {
		wantSet[id] = true
	}
	gotSet := map[ID]bool{}
	for _, a := range got {
		if gotSet[a.ID] {
			t.Fatalf("authorization %q listed more than once", a.ID)
		}
		gotSet[a.ID] = true
	}
	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Fatalf("authorization id set = %v, want %v", gotSet, wantSet)
	}
}

func findListedAuth(t *testing.T, auths []Authorization, id ID) Authorization {
	t.Helper()
	for _, a := range auths {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("authorization %q missing from list", id)
	return Authorization{}
}

func listedAuthByID(auths []Authorization, id ID) (Authorization, bool) {
	for _, a := range auths {
		if a.ID == id {
			return a, true
		}
	}
	return Authorization{}, false
}

func totalAuthorizations(s *Store) int {
	var n int
	_ = s.view(func(snap *snapshot) error { n = len(snap.Authorizations); return nil })
	return n
}

// ---- 患者与接收方两个条件必须同时成立；按授权标识升序 ----

func TestListAuthorizationsFiltersByPatientAndReceiver(t *testing.T) {
	f := setupListAuthFixture(t)

	// 不指定接收方：所选患者的全部授权（两名接收方各自的授权都在）。
	allA, err := f.s.ListAuthorizations(doc, f.pidA, "")
	if err != nil {
		t.Fatalf("list patient A: %v", err)
	}
	assertAuthIDSet(t, allA, f.active.ID, f.future.ID, f.expired.ID, f.revoked.ID, f.otherRcv.ID)
	assertAuthIDsSorted(t, allA)

	// 指定接收方 rcv：患者甲授予 rcv 的四条，按标识升序。
	aRcv, err := f.s.ListAuthorizations(doc, f.pidA, rcv.ID)
	if err != nil {
		t.Fatalf("list A/rcv: %v", err)
	}
	assertAuthIDSet(t, aRcv, f.active.ID, f.future.ID, f.expired.ID, f.revoked.ID)
	assertAuthIDsSorted(t, aRcv)
	for _, a := range aRcv {
		if a.PatientID != f.pidA || a.ReceiverID != rcv.ID {
			t.Fatalf("filter leaked row %+v", a)
		}
	}

	// 不能因为接收方相同而带出患者乙授予 rcv 的授权。
	if leaked, ok := listedAuthByID(aRcv, f.grantB.ID); ok {
		t.Fatalf("patient B grant leaked into patient A list via shared receiver: %+v", leaked)
	}

	// 指定接收方 rcvB：只留下患者甲授予 rcvB 的一条，
	// 不能因为患者相同而混入授予 rcv 的授权。
	aRcvB, err := f.s.ListAuthorizations(doc, f.pidA, rcvB.ID)
	if err != nil {
		t.Fatalf("list A/rcvB: %v", err)
	}
	assertAuthIDSet(t, aRcvB, f.otherRcv.ID)

	// 反向核对：查患者乙 + rcv，只能看到乙自己的授权，甲的四条不在其中。
	bRcv, err := f.s.ListAuthorizations(doc, f.pidB, rcv.ID)
	if err != nil {
		t.Fatalf("list B/rcv: %v", err)
	}
	assertAuthIDSet(t, bRcv, f.grantB.ID)
	bAll, err := f.s.ListAuthorizations(doc, f.pidB, "")
	if err != nil {
		t.Fatalf("list patient B: %v", err)
	}
	assertAuthIDSet(t, bAll, f.grantB.ID)

	// 存在但没有授权的患者、已有患者搭配从未获其授权的接收方：成功空列表。
	for _, tc := range []struct{ name, pid, receiver string }{
		{"patient without authorizations", f.pidC, ""},
		{"patient C filtered by receiver", f.pidC, rcv.ID},
		{"patient A filtered by stranger receiver", f.pidA, "rcv-c"},
		{"patient B filtered by receiver it never granted", f.pidB, rcvB.ID},
	} {
		got, err := f.s.ListAuthorizations(doc, tc.pid, tc.receiver)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != 0 {
			t.Fatalf("%s: got %d rows, want empty", tc.name, len(got))
		}
	}

	// 患者不存在：无论是否指定接收方都返回 ErrNotFound。
	if _, err := f.s.ListAuthorizations(doc, "pat_missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing patient err = %v, want ErrNotFound", err)
	}
	if _, err := f.s.ListAuthorizations(doc, "pat_missing", rcv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing patient with receiver err = %v, want ErrNotFound", err)
	}
}

// ---- 四种授权状态都入列；标识、归属、接收方、有效期、撤回时间与范围形态原样保留 ----

func TestListAuthorizationsIncludesEveryLifecycleStateWithSavedValues(t *testing.T) {
	f := setupListAuthFixture(t)
	now := f.clk.t

	rows, err := f.s.ListAuthorizations(doc, f.pidA, rcv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// 生效中、未开始、已到期、已撤回各保留一条；撤回时间有值也不能隐藏该条。
	assertAuthIDSet(t, rows, f.active.ID, f.future.ID, f.expired.ID, f.revoked.ID)

	// 列表用于查看历史，不是接收方当前可用授权合集：逐行核对状态归类。
	active := findListedAuth(t, rows, f.active.ID)
	if !active.ActiveAt(now) || active.RevokedAt != nil {
		t.Fatalf("active grant misclassified: %+v", active)
	}
	future := findListedAuth(t, rows, f.future.ID)
	if future.ActiveAt(now) || future.RevokedAt != nil || !now.Before(future.StartsAt) {
		t.Fatalf("not-started grant misclassified: %+v", future)
	}
	expired := findListedAuth(t, rows, f.expired.ID)
	if expired.ActiveAt(now) || expired.RevokedAt != nil || !expired.ExpiresAt.Before(now) {
		t.Fatalf("expired grant misclassified: %+v", expired)
	}
	revoked := findListedAuth(t, rows, f.revoked.ID)
	if revoked.RevokedAt == nil || revoked.ActiveAt(now) {
		t.Fatalf("revoked grant misclassified: %+v", revoked)
	}

	// 整类范围、限定记录范围、两者并存的混合授权各自保留原本的就诊、类别
	// 与记录选择：不合并成一条，也不把混合范围改成整类范围。
	if len(active.Scopes) != 1 || active.Scopes[0] != (Scope{EncounterID: f.eA1, Category: Diagnosis}) ||
		len(active.Selections) != 0 {
		t.Fatalf("whole-category scope altered: %+v", active)
	}
	if len(future.Scopes) != 0 || len(future.Selections) != 1 ||
		future.Selections[0] != sel(f.eA1, Diagnosis, f.recA1) {
		t.Fatalf("limited scope altered: %+v", future.Selections)
	}
	if len(expired.Scopes) != 1 || expired.Scopes[0] != (Scope{EncounterID: f.eA2, Category: Diagnosis}) ||
		len(expired.Selections) != 0 {
		t.Fatalf("expired whole-category scope altered: %+v", expired)
	}
	if len(revoked.Scopes) != 1 || revoked.Scopes[0] != (Scope{EncounterID: f.eA1, Category: Order}) ||
		len(revoked.Selections) != 1 || revoked.Selections[0] != sel(f.eA1, Diagnosis, f.recA2) {
		t.Fatalf("mixed scope altered on listing: scopes=%+v selections=%+v",
			revoked.Scopes, revoked.Selections)
	}

	// 结果与正式保存的授权逐字段一致：标识、归属、接收方、有效期、撤回时间、
	// 范围、创建时间全部保持已保存的值。
	for _, id := range []ID{f.active.ID, f.future.ID, f.expired.ID, f.revoked.ID} {
		stored, err := f.s.GetAuthorization(doc, f.pidA, id)
		if err != nil {
			t.Fatalf("get stored %q: %v", id, err)
		}
		listed := findListedAuth(t, rows, id)
		if !reflect.DeepEqual(listed, stored) {
			t.Fatalf("listed authorization %q diverges from stored:\nlisted=%+v\nstored=%+v",
				id, listed, stored)
		}
	}

	// 接收方读取临床内容的规则保持原样：只有生效中的整类授权当前可用；
	// 未开始、已到期、已撤回的历史授权不提供任何读取覆盖。
	res, err := f.s.Read(rcv, f.pidA, f.eA1, Diagnosis)
	if err != nil {
		t.Fatalf("active grant should still allow read: %v", err)
	}
	ids := readRecordIDs(res)
	if len(res.Records) != 2 || !ids[f.recA1] || !ids[f.recA2] {
		t.Fatalf("active whole-category read = %+v, want both effective diagnoses", res.Records)
	}
	if _, err := f.s.Read(rcv, f.pidA, f.eA1, Order); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked mixed grant must not cover orders: %v", err)
	}
	if _, err := f.s.Read(rcv, f.pidA, f.eA2, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expired grant must not cover reading: %v", err)
	}
}

// ---- 患者档案停用后内部使用者仍可查看此前建立的授权 ----

func TestListAuthorizationsRemainsAvailableAfterDeactivation(t *testing.T) {
	f := setupListAuthFixture(t)

	if err := f.s.DeactivatePatient(doc, f.pidA); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	auditAfterDeactivate := len(mustAudit(t, f.s, f.pidA))
	auditBBefore := len(mustAudit(t, f.s, f.pidB))
	totalBefore := totalAuthorizations(f.s)

	// 停用不清空、不拒绝：全部五条历史授权仍在，顺序仍按标识升序。
	allA, err := f.s.ListAuthorizations(doc, f.pidA, "")
	if err != nil {
		t.Fatalf("list after deactivation: %v", err)
	}
	assertAuthIDSet(t, allA, f.active.ID, f.future.ID, f.expired.ID, f.revoked.ID, f.otherRcv.ID)
	assertAuthIDsSorted(t, allA)

	// 接收方过滤同样可用；已撤回授权带撤回时间出现。
	aRcv, err := f.s.ListAuthorizations(doc, f.pidA, rcv.ID)
	if err != nil {
		t.Fatalf("filtered list after deactivation: %v", err)
	}
	assertAuthIDSet(t, aRcv, f.active.ID, f.future.ID, f.expired.ID, f.revoked.ID)
	revoked := findListedAuth(t, aRcv, f.revoked.ID)
	stored, err := f.s.GetAuthorization(doc, f.pidA, f.revoked.ID)
	if err != nil {
		t.Fatalf("get revoked: %v", err)
	}
	if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(*stored.RevokedAt) {
		t.Fatalf("revoked-at not preserved: listed=%v stored=%v",
			revoked.RevokedAt, stored.RevokedAt)
	}
	if !reflect.DeepEqual(revoked, stored) {
		t.Fatalf("revoked authorization diverges from stored after deactivation")
	}

	// 另一名患者不受牵连。
	bAll, err := f.s.ListAuthorizations(doc, f.pidB, "")
	if err != nil {
		t.Fatalf("list patient B after A deactivated: %v", err)
	}
	assertAuthIDSet(t, bAll, f.grantB.ID)

	// 停用只影响接收方读取等既有规则，不改变内部历史视图的只读性质。
	if _, err := f.s.Read(rcv, f.pidA, f.eA1, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver read after deactivation err = %v, want ErrAccessDenied", err)
	}

	// 查看列表本身不新增授权或审计。
	if got := len(mustAudit(t, f.s, f.pidA)); got != auditAfterDeactivate {
		t.Fatalf("listing added audit events: %d -> %d", auditAfterDeactivate, got)
	}
	if got := len(mustAudit(t, f.s, f.pidB)); got != auditBBefore {
		t.Fatalf("listing patient B changed its audit: before=%d after=%d",
			auditBBefore, got)
	}
	if totalAuthorizations(f.s) != totalBefore {
		t.Fatalf("listing changed authorization count: before=%d after=%d",
			totalBefore, totalAuthorizations(f.s))
	}
}

// ---- 接收方身份不能使用内部查询；查看列表不新增授权或审计 ----

func TestListAuthorizationsDeniedToReceiversAndCreatesNothing(t *testing.T) {
	f := setupListAuthFixture(t)

	auditABefore := len(mustAudit(t, f.s, f.pidA))
	auditBBefore := len(mustAudit(t, f.s, f.pidB))
	authBefore := totalAuthorizations(f.s)

	// 即使 rcv 对患者甲持有生效中的授权，也不能使用内部查询：
	// ErrAccessDenied，且不带回任何授权资料（即使按自身接收方过滤也不行）。
	if got, err := f.s.ListAuthorizations(rcv, f.pidA, ""); !errors.Is(err, ErrAccessDenied) || len(got) != 0 {
		t.Fatalf("holder receiver list = %d rows, err %v; want 0 rows, ErrAccessDenied", len(got), err)
	}
	if got, err := f.s.ListAuthorizations(rcv, f.pidA, rcv.ID); !errors.Is(err, ErrAccessDenied) || len(got) != 0 {
		t.Fatalf("holder receiver self-filter = %d rows, err %v; want 0 rows, ErrAccessDenied", len(got), err)
	}
	// 无授权的接收方、无效身份同样被拒绝，且不暴露患者是否存在。
	if _, err := f.s.ListAuthorizations(ReceiverActor("rcv-c"), f.pidA, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("stranger receiver err = %v, want ErrAccessDenied", err)
	}
	if _, err := f.s.ListAuthorizations(Actor{ID: "", Kind: "internal"}, f.pidA, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("blank actor id err = %v, want ErrAccessDenied", err)
	}
	if _, err := f.s.ListAuthorizations(Actor{ID: "x", Kind: "other"}, f.pidA, ""); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("unknown actor kind err = %v, want ErrAccessDenied", err)
	}

	// 一批合法查看（含成功空列表与不存在患者的失败）同样不留痕迹。
	if _, err := f.s.ListAuthorizations(doc, f.pidA, ""); err != nil {
		t.Fatalf("list A: %v", err)
	}
	if _, err := f.s.ListAuthorizations(doc, f.pidA, rcv.ID); err != nil {
		t.Fatalf("list A/rcv: %v", err)
	}
	if _, err := f.s.ListAuthorizations(doc, f.pidC, ""); err != nil {
		t.Fatalf("list C: %v", err)
	}
	if _, err := f.s.ListAuthorizations(doc, "pat_missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing patient must stay ErrNotFound")
	}

	if got := len(mustAudit(t, f.s, f.pidA)); got != auditABefore {
		t.Fatalf("listing added audit for A: %d -> %d", auditABefore, got)
	}
	if got := len(mustAudit(t, f.s, f.pidB)); got != auditBBefore {
		t.Fatalf("listing added audit for B: %d -> %d", auditBBefore, got)
	}
	if got := totalAuthorizations(f.s); got != authBefore {
		t.Fatalf("listing changed authorization count: %d -> %d", authBefore, got)
	}
}
