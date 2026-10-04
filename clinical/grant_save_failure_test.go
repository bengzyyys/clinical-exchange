package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestGrantSelectiveSaveFailureKeepsAuthorizationAndAudit 覆盖“内部使用者身份
// 合法、患者未停用、就诊与类别及所选记录全部合法、时间窗覆盖操作时刻——全部
// 业务条件都满足，但本地无法保存这次业务变更”的授权创建场景，且新授权在同
// 一条中同时包含整类范围（该次就诊的诊断）与限定范围（明确选出第一条医嘱）。
//
// 失败必须以普通保存错误返回（不能伪装成 ErrInvalidArgument/ErrAccessDenied/
// ErrDeactivated 等业务结果，也不能表现为创建成功），且：
//   - 内部使用者的授权列表中仍只有原先那条覆盖第一条诊断的限定授权，
//     原有范围、有效期与撤回状态一律不变；
//   - 患者已有审计事件的内容与顺序保持原样，没有新授权的创建事件，
//     不允许出现只有授权没有审计或只有审计没有授权的半成品；
//   - 接收方可见结果维持提交前状态：仍只能读到第一条诊断的当前内容，
//     第二条诊断不出现，读医嘱仍为 ErrAccessDenied 且不带任何受保护内容
//     （新授权明确选中的第一条医嘱不得提前可见）。
//
// 保存条件恢复后，提交相同的合法授权必须成功：新增一条完整授权与指向它的
// 创建审计（记录实际操作身份与时间），接收方随即能读到两条诊断的当前版本
// 与明确选中的第一条医嘱，第二条医嘱仍不可见。全过程授权有效、患者未停用，
// 读取结果的变化只来自授权是否成功保存。
func TestGrantSelectiveSaveFailureKeepsAuthorizationAndAudit(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "授权保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 某次就诊下两条已生效诊断与两条已生效医嘱。
	mkActive := func(category, content string) (ID, Version) {
		t.Helper()
		r, err := s.CreateDraft(doc, p.ID, e.ID, category, content)
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.ActivateRecord(doc, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID, v
	}
	d1, d1v1 := mkActive(Diagnosis, "诊断一内容")
	clk.t = clk.t.Add(time.Hour)
	d2, d2v1 := mkActive(Diagnosis, "诊断二内容")
	clk.t = clk.t.Add(time.Hour)
	o1, o1v1 := mkActive(Order, "医嘱一内容")
	clk.t = clk.t.Add(time.Hour)
	o2, _ := mkActive(Order, "医嘱二内容")

	// 接收方原先只有一条覆盖第一条诊断的有效限定授权。
	clk.t = clk.t.Add(time.Hour)
	origStart := clk.t.Add(-time.Minute)
	origEnd := clk.t.Add(48 * time.Hour)
	orig, err := s.GrantSelective(doc, p.ID, rcv.ID, nil,
		[]RecordSelection{{EncounterID: e.ID, Category: Diagnosis, RecordID: d1}},
		origStart, origEnd)
	if err != nil {
		t.Fatal(err)
	}

	// 失败尝试与恢复后重试使用的是同一份“合法新授权”参数：
	// 整类范围覆盖该次就诊的诊断，限定范围只选第一条医嘱。
	newStart := clk.t.Add(-time.Minute)
	newEnd := clk.t.Add(48 * time.Hour)
	newScopes := []Scope{{EncounterID: e.ID, Category: Diagnosis}}
	newSelections := []RecordSelection{{EncounterID: e.ID, Category: Order, RecordID: o1}}

	// 提交前的正式基线：授权列表、审计、患者状态与接收方读取结果。
	authsBefore, err := s.ListAuthorizations(doc, p.ID, rcv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(authsBefore) != 1 || authsBefore[0].ID != orig.ID {
		t.Fatalf("baseline authorizations = %+v, want only the original limited grant", authsBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore); got[ActionActivated] != 4 || got[ActionGranted] != 1 {
		t.Fatalf("unexpected baseline audit: %v", got)
	}
	grantsBefore := auditActions(auditBefore)[ActionGranted]

	// 提交前接收方可读第一条诊断的当前内容，第二条诊断不出现。
	diagBefore, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("baseline diagnosis read: %v", err)
	}
	if ids := readRecordIDs(diagBefore); len(diagBefore.Records) != 1 || !ids[d1] || ids[d2] {
		t.Fatalf("baseline diagnosis read = %+v, want only d1", diagBefore.Records)
	}
	if diagBefore.Records[0].VersionID != d1v1.ID || diagBefore.Records[0].Content != "诊断一内容" {
		t.Fatalf("baseline diagnosis content wrong: %+v", diagBefore.Records[0])
	}
	// 提交前读医嘱被明确拒绝，且结果不带任何受保护内容（标识、数量、内容）。
	if ordBefore, err := s.Read(rcv, p.ID, e.ID, Order); !errors.Is(err, ErrAccessDenied) ||
		len(ordBefore.Records) != 0 {
		t.Fatalf("baseline order read = %+v err=%v, want ErrAccessDenied without records", ordBefore, err)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），授权、审计与接收方可见
	// 结果全部维持提交前状态——新授权既没有单独成为正式授权，也没有单独留下
	// 创建审计，接收方没有提前获得任何新增范围。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		// 患者保持未停用。
		pat, err := st.GetPatient(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: get patient: %v", label, err)
		}
		if pat.Deactivated {
			t.Fatalf("%s: patient became deactivated", label)
		}

		// 授权列表仍只有原授权：范围、有效期、撤回状态逐项不变。
		auths, err := st.ListAuthorizations(doc, p.ID, rcv.ID)
		if err != nil {
			t.Fatalf("%s: list authorizations: %v", label, err)
		}
		if !reflect.DeepEqual(auths, authsBefore) {
			t.Fatalf("%s: authorization list changed:\nbefore: %+v\nafter:  %+v", label, authsBefore, auths)
		}
		gotOrig, err := st.GetAuthorization(doc, p.ID, orig.ID)
		if err != nil {
			t.Fatalf("%s: get original authorization: %v", label, err)
		}
		if !reflect.DeepEqual(gotOrig, authsBefore[0]) {
			t.Fatalf("%s: original authorization changed:\nbefore: %+v\nafter:  %+v",
				label, authsBefore[0], gotOrig)
		}
		if gotOrig.RevokedAt != nil || !gotOrig.ActiveAt(st.now()) {
			t.Fatalf("%s: original authorization no longer active at %v: %+v",
				label, st.now(), gotOrig)
		}
		if !reflect.DeepEqual(gotOrig.Scopes, []Scope(nil)) ||
			!reflect.DeepEqual(gotOrig.Selections,
				[]RecordSelection{{EncounterID: e.ID, Category: Diagnosis, RecordID: d1}}) {
			t.Fatalf("%s: original scope changed: %+v", label, gotOrig)
		}

		// 审计事件的内容与顺序原样保留，没有新授权的创建事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		if got := auditActions(audit)[ActionGranted]; got != grantsBefore {
			t.Fatalf("%s: authorization_granted events = %d, want %d", label, got, grantsBefore)
		}

		// 接收方诊断读取与提交前完全一致：仍只有第一条诊断的当前内容。
		diag, err := st.Read(rcv, p.ID, e.ID, Diagnosis)
		if err != nil {
			t.Fatalf("%s: receiver diagnosis read: %v", label, err)
		}
		if !reflect.DeepEqual(diag, diagBefore) {
			t.Fatalf("%s: receiver diagnosis read changed:\nbefore: %+v\nafter:  %+v",
				label, diagBefore, diag)
		}
		if ids := readRecordIDs(diag); ids[d2] {
			t.Fatalf("%s: second diagnosis leaked after failed save", label)
		}
		// 接收方读医嘱仍被拒绝：明确选中的第一条医嘱没有提前进入可见结果，
		// 结果中不带任何受保护内容。
		ord, err := st.Read(rcv, p.ID, e.ID, Order)
		if !errors.Is(err, ErrAccessDenied) {
			t.Fatalf("%s: order read err = %v, want ErrAccessDenied", label, err)
		}
		if len(ord.Records) != 0 {
			t.Fatalf("%s: order read carried protected content after failed save: %+v",
				label, ord.Records)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、患者、就诊、类别、所选记录或时间窗等业务校验无关。
	// 原数据文件先挪到旁边，事后原样还原。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 身份、所属患者、就诊、类别与所选记录全部合法，两条授权的时间窗都覆盖
	// 操作时刻，仅本地保存失败：必须明确报错，不能表现为成功，也不能把保存
	// 故障当成范围或身份不合法。
	clk.t = clk.t.Add(time.Hour)
	failedAt := clk.t
	failedAuth, err := s.GrantSelective(doc, p.ID, rcv.ID, newScopes, newSelections, newStart, newEnd)
	if err == nil {
		t.Fatal("grant creation must fail when local save fails")
	} else if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrAccessDenied) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrMismatchedPatient) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 非 nil 错误时返回值无意义；其偶然携带的标识绝不能成为正式授权或
	// 审计的对象（下面的列表/审计逐项断言会兜住这一点）。
	failedID := failedAuth.ID
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：授权列表、审计、原授权状态与接收方可见结果全部维持提交前状态。
	checkIntact("after failed save", s)
	// 失败尝试的标识既不是正式授权，也没有任何审计指向它。
	for _, a := range mustListAuths(t, s, p.ID) {
		if a.ID == failedID {
			t.Fatalf("failed attempt surfaced as an authorization: %+v", a)
		}
	}
	for _, ev := range mustAudit(t, s, p.ID) {
		if ev.ObjectID == failedID {
			t.Fatalf("failed attempt left an audit event: %+v", ev)
		}
	}
	// 原授权的时间窗仍覆盖操作时刻，读取差异只能来自保存是否成功。
	if !orig.ActiveAt(failedAt) || !origStart.Before(failedAt) || !failedAt.Before(origEnd) {
		t.Fatalf("test setup: original authorization must be active at failure time")
	}

	// 关闭后从原数据位置重新打开：先恢复保存条件（还原原数据文件）。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dataPath); err != nil {
		t.Fatalf("unblock data path: %v", err)
	}
	if err := os.Rename(backupPath, dataPath); err != nil {
		t.Fatalf("restore data file: %v", err)
	}

	s2 := open()
	t.Cleanup(func() { _ = s2.Close() })
	// 重开后仍是提交前的正式数据：失败授权与其创建事件都不存在。
	checkIntact("after reopen", s2)

	// 保存条件恢复后：提交相同的合法授权必须成功，得到一条完整的新授权。
	clk.t = clk.t.Add(time.Hour)
	succeededAt := clk.t
	aNew, err := s2.GrantSelective(doc, p.ID, rcv.ID, newScopes, newSelections, newStart, newEnd)
	if err != nil {
		t.Fatalf("retry with the same legal grant after recovery: %v", err)
	}
	if aNew.ID == "" || aNew.ID == orig.ID || aNew.ID == failedID {
		t.Fatalf("retry must create a new distinct authorization, got %+v", aNew)
	}
	if aNew.PatientID != p.ID || aNew.ReceiverID != rcv.ID {
		t.Fatalf("new authorization header wrong: %+v", aNew)
	}
	if !reflect.DeepEqual(aNew.Scopes, newScopes) ||
		!reflect.DeepEqual(aNew.Selections, newSelections) {
		t.Fatalf("new authorization scope wrong: %+v", aNew)
	}
	if !aNew.StartsAt.Equal(newStart) || !aNew.ExpiresAt.Equal(newEnd) ||
		!aNew.CreatedAt.Equal(succeededAt) || aNew.RevokedAt != nil {
		t.Fatalf("new authorization window/lifecycle wrong: %+v", aNew)
	}

	// 授权列表恰为两条：原授权原样保留在前（按标识排序），新授权完整存在。
	auths, err := s2.ListAuthorizations(doc, p.ID, rcv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 {
		t.Fatalf("authorization count after retry = %d, want 2", len(auths))
	}
	// 列表按授权标识排序，标识为随机生成：按标识定位原授权与新授权。
	authByID := map[ID]Authorization{}
	for _, a := range auths {
		authByID[a.ID] = a
	}
	retryOrig, ok := authByID[orig.ID]
	if !ok {
		t.Fatalf("original authorization missing after retry: %+v", auths)
	}
	if !reflect.DeepEqual(retryOrig, authsBefore[0]) {
		t.Fatalf("original authorization mutated by retry:\nbefore: %+v\nafter:  %+v",
			authsBefore[0], retryOrig)
	}
	retryNew, ok := authByID[aNew.ID]
	if !ok {
		t.Fatalf("new authorization missing from list: %+v", auths)
	}
	if !reflect.DeepEqual(retryNew, aNew) {
		t.Fatalf("listed new authorization differs from grant result:\ngrant: %+v\nlist:  %+v",
			aNew, retryNew)
	}

	// 审计相对提交前基线只新增一条授权创建事件，指向成功创建的授权并记录
	// 实际操作身份与时间（采用成功时刻而非失败尝试时刻）；既有事件原样保留。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after retry:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionGranted || last.ObjectType != "authorization" ||
		last.ObjectID != aNew.ID || last.ActorID != doc.ID ||
		last.PatientID != p.ID || !last.OccurredAt.Equal(succeededAt) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if last.OccurredAt.Equal(failedAt) {
		t.Fatalf("audit took the failed attempt's time %v instead of success time %v",
			failedAt, succeededAt)
	}
	if got := auditActions(audit)[ActionGranted]; got != grantsBefore+1 {
		t.Fatalf("authorization_granted events = %d, want %d", got, grantsBefore+1)
	}

	// 接收方诊断读取：整类范围让两条诊断的当前版本都可见，各出现一次并按
	// 记录标识稳定排序。
	diag, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("diagnosis read after retry: %v", err)
	}
	diagByID := map[ID]EffectiveRecord{}
	for _, r := range diag.Records {
		diagByID[r.RecordID] = r
	}
	if len(diag.Records) != 2 {
		t.Fatalf("diagnosis records = %d, want 2: %+v", len(diag.Records), diag.Records)
	}
	if r1 := diagByID[d1]; r1.VersionID != d1v1.ID || r1.Content != "诊断一内容" {
		t.Fatalf("first diagnosis wrong after retry: %+v", r1)
	}
	if r2 := diagByID[d2]; r2.VersionID != d2v1.ID || r2.Content != "诊断二内容" {
		t.Fatalf("second diagnosis wrong after retry: %+v", r2)
	}
	for i := 1; i < len(diag.Records); i++ {
		if diag.Records[i-1].RecordID > diag.Records[i].RecordID {
			t.Fatalf("diagnosis records not in stable id order: %+v", diag.Records)
		}
	}

	// 接收方医嘱读取：只有明确选中的第一条医嘱可见，第二条医嘱仍不进入结果。
	ord, err := s2.Read(rcv, p.ID, e.ID, Order)
	if err != nil {
		t.Fatalf("order read after retry: %v", err)
	}
	if len(ord.Records) != 1 {
		t.Fatalf("order records = %d, want exactly the selected one: %+v",
			len(ord.Records), ord.Records)
	}
	if ord.Records[0].RecordID != o1 || ord.Records[0].VersionID != o1v1.ID ||
		ord.Records[0].Content != "医嘱一内容" {
		t.Fatalf("visible order wrong: %+v", ord.Records[0])
	}
	if ids := readRecordIDs(ord); ids[o2] {
		t.Fatal("unselected second order must not enter the visible result")
	}

	// 患者仍未停用，两条授权此刻都有效——读取变化只来自授权成功保存。
	if pat, _ := s2.GetPatient(doc, p.ID); pat.Deactivated {
		t.Fatal("patient must remain active throughout")
	}
	if !aNew.ActiveAt(succeededAt) || !retryOrig.ActiveAt(succeededAt) {
		t.Fatalf("authorizations must be active at success time %v", succeededAt)
	}

	// 关闭后从同一数据位置重新打开：新授权与其创建审计完整可见，失败尝试
	// 不作为另一份授权或事件出现；接收方读取结果与重开前一致。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })

	gotAuths, err := s3.ListAuthorizations(doc, p.ID, rcv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotAuths) != 2 {
		t.Fatalf("authorization count after reopen = %d, want 2", len(gotAuths))
	}
	var grantedIDs []ID
	for _, ev := range mustAudit(t, s3, p.ID) {
		if ev.Action == ActionGranted {
			grantedIDs = append(grantedIDs, ev.ObjectID)
		}
	}
	if !reflect.DeepEqual(grantedIDs, []ID{orig.ID, aNew.ID}) {
		t.Fatalf("authorization_granted events after reopen = %v, want [%q %q]",
			grantedIDs, orig.ID, aNew.ID)
	}
	diag3, err := s3.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("diagnosis read after reopen: %v", err)
	}
	if !reflect.DeepEqual(diag3, diag) {
		t.Fatalf("diagnosis read changed across reopen:\nbefore: %+v\nafter:  %+v", diag, diag3)
	}
	ord3, err := s3.Read(rcv, p.ID, e.ID, Order)
	if err != nil {
		t.Fatalf("order read after reopen: %v", err)
	}
	if len(ord3.Records) != 1 || ord3.Records[0].RecordID != o1 {
		t.Fatalf("order read after reopen = %+v, want only the selected first order", ord3.Records)
	}
	if ids := readRecordIDs(ord3); ids[o2] {
		t.Fatal("unselected second order leaked after reopen")
	}
}

// mustListAuths 返回该患者的全部授权，失败即终止测试。
func mustListAuths(t *testing.T, s *Store, pid ID) []Authorization {
	t.Helper()
	auths, err := s.ListAuthorizations(doc, pid, "")
	if err != nil {
		t.Fatalf("list authorizations: %v", err)
	}
	return auths
}
