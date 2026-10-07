package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// ---- ListAuthorizations 返回结果与正式授权隔离的回归夹具 ----
//
// 同一患者名下只有两条授权，且互不补足对方的范围：
//   - limited：仍有效的限定记录授权，仅明确选中 e 下诊断 dx1；
//   - mixed：整类范围（e 下医嘱）与限定记录范围（e 下诊断 dx2）并存，
//     按参数选择在建立后立即撤回。
//
// e 下另有未被任何有效授权选中的诊断 dx3，以及医嘱 or1、or2：撤回 mixed
// 后该就诊的医嘱类别不存在任何其他授权，读取必须 ErrAccessDenied。
type listMutationFixture struct {
	s   *Store
	clk *fakeClock

	particle ID
	e        ID // 主就诊
	eOther   ID // 属于同一患者的另一就诊，用于本地改声明就诊的尝试

	dx1 ID // e 下已生效诊断，被 limited 选中
	dx2 ID // e 下已生效诊断，被 mixed 的限定项选中（mixed 已撤回）
	dx3 ID // e 下已生效诊断，从未被选中
	or1 ID // e 下已生效医嘱，仅 mixed 的整类范围曾覆盖
	or2 ID // e 下已生效医嘱，同上

	limited Authorization // 生效中：仅限定 dx1
	mixed   Authorization // 已撤回（按参数）：整类 e/医嘱 + 限定 e/诊断 dx2
}

func setupListMutationFixture(t *testing.T, revokeMixed bool) listMutationFixture {
	t.Helper()
	s, clk := newTestStore(t)

	p, err := s.RegisterPatient(doc, "列表篡改隔离患者")
	if err != nil {
		t.Fatalf("register patient: %v", err)
	}
	addEnc := func() ID {
		t.Helper()
		e, err := s.AddEncounter(doc, p.ID, time.Time{})
		if err != nil {
			t.Fatalf("add encounter: %v", err)
		}
		return e.ID
	}
	eff := func(eid ID, category, content string) ID {
		t.Helper()
		r, err := s.CreateDraft(doc, p.ID, eid, category, content)
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if _, err := s.ActivateRecord(doc, r.ID); err != nil {
			t.Fatalf("activate: %v", err)
		}
		return r.ID
	}

	e := addEnc()
	eOther := addEnc()
	dx1 := eff(e, Diagnosis, "诊断一")
	dx2 := eff(e, Diagnosis, "诊断二")
	dx3 := eff(e, Diagnosis, "诊断三")
	or1 := eff(e, Order, "医嘱一")
	or2 := eff(e, Order, "医嘱二")

	now := clk.t
	st, en := now.Add(-time.Hour), now.Add(24*time.Hour)

	limited, err := s.GrantSelective(doc, p.ID, rcv.ID, nil,
		[]RecordSelection{sel(e, Diagnosis, dx1)}, st, en)
	if err != nil {
		t.Fatalf("limited grant: %v", err)
	}
	mixed, err := s.GrantSelective(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e, Category: Order}},
		[]RecordSelection{sel(e, Diagnosis, dx2)}, st, en)
	if err != nil {
		t.Fatalf("mixed grant: %v", err)
	}
	if revokeMixed {
		if err := s.Revoke(doc, p.ID, mixed.ID); err != nil {
			t.Fatalf("revoke mixed: %v", err)
		}
		mixed, err = s.GetAuthorization(doc, p.ID, mixed.ID)
		if err != nil {
			t.Fatalf("reload mixed: %v", err)
		}
	}

	return listMutationFixture{
		s: s, clk: clk,
		particle: p.ID, e: e, eOther: eOther,
		dx1: dx1, dx2: dx2, dx3: dx3, or1: or1, or2: or2,
		limited: limited, mixed: mixed,
	}
}

// listedAuthPtr 返回清单中某条授权所在切片元素的指针，使本地整理直接作用
// 在返回结果本身（替换限定切片、清空撤回标记等），而不是只改一个局部副本。
func listedAuthPtr(t *testing.T, auths []Authorization, id ID) *Authorization {
	t.Helper()
	for i := range auths {
		if auths[i].ID == id {
			return &auths[i]
		}
	}
	t.Fatalf("authorization %q missing from list", id)
	return nil
}

// assertDeniedWithoutContent 要求 ErrAccessDenied，且返回值不带任何受保护
// 内容（就诊、类别、记录都必须是零值）——手里的清单被改成“看起来未撤回”
// 不能让接收方借读取拿到内容，也不能从拒绝结果里夹带记录标识或数量。
func assertDeniedWithoutContent(t *testing.T, res ReadResult, err error, where string) {
	t.Helper()
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("%s: err = %v, want ErrAccessDenied", where, err)
	}
	if res.EncounterID != "" || res.Category != "" || len(res.Records) != 0 {
		t.Fatalf("%s: denial leaked protected content: %+v", where, res)
	}
}

// 篡改手中清单的限定项：改记录、改就诊、改类别、移除、整项替换，
// 都只改变调用方手中的那一份；正式授权的记录选择与声明归属保持原值，
// 读取权限不被本地替换进去的记录扩大，混合范围中的整类条目不被连带改变。

func TestListAuthorizationsLocalSelectionTamperingDoesNotChangeSavedGrant(t *testing.T) {
	f := setupListMutationFixture(t, true)
	pid := f.particle

	auditBefore := len(mustAudit(t, f.s, pid))
	totalBefore := totalAuthorizations(f.s)

	// 不指定接收方与按接收方筛选各取一份，两份都用于本地整理。
	rowsAll, err := f.s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	rowsRcv, err := f.s.ListAuthorizations(doc, pid, rcv.ID)
	if err != nil {
		t.Fatalf("list by receiver: %v", err)
	}

	// 1) 直接在返回结果上改限定项所指的记录、就诊与类别。
	limited := listedAuthPtr(t, rowsAll, f.limited.ID)
	limited.Selections[0].RecordID = f.dx3
	limited.Selections[0].EncounterID = f.eOther
	limited.Selections[0].Category = Order

	// 2) 在另一份结果上移除限定项（空选择不代表整类授权，也不能把授权改成整类）。
	listedAuthPtr(t, rowsRcv, f.limited.ID).Selections = nil

	// 3) 第三份结果上整项替换成从未被选中的记录，并向整类范围追加条目。
	rowsAll2, err := f.s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatalf("list all again: %v", err)
	}
	limited3 := listedAuthPtr(t, rowsAll2, f.limited.ID)
	limited3.Selections = []RecordSelection{sel(f.e, Diagnosis, f.dx2)}
	limited3.Scopes = append(limited3.Scopes, Scope{EncounterID: f.e, Category: Order})

	// 4) 已撤回的混合授权：只改限定项，整类条目必须原样保留。
	mixed := listedAuthPtr(t, rowsRcv, f.mixed.ID)
	mixed.Selections[0].RecordID = f.dx3
	mixed.Selections[0].EncounterID = f.eOther
	mixed.Selections = nil

	// 正式保存的授权逐字段保持建立/撤回时的值。
	storedLimited, err := f.s.GetAuthorization(doc, pid, f.limited.ID)
	if err != nil {
		t.Fatalf("get limited: %v", err)
	}
	if !reflect.DeepEqual(storedLimited, f.limited) {
		t.Fatalf("saved limited grant changed through local tampering:\nsaved = %+v\nwant  = %+v",
			storedLimited, f.limited)
	}
	storedMixed, err := f.s.GetAuthorization(doc, pid, f.mixed.ID)
	if err != nil {
		t.Fatalf("get mixed: %v", err)
	}
	if !reflect.DeepEqual(storedMixed, f.mixed) {
		t.Fatalf("saved mixed grant changed through local tampering:\nsaved = %+v\nwant  = %+v",
			storedMixed, f.mixed)
	}
	// 明确再点一次两类条目各自保留：限定项仍指向 dx2，整类仍是 e/医嘱。
	if len(storedMixed.Selections) != 1 || storedMixed.Selections[0] != sel(f.e, Diagnosis, f.dx2) {
		t.Fatalf("mixed limited entry lost or altered: %+v", storedMixed.Selections)
	}
	if len(storedMixed.Scopes) != 1 || storedMixed.Scopes[0] != (Scope{EncounterID: f.e, Category: Order}) {
		t.Fatalf("mixed whole-category entry altered via limited-entry tampering: %+v", storedMixed.Scopes)
	}

	// 重新查询只反映正式保存的授权，不反映任何一份本地整理结果。
	refreshed, err := f.s.ListAuthorizations(doc, pid, rcv.ID)
	if err != nil {
		t.Fatalf("relist: %v", err)
	}
	assertAuthIDSet(t, refreshed, f.limited.ID, f.mixed.ID)
	for _, want := range []Authorization{f.limited, f.mixed} {
		if got := findListedAuth(t, refreshed, want.ID); !reflect.DeepEqual(got, want) {
			t.Fatalf("relisted %q reflects local edits:\ngot  = %+v\nwant = %+v", want.ID, got, want)
		}
	}

	// 权限结果以“没有其他有效授权补足”为准：生效的限定授权仍只允许 dx1，
	// 本地替换进去的 dx2/dx3、追加的整类医嘱范围都不产生任何读取覆盖。
	res, err := f.s.Read(rcv, pid, f.e, Diagnosis)
	if err != nil {
		t.Fatalf("limited grant should still allow reading dx1: %v", err)
	}
	ids := readRecordIDs(res)
	if len(res.Records) != 1 || !ids[f.dx1] || ids[f.dx2] || ids[f.dx3] {
		t.Fatalf("diagnosis read = %v, want only originally selected dx1", ids)
	}
	// 已撤回的混合授权是医嘱类别的唯一授权：拒绝且不带受保护内容，
	// 不能因为本地清单看起来仍覆盖医嘱而读到 or1/or2。
	denied, err := f.s.Read(rcv, pid, f.e, Order)
	assertDeniedWithoutContent(t, denied, err, "orders after revoked-only grant tampered")

	// 查询与本地整理不产生新的授权或审计事件。
	if got := totalAuthorizations(f.s); got != totalBefore {
		t.Fatalf("authorization count changed: %d -> %d", totalBefore, got)
	}
	if got := len(mustAudit(t, f.s, pid)); got != auditBefore {
		t.Fatalf("listing/local tampering changed audit: %d -> %d", auditBefore, got)
	}
}

// 篡改手中清单的撤回时间：直接改写时间、本地清空撤回标记，
// 都不影响正式授权保留的首次撤回时间；接收方不能据此重新读到内容。

func TestListAuthorizationsLocalRevocationTamperingDoesNotReviveGrant(t *testing.T) {
	f := setupListMutationFixture(t, true)
	pid := f.particle
	firstRevokedAt := *f.mixed.RevokedAt

	auditBefore := len(mustAudit(t, f.s, pid))
	totalBefore := totalAuthorizations(f.s)

	// 1) 直接改写返回结果上撤回时间“所指的时刻”（不只是给字段换一个指针），
	// 用以验证撤回时间与库内对象是深拷贝隔离的。
	rowsA, err := f.s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	fakeTime := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	*listedAuthPtr(t, rowsA, f.mixed.ID).RevokedAt = fakeTime

	// 2) 在另一份本地结果上把撤回标记清空，伪装成从未撤回。
	rowsB, err := f.s.ListAuthorizations(doc, pid, rcv.ID)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	listedAuthPtr(t, rowsB, f.mixed.ID).RevokedAt = nil

	// 本地两份各自呈现篡改后的样子，互不影响。
	if got := findListedAuth(t, rowsA, f.mixed.ID).RevokedAt; got == nil || !got.Equal(fakeTime) {
		t.Fatalf("local copy A revoke time = %v, want local fake %v", got, fakeTime)
	}
	if got := findListedAuth(t, rowsB, f.mixed.ID).RevokedAt; got != nil {
		t.Fatalf("local copy B revoke marker = %v, want cleared locally", got)
	}

	// 正式授权仍保留首次撤回时间。
	stored, err := f.s.GetAuthorization(doc, pid, f.mixed.ID)
	if err != nil {
		t.Fatalf("get mixed: %v", err)
	}
	if stored.RevokedAt == nil || !stored.RevokedAt.Equal(firstRevokedAt) {
		t.Fatalf("stored revoke time = %v, want first revoke time %v",
			stored.RevokedAt, firstRevokedAt)
	}

	// 时钟前进后重复撤回仍是幂等确认：不能把首次撤回时间改写成本次时刻。
	f.clk.t = f.clk.t.Add(2 * time.Hour)
	if err := f.s.Revoke(doc, pid, f.mixed.ID); err != nil {
		t.Fatalf("idempotent re-revoke: %v", err)
	}
	stored, err = f.s.GetAuthorization(doc, pid, f.mixed.ID)
	if err != nil {
		t.Fatalf("re-get mixed: %v", err)
	}
	if stored.RevokedAt == nil || !stored.RevokedAt.Equal(firstRevokedAt) {
		t.Fatalf("first revoke time overwritten: got %v, want %v",
			stored.RevokedAt, firstRevokedAt)
	}

	// 手中清单看起来未撤回，接收方仍读不到内容：医嘱唯一授权已撤回，拒绝且无内容。
	denied, err := f.s.Read(rcv, pid, f.e, Order)
	assertDeniedWithoutContent(t, denied, err, "orders with locally un-revoked list")
	// 诊断类别不受本地清空撤回影响：仍只有 dx1，dx2 不随“复活”的混合授权回来。
	res, err := f.s.Read(rcv, pid, f.e, Diagnosis)
	if err != nil {
		t.Fatalf("dx1 read: %v", err)
	}
	if ids := readRecordIDs(res); len(res.Records) != 1 || !ids[f.dx1] || ids[f.dx2] {
		t.Fatalf("diagnosis read after local un-revoke = %v, want only dx1", ids)
	}

	// 重新查询仍只反映正式保存的撤回状态与首次撤回时间。
	refreshed, err := f.s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatalf("relist: %v", err)
	}
	relisted := findListedAuth(t, refreshed, f.mixed.ID)
	if relisted.RevokedAt == nil || !relisted.RevokedAt.Equal(firstRevokedAt) {
		t.Fatalf("relisted revoke time = %v, want %v", relisted.RevokedAt, firstRevokedAt)
	}
	if !reflect.DeepEqual(relisted, stored) {
		t.Fatalf("relisted mixed grant diverges from stored:\n%+v\n%+v", relisted, stored)
	}

	if got := totalAuthorizations(f.s); got != totalBefore {
		t.Fatalf("authorization count changed: %d -> %d", totalBefore, got)
	}
	// 两次查询 + 本地篡改不新增审计；幂等重复撤回同样不新增事件。
	if got := len(mustAudit(t, f.s, pid)); got != auditBefore {
		t.Fatalf("audit event count changed: %d -> %d", auditBefore, got)
	}
}

// 同一条授权先后取得的两份清单、不指定接收方与按接收方筛选的清单互不影响：
// 篡改其中一份，较早拿到的另一份保留查询时的记录选择与撤回时间，
// 重新查询只反映正式保存的授权。

func TestListAuthorizationsSnapshotsIndependentAcrossQueries(t *testing.T) {
	// 混合授权先不撤回：早一批清单拿到的是“未撤回”的查询时刻状态。
	f := setupListMutationFixture(t, false)
	pid := f.particle

	beforeAll, err := f.s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatalf("before list all: %v", err)
	}
	beforeRcv, err := f.s.ListAuthorizations(doc, pid, rcv.ID)
	if err != nil {
		t.Fatalf("before list receiver: %v", err)
	}
	if findListedAuth(t, beforeAll, f.mixed.ID).RevokedAt != nil {
		t.Fatalf("pre-revoke snapshot already shows revocation")
	}

	if err := f.s.Revoke(doc, pid, f.mixed.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	nowRevoked, err := f.s.GetAuthorization(doc, pid, f.mixed.ID)
	if err != nil {
		t.Fatalf("get revoked: %v", err)
	}

	afterAll, err := f.s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatalf("after list all: %v", err)
	}
	afterRcv, err := f.s.ListAuthorizations(doc, pid, rcv.ID)
	if err != nil {
		t.Fatalf("after list receiver: %v", err)
	}

	// 先篡改 afterAll：换掉限定授权的记录选择、清空混合授权的撤回时间。
	tamperedAll := listedAuthPtr(t, afterAll, f.limited.ID)
	tamperedAll.Selections[0].RecordID = f.dx3
	listedAuthPtr(t, afterAll, f.mixed.ID).RevokedAt = nil

	// afterRcv 必须保持撤回后的正式样子，不受 afterAll 篡改影响。
	rLimited := findListedAuth(t, afterRcv, f.limited.ID)
	if len(rLimited.Selections) != 1 || rLimited.Selections[0] != sel(f.e, Diagnosis, f.dx1) {
		t.Fatalf("afterRcv limited selection leaked from afterAll tamper: %+v", rLimited.Selections)
	}
	rMixed := findListedAuth(t, afterRcv, f.mixed.ID)
	if rMixed.RevokedAt == nil || !rMixed.RevokedAt.Equal(*nowRevoked.RevokedAt) {
		t.Fatalf("afterRcv revoke marker leaked from afterAll tamper: %v", rMixed.RevokedAt)
	}

	// 再篡改 afterRcv：移除限定项、改掉混合授权限定项指向的记录。
	listedAuthPtr(t, afterRcv, f.limited.ID).Selections = nil
	listedAuthPtr(t, afterRcv, f.mixed.ID).Selections[0].RecordID = f.dx3

	// afterAll 只呈现它自己被改的样子，afterRcv 的篡改不回串：
	// 它的混合授权撤回时间仍为本地清空的 nil，但限定项仍指向 dx2；
	// limited 的记录仍是它自己替换的 dx3。
	aMixed := findListedAuth(t, afterAll, f.mixed.ID)
	if aMixed.RevokedAt != nil {
		t.Fatalf("afterAll local revoke-clear lost: %v", aMixed.RevokedAt)
	}
	if len(aMixed.Selections) != 1 || aMixed.Selections[0] != sel(f.e, Diagnosis, f.dx2) {
		t.Fatalf("afterAll mixed selection leaked from afterRcv tamper: %+v", aMixed.Selections)
	}
	if got := findListedAuth(t, afterAll, f.limited.ID).Selections[0].RecordID; got != f.dx3 {
		t.Fatalf("afterAll local record swap lost: %v", got)
	}

	// 较早拿到的两份清单保留各自查询时的状态：撤回时间仍为 nil，
	// 限定项仍是 dx1/dx2，整类条目仍是 e/医嘱——后来的撤回与两份清单的
	// 篡改都不回溯影响它们。
	assertEarlySnapshot := func(t *testing.T, rows []Authorization, where string) {
		t.Helper()
		limited := findListedAuth(t, rows, f.limited.ID)
		if len(limited.Selections) != 1 || limited.Selections[0] != sel(f.e, Diagnosis, f.dx1) {
			t.Fatalf("%s: limited selection changed: %+v", where, limited.Selections)
		}
		mixed := findListedAuth(t, rows, f.mixed.ID)
		if mixed.RevokedAt != nil {
			t.Fatalf("%s: early snapshot acquired a revoke time: %v", where, mixed.RevokedAt)
		}
		if len(mixed.Selections) != 1 || mixed.Selections[0] != sel(f.e, Diagnosis, f.dx2) {
			t.Fatalf("%s: mixed selection changed: %+v", where, mixed.Selections)
		}
		if len(mixed.Scopes) != 1 || mixed.Scopes[0] != (Scope{EncounterID: f.e, Category: Order}) {
			t.Fatalf("%s: mixed scope changed: %+v", where, mixed.Scopes)
		}
	}
	assertEarlySnapshot(t, beforeAll, "early unfiltered list")
	assertEarlySnapshot(t, beforeRcv, "early receiver-filtered list")

	// 重新查询只反映正式保存的授权。
	fresh, err := f.s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatalf("fresh list: %v", err)
	}
	storedLimited, err := f.s.GetAuthorization(doc, pid, f.limited.ID)
	if err != nil {
		t.Fatalf("get limited: %v", err)
	}
	if got := findListedAuth(t, fresh, f.limited.ID); !reflect.DeepEqual(got, storedLimited) {
		t.Fatalf("fresh limited row diverges from stored:\ngot  = %+v\nwant = %+v", got, storedLimited)
	}
	if got := findListedAuth(t, fresh, f.mixed.ID); !reflect.DeepEqual(got, nowRevoked) {
		t.Fatalf("fresh mixed row diverges from stored:\ngot  = %+v\nwant = %+v", got, nowRevoked)
	}

	// 读取行为以正式授权为准：仍只有 dx1；医嘱因唯一授权已撤回而被拒，
	// 拒绝结果不带受保护内容。
	res, err := f.s.Read(rcv, pid, f.e, Diagnosis)
	if err != nil {
		t.Fatalf("diagnosis read: %v", err)
	}
	if ids := readRecordIDs(res); len(res.Records) != 1 || !ids[f.dx1] || ids[f.dx2] || ids[f.dx3] {
		t.Fatalf("diagnosis read = %v, want only dx1", ids)
	}
	denied, err := f.s.Read(rcv, pid, f.e, Order)
	assertDeniedWithoutContent(t, denied, err, "orders covered only by revoked grant")
}
