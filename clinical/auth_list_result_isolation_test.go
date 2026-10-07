package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件为 ListAuthorizations 的“返回结果隔离”约定补回归保障：内部使用者
// 通过 ListAuthorizations 拿到患者已经保存的授权后，可以在自己的程序里整理
// 这些结果；但这种本地整理不能成为更换获准记录、改写撤回历史，或恢复已撤回
// 授权的另一条入口。本文件只补测试，不改变授权建立、撤回与接收方读取的行为。
//
// 覆盖的约定：
//   - 同一患者的清单中并存：仍然有效的限定记录授权，以及已经撤回、同时包含
//     整类范围与限定记录范围的授权；
//   - 调用方在手中的结果上改动限定项所指的记录、就诊或类别，或者移除、替换
//     限定项，只改变手中的清单：再次查看对应授权时，原来选定的记录与声明的
//     归属保持原值；原先没有被选中的记录不能因此获得权限，同类别下的其他记录
//     也不能被当成整类范围开放；混合范围中的两类条目各自保留，改动限定项不能
//     连带改变整类范围；
//   - 对已经撤回的授权，直接改动返回的撤回时间、或在本地把撤回标记清空，都不
//     能改写正式保存的首次撤回时间；接收方不能因为手中的清单看起来未撤回就重新
//     读到内容；若已撤回授权是某范围唯一的授权，读取返回 ErrAccessDenied，且
//     不带受保护内容；
//   - 同一条授权先后取得的两份清单，以及不指定接收方与按接收方筛选取得的清单，
//     彼此独立：修改其中一份后，较早拿到的另一份仍保留查询时的记录选择与撤回
//     时间；随后重新查询只反映正式保存的授权；同一份清单中改动一个条目也不能
//     连带其他条目；
//   - 权限结果以“没有其他有效授权补足”为准：仍有效的限定授权继续只允许原来
//     选中的已生效记录，读取结果不夹带被本地替换进去的记录；
//   - 查询与本地整理都不产生新的授权或审计事件。
//
// 预期基线由测试自行深拷贝（刻意不调用生产代码的 cloneAuthorizationValue）：
// 即便生产拷贝退化为浅拷贝（共用范围切片或撤回时间指针），基线仍保持查询时的
// 内容，隔离断言才能真正抓住泄漏。

// ---- 测试夹具 ----
//
// 一名合成患者 pid：
//   - 就诊 e1：已生效诊断 selected（被限定授权选中）、unselected（同类但从未
//     被选中）、replacement（只用于在本地被替换进手中清单，正式从未授权）；
//     已生效医嘱 order1（同类别下唯一一条医嘱，用来核对它不会因限定项被改而
//     被当成整类开放）；
//   - 就诊 e2：已生效诊断 otherEnc（用于把本地限定项改指向其他就诊）。
//
// 两条授权都授予接收方 rcv：
//   - activeSel：仍有效（在夹具时钟的时间窗内），只含限定记录范围，选中 selected；
//   - mixedRevoked：建立后即撤回，混合范围——整类 e1/Order + 限定 e1/Diagnosis
//     的 unselected。它是 e1/Order 的唯一授权，也是 unselected 的唯一授权。
//
// e1/Diagnosis 的读取覆盖里只有 activeSel 明确选中的 selected；没有任何整类
// 授权为它补足，因此 unselected、replacement 读取时都不可见。
type authListIsolationFixture struct {
	s   *Store
	clk *fakeClock

	pid ID
	e1  ID
	e2  ID

	selected    ID // e1 诊断：activeSel 明确选中的已生效记录
	unselected  ID // e1 诊断：同类但从未被任何有效授权选中
	replacement ID // e1 诊断：只用于本地替换，正式从未授权
	otherEnc    ID // e2 诊断：用于把本地限定项改指向其他就诊
	order1      ID // e1 医嘱：mixedRevoked 整类范围下唯一一条，撤回后无授权补足

	start time.Time
	end   time.Time

	activeSel    Authorization // 仍有效：限定 selected
	mixedRevoked Authorization // 已撤回：整类 e1 医嘱 + 限定 e1 诊断 unselected
}

func setupAuthListIsolation(t *testing.T) authListIsolationFixture {
	t.Helper()
	s, clk := newTestStore(t)
	pid, e1 := setupPatientEncounter(t, s)
	e2enc, err := s.AddEncounter(doc, pid, time.Time{})
	if err != nil {
		t.Fatalf("add second encounter: %v", err)
	}
	effRecord := func(eid ID, category, content string) ID {
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
	selected := effRecord(e1, Diagnosis, "限定授权选中的诊断")
	unselected := effRecord(e1, Diagnosis, "同类但未选中的诊断")
	replacement := effRecord(e1, Diagnosis, "仅用于本地替换的诊断")
	otherEnc := effRecord(e2enc.ID, Diagnosis, "其他就诊的诊断")
	order1 := effRecord(e1, Order, "混合授权整类范围下的医嘱")

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(24 * time.Hour)

	activeSel, err := s.GrantSelective(doc, pid, rcv.ID, nil,
		[]RecordSelection{sel(e1, Diagnosis, selected)}, start, end)
	if err != nil {
		t.Fatalf("active selective grant: %v", err)
	}
	mixedRevoked, err := s.GrantSelective(doc, pid, rcv.ID,
		[]Scope{{EncounterID: e1, Category: Order}},
		[]RecordSelection{sel(e1, Diagnosis, unselected)}, start, end)
	if err != nil {
		t.Fatalf("mixed grant: %v", err)
	}
	if err := s.Revoke(doc, pid, mixedRevoked.ID); err != nil {
		t.Fatalf("revoke mixed grant: %v", err)
	}

	return authListIsolationFixture{
		s: s, clk: clk, pid: pid, e1: e1, e2: e2enc.ID,
		selected: selected, unselected: unselected, replacement: replacement,
		otherEnc: otherEnc, order1: order1,
		start: start, end: end,
		activeSel: activeSel, mixedRevoked: mixedRevoked,
	}
}

// ---- 测试自用的独立深拷贝与比对（不经过生产代码的拷贝逻辑） ----

// snapshotAuthorization 独立深拷贝一条授权作为预期基线：整类范围、限定范围
// 与撤回时间都各自复制，绝不与生产返回共用底层数组或指针。
func snapshotAuthorization(a Authorization) Authorization {
	cp := a
	cp.Scopes = append([]Scope(nil), a.Scopes...)
	cp.Selections = append([]RecordSelection(nil), a.Selections...)
	if a.RevokedAt != nil {
		rt := *a.RevokedAt
		cp.RevokedAt = &rt
	}
	return cp
}

// snapshotAuthorizationList 独立深拷贝整份授权清单作为预期基线。
func snapshotAuthorizationList(auths []Authorization) []Authorization {
	out := make([]Authorization, len(auths))
	for i := range auths {
		out[i] = snapshotAuthorization(auths[i])
	}
	return out
}

// assertAuthorizationMatches 逐字段比对一条授权与预期基线，包括整类范围、
// 限定范围与撤回时间的有无及取值。
func assertAuthorizationMatches(t *testing.T, got, want Authorization, label string) {
	t.Helper()
	if got.ID != want.ID || got.PatientID != want.PatientID || got.ReceiverID != want.ReceiverID ||
		!got.StartsAt.Equal(want.StartsAt) || !got.ExpiresAt.Equal(want.ExpiresAt) ||
		!got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("%s: authorization header changed:\n got=%+v\nwant=%+v", label, got, want)
	}
	if !reflect.DeepEqual(got.Scopes, want.Scopes) {
		t.Fatalf("%s: whole-category scopes changed:\n got=%+v\nwant=%+v", label, got.Scopes, want.Scopes)
	}
	if !reflect.DeepEqual(got.Selections, want.Selections) {
		t.Fatalf("%s: limited selections changed:\n got=%+v\nwant=%+v", label, got.Selections, want.Selections)
	}
	if (got.RevokedAt == nil) != (want.RevokedAt == nil) {
		t.Fatalf("%s: revoked-at presence changed: got present=%v, want present=%v",
			label, got.RevokedAt != nil, want.RevokedAt != nil)
	}
	if want.RevokedAt != nil && !got.RevokedAt.Equal(*want.RevokedAt) {
		t.Fatalf("%s: revoked-at changed:\n got=%v\nwant=%v", label, got.RevokedAt, want.RevokedAt)
	}
}

// assertListMatches 按授权标识定位并逐条比对整份清单与基线；条数、标识集合
// 与排序都必须一致。
func assertListMatches(t *testing.T, got, want []Authorization, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: authorization count changed: got %d, want %d", label, len(got), len(want))
	}
	wantByID := make(map[ID]Authorization, len(want))
	for _, a := range want {
		wantByID[a.ID] = a
	}
	for i, g := range got {
		w, ok := wantByID[g.ID]
		if !ok {
			t.Fatalf("%s: unexpected authorization %q at index %d", label, g.ID, i)
		}
		assertAuthorizationMatches(t, g, w, label+": authorization "+g.ID)
	}
}

// listedAuthIndex 返回某授权在清单中的下标；不在其中即致命失败。
func listedAuthIndex(t *testing.T, auths []Authorization, id ID) int {
	t.Helper()
	for i := range auths {
		if auths[i].ID == id {
			return i
		}
	}
	t.Fatalf("authorization %q missing from list", id)
	return -1
}

// ---- 形态：仍有效的限定授权与已撤回的混合授权都在清单中，并保留保存值 ----

func TestAuthListIsolationFixtureShape(t *testing.T) {
	f := setupAuthListIsolation(t)
	now := f.clk.t

	rows, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	assertAuthIDSet(t, rows, f.activeSel.ID, f.mixedRevoked.ID)

	active := findListedAuth(t, rows, f.activeSel.ID)
	if !active.ActiveAt(now) || active.RevokedAt != nil {
		t.Fatalf("active selective grant misclassified: %+v", active)
	}
	if len(active.Scopes) != 0 || !reflect.DeepEqual(active.Selections,
		[]RecordSelection{sel(f.e1, Diagnosis, f.selected)}) {
		t.Fatalf("active selective scope altered: scopes=%+v selections=%+v",
			active.Scopes, active.Selections)
	}

	revoked := findListedAuth(t, rows, f.mixedRevoked.ID)
	if revoked.RevokedAt == nil || revoked.ActiveAt(now) {
		t.Fatalf("revoked mixed grant misclassified: %+v", revoked)
	}
	if !reflect.DeepEqual(revoked.Scopes, []Scope{{EncounterID: f.e1, Category: Order}}) ||
		!reflect.DeepEqual(revoked.Selections, []RecordSelection{sel(f.e1, Diagnosis, f.unselected)}) {
		t.Fatalf("mixed scope altered on listing: scopes=%+v selections=%+v",
			revoked.Scopes, revoked.Selections)
	}

	// 基线读取：仍有效的限定授权只放行 selected；已撤回混合授权覆盖的
	// unselected 与 order1 都不可见，replacement、其他就诊同理。
	res, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("active selective grant should allow read: %v", err)
	}
	if ids := readRecordIDs(res); len(ids) != 1 || !ids[f.selected] {
		t.Fatalf("diagnosis read = %+v, want only the selected record", res.Records)
	}
	if _, err := f.s.Read(rcv, f.pid, f.e1, Order); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked mixed grant was the only order grant: %v", err)
	}
	if _, err := f.s.Read(rcv, f.pid, f.e2, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("other encounter must stay uncovered: %v", err)
	}
}

// ---- 改动手中清单的限定项（记录/就诊/类别）：只改变手中的清单 ----

func TestListAuthorizationsTamperedLimitedSelectionsDoNotChangeStore(t *testing.T) {
	f := setupAuthListIsolation(t)

	rows, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := snapshotAuthorizationList(rows)
	auditBefore := len(mustAudit(t, f.s, f.pid))
	authTotalBefore := totalAuthorizations(f.s)

	// 在手中结果上对仍有效限定授权的限定项做尽可能广泛的改动：
	// 改记录标识、改声明就诊、改声明类别。
	ai := listedAuthIndex(t, rows, f.activeSel.ID)
	rows[ai].Selections[0].RecordID = f.replacement
	rows[ai].Selections[0].EncounterID = f.e2
	rows[ai].Selections[0].Category = Order

	// 对已撤回混合授权的限定项也做同样改动：限定项变化不能连带整类范围。
	ri := listedAuthIndex(t, rows, f.mixedRevoked.ID)
	rows[ri].Selections[0].RecordID = f.replacement
	rows[ri].Selections[0].EncounterID = f.e2
	rows[ri].Selections[0].Category = Order

	// 再次查看：两条正式授权的记录选择、声明归属与整类范围全部保持原值。
	storedActive, err := f.s.GetAuthorization(doc, f.pid, f.activeSel.ID)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	assertAuthorizationMatches(t, storedActive, want[ai], "GetAuthorization active after selections tampered")
	storedRevoked, err := f.s.GetAuthorization(doc, f.pid, f.mixedRevoked.ID)
	if err != nil {
		t.Fatalf("get revoked: %v", err)
	}
	assertAuthorizationMatches(t, storedRevoked, want[ri], "GetAuthorization revoked after selections tampered")

	// 重新取得的清单同样只反映正式保存的授权。
	again, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil {
		t.Fatalf("re-list: %v", err)
	}
	assertListMatches(t, again, want, "re-list after selections tampered")

	// 读取不夹带被本地替换进去的记录，也不把同类别其他记录当成整类开放：
	// e1 诊断仍只有 selected；本地改成指向 e2/Order 不构成任何新授权。
	res, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("read after local selection tamper: %v", err)
	}
	if ids := readRecordIDs(res); len(ids) != 1 || !ids[f.selected] ||
		ids[f.replacement] || ids[f.unselected] {
		t.Fatalf("diagnosis read leaked locally replaced records: %+v", res.Records)
	}
	if _, err := f.s.Read(rcv, f.pid, f.e2, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("locally pointed encounter gained access: %v", err)
	}
	// 已撤回混合授权是 e1 医嘱的唯一授权：本地改动它的限定项不能恢复整类医嘱。
	if res, err := f.s.Read(rcv, f.pid, f.e1, Order); !errors.Is(err, ErrAccessDenied) ||
		len(res.Records) != 0 || res.EncounterID != "" {
		t.Fatalf("revoked order scope must stay denied with blank result: res=%+v err=%v", res, err)
	}

	// 查询与本地整理都不产生新的授权或审计事件。
	if got := totalAuthorizations(f.s); got != authTotalBefore {
		t.Fatalf("local tamper changed authorization count: before=%d after=%d", authTotalBefore, got)
	}
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("local tamper changed audit count: before=%d after=%d", auditBefore, got)
	}
}

// ---- 移除、替换限定项，以及改动同一条混合授权的整类项：互不连带 ----

func TestListAuthorizationsRemovedOrReplacedSelectionsDoNotChangeStore(t *testing.T) {
	f := setupAuthListIsolation(t)

	rows, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := snapshotAuthorizationList(rows)
	ai := listedAuthIndex(t, rows, f.activeSel.ID)
	ri := listedAuthIndex(t, rows, f.mixedRevoked.ID)

	// 移除仍有效授权的限定项（本地清空）；把已撤回授权的限定项替换成另一条记录。
	rows[ai].Selections = nil
	rows[ri].Selections = []RecordSelection{sel(f.e1, Diagnosis, f.replacement)}
	// 顺手在本地改动混合授权的整类范围：两类条目各自保留，改一边不能连带另一边，
	// 本地改动同样不入库。
	rows[ri].Scopes[0] = Scope{EncounterID: f.e2, Category: Diagnosis}

	storedActive, err := f.s.GetAuthorization(doc, f.pid, f.activeSel.ID)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	assertAuthorizationMatches(t, storedActive, want[ai], "active grant after selections removed locally")
	storedRevoked, err := f.s.GetAuthorization(doc, f.pid, f.mixedRevoked.ID)
	if err != nil {
		t.Fatalf("get revoked: %v", err)
	}
	assertAuthorizationMatches(t, storedRevoked, want[ri], "revoked grant after selections/scopes replaced locally")

	again, err := f.s.ListAuthorizations(doc, f.pid, "")
	if err != nil {
		t.Fatalf("re-list without receiver: %v", err)
	}
	assertListMatches(t, again, want, "unfiltered re-list after local removal/replacement")

	// 本地清空限定项不能让 selected 失去授权；本地替换不能让 replacement 获得授权。
	res, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("read after local removal/replacement: %v", err)
	}
	if ids := readRecordIDs(res); len(ids) != 1 || !ids[f.selected] || ids[f.replacement] {
		t.Fatalf("diagnosis read changed via local removal/replacement: %+v", res.Records)
	}
	// 本地把整类项改到 e2 诊断，不能让 e2 获得授权；原本撤回的 e1 医嘱仍被拒。
	if _, err := f.s.Read(rcv, f.pid, f.e2, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("locally moved whole scope granted another encounter: %v", err)
	}
	if _, err := f.s.Read(rcv, f.pid, f.e1, Order); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked order scope must stay denied: %v", err)
	}
}

// ---- 改动手中结果的撤回时间或本地清空撤回标记：首次撤回时间与拒绝访问不变 ----

func TestListAuthorizationsTamperedRevokedAtDoesNotReviveAccess(t *testing.T) {
	f := setupAuthListIsolation(t)

	// 先取回正式保存的首次撤回时间作为核对依据。
	stored, err := f.s.GetAuthorization(doc, f.pid, f.mixedRevoked.ID)
	if err != nil {
		t.Fatalf("get stored revoked: %v", err)
	}
	if stored.RevokedAt == nil {
		t.Fatal("mixed grant should be revoked")
	}
	savedRevokedAt := *stored.RevokedAt

	rows, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := snapshotAuthorizationList(rows)
	ri := listedAuthIndex(t, rows, f.mixedRevoked.ID)
	auditBefore := len(mustAudit(t, f.s, f.pid))

	// 直接改动返回结果中的撤回时间，再在本地把撤回标记清空。
	forged := savedRevokedAt.Add(48 * time.Hour)
	*rows[ri].RevokedAt = forged
	rows[ri].RevokedAt = nil

	// 手中清单此时“看起来未撤回”，但较早取得的另一份结果与正式授权都不受影响。
	fresh, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil {
		t.Fatalf("re-list: %v", err)
	}
	assertListMatches(t, fresh, want, "re-list after revoked-at locally cleared")
	reGet, err := f.s.GetAuthorization(doc, f.pid, f.mixedRevoked.ID)
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if reGet.RevokedAt == nil || !reGet.RevokedAt.Equal(savedRevokedAt) {
		t.Fatalf("official first revoked-at rewritten via listed value: %v, want %v",
			reGet.RevokedAt, savedRevokedAt)
	}

	// 接收方不能因为手里的清单看起来未撤回就重新读到内容：已撤回的混合授权是
	// e1 医嘱的唯一授权，必须 ErrAccessDenied 且结果不带受保护内容。
	res, err := f.s.Read(rcv, f.pid, f.e1, Order)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("read after local clear of revoked-at err = %v, want ErrAccessDenied", err)
	}
	if !reflect.DeepEqual(res, ReadResult{}) {
		t.Fatalf("denied read must carry no protected content: %+v", res)
	}
	// 该混合授权的限定项同样不恢复：unselected 仍读不到（诊断里只有 selected）。
	diag, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("diagnosis read: %v", err)
	}
	if ids := readRecordIDs(diag); len(ids) != 1 || !ids[f.selected] || ids[f.unselected] {
		t.Fatalf("revoked selection must not revive via local list: %+v", diag.Records)
	}

	// 本地整理不产生新的授权或审计事件。
	if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
		t.Fatalf("local revoked-at tamper changed audit count: before=%d after=%d", auditBefore, got)
	}
}

// ---- 两份清单（先后取得 / 是否按接收方筛选）互不影响；同清单条目互不连带 ----

func TestListAuthorizationsCopiesAreIndependentAcrossQueries(t *testing.T) {
	f := setupAuthListIsolation(t)

	// 同一条授权先后取得的两份按接收方筛选的清单。
	list1, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil {
		t.Fatalf("list filtered 1: %v", err)
	}
	list2, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil {
		t.Fatalf("list filtered 2: %v", err)
	}
	// 不指定接收方取得的清单（夹具中只有 rcv 一名接收方，条数相同，独立切片）。
	unfiltered, err := f.s.ListAuthorizations(doc, f.pid, "")
	if err != nil {
		t.Fatalf("list unfiltered: %v", err)
	}
	want := snapshotAuthorizationList(list1)
	if len(want) != 2 || len(list2) != 2 || len(unfiltered) != 2 {
		t.Fatalf("each list should hold 2 authorizations: %d/%d/%d",
			len(list1), len(list2), len(unfiltered))
	}

	// 篡改第一份清单的两个条目：限定项、整类项、撤回时间全部改/清空，
	// 并丢掉第二个元素。
	a1 := listedAuthIndex(t, list1, f.activeSel.ID)
	r1 := listedAuthIndex(t, list1, f.mixedRevoked.ID)
	list1[a1].Selections[0] = sel(f.e2, Order, f.replacement)
	list1[r1].Selections[0] = sel(f.e2, Diagnosis, f.otherEnc)
	list1[r1].Scopes[0] = Scope{EncounterID: f.e2, Category: Order}
	*list1[r1].RevokedAt = list1[r1].RevokedAt.Add(99 * time.Hour)
	list1[r1].RevokedAt = nil
	list1 = list1[:1]

	// 较早拿到的第二份清单完整保留查询时的记录选择与撤回时间，条目互不连带。
	assertListMatches(t, list2, want, "earlier filtered list after other list tampered")

	// 不指定接收方取得的清单同样不受按接收方筛选清单被篡改的影响。
	assertListMatches(t, unfiltered, want, "unfiltered list after filtered list tampered")

	// 反向：篡改不指定接收方清单的一个条目，按接收方筛选的清单仍保持原值。
	ur := listedAuthIndex(t, unfiltered, f.mixedRevoked.ID)
	unfiltered[ur].Selections = nil
	unfiltered[ur].Scopes = nil
	unfiltered[ur].RevokedAt = nil
	assertListMatches(t, list2, want, "filtered list after unfiltered list tampered")

	// 随后重新查询（两种过滤都查）只反映正式保存的授权。
	againFiltered, err := f.s.ListAuthorizations(doc, f.pid, rcv.ID)
	if err != nil {
		t.Fatalf("re-list filtered: %v", err)
	}
	assertListMatches(t, againFiltered, want, "fresh filtered list after local tampering")
	againAll, err := f.s.ListAuthorizations(doc, f.pid, "")
	if err != nil {
		t.Fatalf("re-list unfiltered: %v", err)
	}
	assertListMatches(t, againAll, want, "fresh unfiltered list after local tampering")

	// 权限结果始终以正式保存的授权为准：仍有效的限定授权只放行 selected，
	// 已撤回授权覆盖的范围仍 ErrAccessDenied。
	res, err := f.s.Read(rcv, f.pid, f.e1, Diagnosis)
	if err != nil {
		t.Fatalf("read after cross-list tampering: %v", err)
	}
	if ids := readRecordIDs(res); len(ids) != 1 || !ids[f.selected] {
		t.Fatalf("diagnosis read changed via cross-list tampering: %+v", res.Records)
	}
	if _, err := f.s.Read(rcv, f.pid, f.e1, Order); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("revoked-only order scope must stay denied: %v", err)
	}
}

// ---- 本地篡改后经历一次真实原子写盘并重开：正式授权仍保持原值 ----

func TestListAuthorizationsLocalTamperingDoesNotPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "清单隔离重开患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	mkEff := func(category, content string) ID {
		t.Helper()
		r, err := s.CreateDraft(doc, p.ID, e.ID, category, content)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ActivateRecord(doc, r.ID); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	kept := mkEff(Diagnosis, "正式选中的诊断")
	sibling := mkEff(Diagnosis, "同类其他诊断")
	ord := mkEff(Order, "整类医嘱")
	_ = sibling

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(48 * time.Hour)
	active, err := s.GrantSelective(doc, p.ID, rcv.ID, nil,
		[]RecordSelection{sel(e.ID, Diagnosis, kept)}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	mixed, err := s.GrantSelective(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Order}},
		[]RecordSelection{sel(e.ID, Diagnosis, sibling)}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(doc, p.ID, mixed.ID); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListAuthorizations(doc, p.ID, rcv.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := snapshotAuthorizationList(rows)
	savedRevoked := *findListedAuth(t, want, mixed.ID).RevokedAt

	// 取得清单并在本地全面篡改：限定项换记录/就诊/类别、整类项改写、撤回时间清空。
	ai := listedAuthIndex(t, rows, active.ID)
	ri := listedAuthIndex(t, rows, mixed.ID)
	rows[ai].Selections[0] = sel(e.ID, Order, ord)
	rows[ri].Selections = nil
	rows[ri].Scopes[0] = Scope{EncounterID: "enc_local", Category: Diagnosis}
	rows[ri].RevokedAt = nil

	// 触发一次真实的整体原子写盘（合法更正），本地篡改不应借写盘进入快照。
	if _, err := s.CorrectRecord(doc, kept, 1, "正式选中的诊断-更正", "触发一次真实落盘"); err != nil {
		t.Fatalf("correction that forces persist: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })

	reopened, err := s2.ListAuthorizations(doc, p.ID, rcv.ID)
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	assertListMatches(t, reopened, want, "authorization list after reopen")
	gotMixed := findListedAuth(t, reopened, mixed.ID)
	if gotMixed.RevokedAt == nil || !gotMixed.RevokedAt.Equal(savedRevoked) {
		t.Fatalf("first revoked-at lost across reopen: got %v, want %v",
			gotMixed.RevokedAt, savedRevoked)
	}

	// 读取权限重开后仍按正式授权：只放行 selected（更正后的当前版本），
	// 被本地换进去的医嘱不出现；撤回的整类医嘱仍被拒且不带内容。
	res, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("diagnosis read after reopen: %v", err)
	}
	if ids := readRecordIDs(res); len(ids) != 1 || !ids[kept] {
		t.Fatalf("diagnosis read after reopen leaked local replacement: %+v", res.Records)
	}
	if denied, err := s2.Read(rcv, p.ID, e.ID, Order); !errors.Is(err, ErrAccessDenied) ||
		!reflect.DeepEqual(denied, ReadResult{}) {
		t.Fatalf("revoked order scope after reopen: res=%+v err=%v", denied, err)
	}

	// 审计仍只有建立/撤回等真实事件，本地整理没有留下痕迹。
	acts := auditActions(mustAudit(t, s2, p.ID))
	if acts[ActionGranted] != 2 || acts[ActionRevoked] != 1 {
		t.Fatalf("audit counts after reopen wrong: %+v", acts)
	}
}
