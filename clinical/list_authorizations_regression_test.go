package clinical

import (
	"errors"
	"sort"
	"testing"
	"time"
)

// ---- ListAuthorizations 回归：多患者、多接收方、多状态并存 ----

// 同一存储中两名患者、两名接收方、四种授权状态（有效/未开始/已到期/已撤回）
// 并存时，内部列表必须同时按患者与接收方两个条件过滤，结果按授权标识升序，
// 且各授权的标识、归属、接收方、有效期、撤回时间与范围保持已保存的值。
func TestListAuthorizationsFiltersByPatientAndReceiver(t *testing.T) {
	s, clk := newTestStore(t)

	// 患者甲：两次就诊，各备一条已生效诊断记录（供限定范围选择）。
	pidA, encA1 := setupPatientEncounter(t, s)
	encA2Obj, err := s.AddEncounter(doc, pidA, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	encA2ID := encA2Obj.ID
	mkDiag := func(pid, eid ID, content string) ID {
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
	recA1 := mkDiag(pidA, encA1, "甲-诊断1")
	recA2 := mkDiag(pidA, encA2ID, "甲-诊断2")

	// 患者乙：一名就诊，同样授权给 rcv-a（与患者甲共用接收方）。
	pidB, encB := setupPatientEncounter(t, s)

	now := clk.t
	// 患者甲 → rcv-a：四种状态各一条。
	aActive, err := s.Grant(doc, pidA, rcv.ID,
		[]Scope{{EncounterID: encA1, Category: Diagnosis}},
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	aFuture, err := s.Grant(doc, pidA, rcv.ID,
		[]Scope{{EncounterID: encA1, Category: Order}},
		now.Add(2*time.Hour), now.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	aExpired, err := s.Grant(doc, pidA, rcv.ID,
		[]Scope{{EncounterID: encA2ID, Category: Diagnosis}},
		now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	aRevoked, err := s.Grant(doc, pidA, rcv.ID,
		[]Scope{{EncounterID: encA2ID, Category: Order}},
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 患者甲 → rcv-b：一条纯限定范围、一条整类与限定并存。
	aSelective, err := s.GrantSelective(doc, pidA, rcvB.ID, nil,
		[]RecordSelection{{EncounterID: encA1, Category: Diagnosis, RecordID: recA1}},
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	aCombined, err := s.GrantSelective(doc, pidA, rcvB.ID,
		[]Scope{{EncounterID: encA2ID, Category: Diagnosis}},
		[]RecordSelection{{EncounterID: encA2ID, Category: Diagnosis, RecordID: recA2}},
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 患者乙 → rcv-a：接收方相同但患者不同，绝不能混入患者甲的结果。
	bActive, err := s.Grant(doc, pidB, rcv.ID,
		[]Scope{{EncounterID: encB, Category: Diagnosis}},
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// 撤回其中一条：撤回时间有值也不能让该条从列表中消失。
	clk.t = now.Add(30 * time.Minute)
	revokedAt := clk.t
	if err := s.Revoke(doc, pidA, aRevoked.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	clk.t = now

	sortedIDs := func(auths []Authorization) []ID {
		ids := make([]ID, 0, len(auths))
		for _, a := range auths {
			ids = append(ids, a.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return ids
	}
	wantIDs := func(auths ...Authorization) []ID {
		return sortedIDs(auths)
	}
	gotIDs := func(list []Authorization) []ID {
		ids := make([]ID, 0, len(list))
		for _, a := range list {
			ids = append(ids, a.ID)
		}
		return ids
	}
	equalIDs := func(a, b []ID) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// 不指定接收方：看到所选患者的全部授权，四种状态都在，
	// 按授权标识升序；患者乙的授权（接收方相同）不得混入。
	listA, err := s.ListAuthorizations(doc, pidA, "")
	if err != nil {
		t.Fatalf("list patient A: %v", err)
	}
	wantA := wantIDs(aActive, aFuture, aExpired, aRevoked, aSelective, aCombined)
	if !equalIDs(gotIDs(listA), wantA) {
		t.Fatalf("patient A list ids = %v, want %v", gotIDs(listA), wantA)
	}

	// 指定接收方：只留下该患者授予此接收方的授权；患者相同也不能
	// 混入授予其他接收方的授权。筛选后仍按授权标识升序。
	listARcv, err := s.ListAuthorizations(doc, pidA, rcv.ID)
	if err != nil {
		t.Fatalf("list patient A receiver rcv-a: %v", err)
	}
	wantARcv := wantIDs(aActive, aFuture, aExpired, aRevoked)
	if !equalIDs(gotIDs(listARcv), wantARcv) {
		t.Fatalf("patient A rcv-a ids = %v, want %v", gotIDs(listARcv), wantARcv)
	}
	listARcvB, err := s.ListAuthorizations(doc, pidA, rcvB.ID)
	if err != nil {
		t.Fatalf("list patient A receiver rcv-b: %v", err)
	}
	if want := wantIDs(aSelective, aCombined); !equalIDs(gotIDs(listARcvB), want) {
		t.Fatalf("patient A rcv-b ids = %v, want %v", gotIDs(listARcvB), want)
	}

	// 患者乙：只有自己的授权；指定从未获得其授权的接收方返回空列表。
	listB, err := s.ListAuthorizations(doc, pidB, "")
	if err != nil {
		t.Fatalf("list patient B: %v", err)
	}
	if want := wantIDs(bActive); !equalIDs(gotIDs(listB), want) {
		t.Fatalf("patient B ids = %v, want %v", gotIDs(listB), want)
	}
	listBRcvB, err := s.ListAuthorizations(doc, pidB, rcvB.ID)
	if err != nil {
		t.Fatalf("list patient B receiver rcv-b: %v", err)
	}
	if len(listBRcvB) != 0 {
		t.Fatalf("patient B never granted rcv-b, got %+v", listBRcvB)
	}

	// 逐条核对已保存的值：标识、归属、接收方、有效期、撤回时间与范围。
	byID := map[ID]Authorization{}
	for _, a := range listA {
		byID[a.ID] = a
	}
	checkAuth := func(want Authorization, wantScopes []Scope, wantSels []RecordSelection, revokedAt *time.Time) {
		t.Helper()
		got, ok := byID[want.ID]
		if !ok {
			t.Fatalf("authorization %q missing from list", want.ID)
		}
		if got.PatientID != want.PatientID || got.ReceiverID != want.ReceiverID {
			t.Fatalf("auth %q ownership = %s/%s, want %s/%s",
				got.ID, got.PatientID, got.ReceiverID, want.PatientID, want.ReceiverID)
		}
		if !got.StartsAt.Equal(want.StartsAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Fatalf("auth %q window = [%v,%v), want [%v,%v)",
				got.ID, got.StartsAt, got.ExpiresAt, want.StartsAt, want.ExpiresAt)
		}
		if revokedAt == nil {
			if got.RevokedAt != nil {
				t.Fatalf("auth %q unexpectedly revoked at %v", got.ID, got.RevokedAt)
			}
		} else {
			if got.RevokedAt == nil || !got.RevokedAt.Equal(*revokedAt) {
				t.Fatalf("auth %q revoked-at = %v, want %v", got.ID, got.RevokedAt, *revokedAt)
			}
		}
		if len(got.Scopes) != len(wantScopes) {
			t.Fatalf("auth %q scopes = %+v, want %+v", got.ID, got.Scopes, wantScopes)
		}
		for i, sc := range wantScopes {
			if got.Scopes[i] != sc {
				t.Fatalf("auth %q scope %d = %+v, want %+v", got.ID, i, got.Scopes[i], sc)
			}
		}
		if len(got.Selections) != len(wantSels) {
			t.Fatalf("auth %q selections = %+v, want %+v", got.ID, got.Selections, wantSels)
		}
		for i, sel := range wantSels {
			if got.Selections[i] != sel {
				t.Fatalf("auth %q selection %d = %+v, want %+v", got.ID, i, got.Selections[i], sel)
			}
		}
	}
	checkAuth(aActive, []Scope{{EncounterID: encA1, Category: Diagnosis}}, nil, nil)
	checkAuth(aFuture, []Scope{{EncounterID: encA1, Category: Order}}, nil, nil)
	checkAuth(aExpired, []Scope{{EncounterID: encA2ID, Category: Diagnosis}}, nil, nil)
	checkAuth(aRevoked, []Scope{{EncounterID: encA2ID, Category: Order}}, nil, &revokedAt)
	// 纯限定范围保持限定，不被改成整类范围。
	checkAuth(aSelective, nil,
		[]RecordSelection{{EncounterID: encA1, Category: Diagnosis, RecordID: recA1}}, nil)
	// 整类与限定并存的授权保留各自原本的就诊、类别与记录选择，
	// 不合并成一条、也不互相转化。
	checkAuth(aCombined,
		[]Scope{{EncounterID: encA2ID, Category: Diagnosis}},
		[]RecordSelection{{EncounterID: encA2ID, Category: Diagnosis, RecordID: recA2}}, nil)

	// 存在但没有授权的患者：成功返回空列表。
	pidC, err := s.RegisterPatient(doc, "合成患者丙")
	if err != nil {
		t.Fatal(err)
	}
	listC, err := s.ListAuthorizations(doc, pidC.ID, "")
	if err != nil {
		t.Fatalf("list patient without grants: %v", err)
	}
	if len(listC) != 0 {
		t.Fatalf("patient without grants listed %d", len(listC))
	}

	// 患者不存在：ErrNotFound。
	if _, err := s.ListAuthorizations(doc, "pat_missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing patient err = %v, want ErrNotFound", err)
	}

	// 接收方即使对该患者持有有效授权，也不能使用内部查询，
	// 且结果不得带回任何授权资料。
	gotRcv, err := s.ListAuthorizations(rcv, pidA, "")
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver list err = %v, want ErrAccessDenied", err)
	}
	if len(gotRcv) != 0 {
		t.Fatalf("receiver list carried authorizations: %+v", gotRcv)
	}
	gotRcvB, err := s.ListAuthorizations(rcvB, pidA, rcvB.ID)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver list own id err = %v, want ErrAccessDenied", err)
	}
	if len(gotRcvB) != 0 {
		t.Fatalf("receiver list carried authorizations: %+v", gotRcvB)
	}

	// 查看列表本身不新增授权、不新增审计。
	audBefore := len(mustAudit(t, s, pidA))
	if _, err := s.ListAuthorizations(doc, pidA, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListAuthorizations(doc, pidA, rcv.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(mustAudit(t, s, pidA)); n != audBefore {
		t.Fatalf("listing added audit events: %d -> %d", audBefore, n)
	}
	listAgain, err := s.ListAuthorizations(doc, pidA, "")
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(gotIDs(listAgain), wantA) {
		t.Fatalf("listing changed authorizations: %v, want %v", gotIDs(listAgain), wantA)
	}
}

// 患者档案停用后，内部使用者仍可查看此前建立的授权：
// 列表不清空、不拒绝访问，各授权保持已保存的值。
func TestListAuthorizationsAfterDeactivation(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)

	now := clk.t
	a1, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.Grant(doc, pid, rcvB.ID,
		[]Scope{{EncounterID: eid, Category: Order}},
		now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(doc, pid, a2.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	list, err := s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatalf("list after deactivation: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list after deactivation = %d, want 2", len(list))
	}
	byID := map[ID]Authorization{}
	for _, a := range list {
		byID[a.ID] = a
	}
	got1, ok := byID[a1.ID]
	if !ok || got1.ReceiverID != rcv.ID || got1.RevokedAt != nil {
		t.Fatalf("active grant after deactivation = %+v", got1)
	}
	got2, ok := byID[a2.ID]
	if !ok || got2.ReceiverID != rcvB.ID || got2.RevokedAt == nil {
		t.Fatalf("revoked grant after deactivation = %+v", got2)
	}
	// 按接收方筛选在停用后同样可用。
	listRcv, err := s.ListAuthorizations(doc, pid, rcv.ID)
	if err != nil {
		t.Fatalf("filtered list after deactivation: %v", err)
	}
	if len(listRcv) != 1 || listRcv[0].ID != a1.ID {
		t.Fatalf("filtered list after deactivation = %+v", listRcv)
	}
}
