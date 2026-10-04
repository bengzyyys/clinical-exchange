package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestGrantSelectiveSaveFailureKeepsStateAndAllowsRetry 覆盖“内部使用者身份合法、
// 患者未停用、就诊/类别/所选记录全部合法、时间窗覆盖操作时间，但本地保存失败”
// 的授权创建场景：整类范围与限定范围并存于同一条新授权。
//
// 保存失败必须以普通错误返回（不能表现为创建成功，也不能伪装成范围或身份不
// 合法等业务错误），且授权列表、审计与接收方可见结果全部维持提交前状态——
// 不允许出现只有授权没有审计、或只有审计没有授权的半成品，接收方也不能提前
// 获得新增范围。保存条件恢复后，提交相同的合法授权必须成功：新增一条完整授权
// 及其创建审计，接收方随后读到整类范围覆盖的全部诊断与明确选中的医嘱。
func TestGrantSelectiveSaveFailureKeepsStateAndAllowsRetry(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)}
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
	// 同一次就诊下：两条已生效诊断、两条已生效医嘱。
	activate := func(category, content string) (ID, Version) {
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
	d1, d1v1 := activate(Diagnosis, "诊断一内容")
	d2, d2v1 := activate(Diagnosis, "诊断二内容")
	o1, o1v1 := activate(Order, "医嘱一内容")
	o2, _ := activate(Order, "医嘱二内容")

	// 接收方原先只有覆盖第一条诊断的有效限定授权。
	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(48 * time.Hour)
	origAuth, err := s.GrantSelective(doc, p.ID, rcv.ID, nil,
		[]RecordSelection{sel(e.ID, Diagnosis, d1)}, start, end)
	if err != nil {
		t.Fatal(err)
	}

	// 提交前的正式基线：授权列表、审计与接收方读取结果。
	authsBefore, err := s.ListAuthorizations(doc, p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(authsBefore) != 1 || authsBefore[0].ID != origAuth.ID {
		t.Fatalf("unexpected baseline authorizations: %+v", authsBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	grantsBefore := auditActions(auditBefore)[ActionGranted]
	readDiagBefore, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if len(readDiagBefore.Records) != 1 || readDiagBefore.Records[0].RecordID != d1 ||
		readDiagBefore.Records[0].VersionID != d1v1.ID {
		t.Fatalf("unexpected baseline diagnosis read: %+v", readDiagBefore)
	}
	// 医嘱原本没有任何授权：明确拒绝且不携带受保护内容。
	if res, err := s.Read(rcv, p.ID, e.ID, Order); !errors.Is(err, ErrAccessDenied) || len(res.Records) != 0 {
		t.Fatalf("baseline order read: res=%+v err=%v, want ErrAccessDenied without content", res, err)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），授权列表、审计与接收方
	// 可见结果都与提交前一致——原授权的范围、有效期与撤回状态不变，没有新授权
	// 的创建事件，接收方不能多出第二条诊断或任何医嘱。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		auths, err := st.ListAuthorizations(doc, p.ID, "")
		if err != nil {
			t.Fatalf("%s: list authorizations: %v", label, err)
		}
		if !reflect.DeepEqual(auths, authsBefore) {
			t.Fatalf("%s: authorization list changed:\nbefore: %+v\nafter:  %+v", label, authsBefore, auths)
		}
		got, err := st.GetAuthorization(doc, p.ID, origAuth.ID)
		if err != nil {
			t.Fatalf("%s: get original authorization: %v", label, err)
		}
		if len(got.Scopes) != 0 || len(got.Selections) != 1 ||
			got.Selections[0] != sel(e.ID, Diagnosis, d1) ||
			!got.StartsAt.Equal(origAuth.StartsAt) || !got.ExpiresAt.Equal(origAuth.ExpiresAt) ||
			got.RevokedAt != nil {
			t.Fatalf("%s: original authorization mutated: %+v", label, got)
		}

		// 既有审计的内容与顺序保持原样，没有新授权的创建事件。
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

		// 接收方仍只能读到第一条诊断的当前内容，第二条诊断不出现。
		res, err := st.Read(rcv, p.ID, e.ID, Diagnosis)
		if err != nil {
			t.Fatalf("%s: receiver diagnosis read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readDiagBefore) {
			t.Fatalf("%s: receiver diagnosis read changed:\nbefore: %+v\nafter:  %+v", label, readDiagBefore, res)
		}
		// 医嘱仍然整体拒绝，且不携带任何受保护内容。
		if res, err := st.Read(rcv, p.ID, e.ID, Order); !errors.Is(err, ErrAccessDenied) || len(res.Records) != 0 {
			t.Fatalf("%s: receiver order read: res=%+v err=%v, want ErrAccessDenied without content", label, res, err)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、范围或所选记录的合法性无关。原数据文件先挪到旁边，事后原样还原。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 身份、所属患者、就诊、类别与所选记录全部合法，时间窗覆盖操作时间，
	// 仅本地保存失败：必须明确报错，且不能伪装成参数非法、记录不存在、
	// 跨患者或访问拒绝等业务结果。（非 nil 错误时返回值无意义，调用方必须
	// 以错误为准；下面的正式状态断言保证它没有成为一条正式授权。）
	newScopes := []Scope{{EncounterID: e.ID, Category: Diagnosis}}
	newSelections := []RecordSelection{sel(e.ID, Order, o1)}
	failedAuth, err := s.GrantSelective(doc, p.ID, rcv.ID, newScopes, newSelections, start, end)
	if err == nil {
		t.Fatal("grant must fail when local save fails")
	}
	if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrMismatchedPatient) || errors.Is(err, ErrAccessDenied) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}
	// 失败尝试不能留下只有授权的结果：即便错误返回中携带了准备好的授权，
	// 它也没有成为正式数据。
	if failedAuth.ID != "" {
		if _, getErr := s.GetAuthorization(doc, p.ID, failedAuth.ID); !errors.Is(getErr, ErrNotFound) {
			t.Fatalf("failed grant surfaced as a stored authorization: %v", getErr)
		}
	}

	// 失败后：授权列表、审计与接收方可见结果全部维持提交前状态。
	checkIntact("after failed save", s)

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

	// 保存条件恢复后：提交相同的合法授权（同患者、同接收方、同样的整类
	// 范围与限定范围、同一时间窗）必须成功。授权窗口仍覆盖操作时间。
	clk.t = clk.t.Add(time.Hour)
	newAuth, err := s2.GrantSelective(doc, p.ID, rcv.ID, newScopes, newSelections, start, end)
	if err != nil {
		t.Fatalf("retry after save recovered: %v", err)
	}
	if newAuth.ID == "" || newAuth.ID == origAuth.ID {
		t.Fatalf("retry must create a new authorization, got %+v", newAuth)
	}
	if !reflect.DeepEqual(newAuth.Scopes, newScopes) ||
		!reflect.DeepEqual(newAuth.Selections, newSelections) ||
		!newAuth.StartsAt.Equal(start) || !newAuth.ExpiresAt.Equal(end) ||
		newAuth.RevokedAt != nil || !newAuth.CreatedAt.Equal(clk.t) {
		t.Fatalf("new authorization wrong: %+v", newAuth)
	}

	// 授权列表恰为两条：原授权在前且原样保留，新授权完整可查。
	auths, err := s2.ListAuthorizations(doc, p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 {
		t.Fatalf("authorizations after retry = %d, want 2", len(auths))
	}
	for _, a := range auths {
		if a.ID == origAuth.ID && !reflect.DeepEqual(a, authsBefore[0]) {
			t.Fatalf("original authorization changed by retry:\nbefore: %+v\nafter:  %+v", authsBefore[0], a)
		}
	}
	gotNew, err := s2.GetAuthorization(doc, p.ID, newAuth.ID)
	if err != nil {
		t.Fatalf("get new authorization: %v", err)
	}
	if !reflect.DeepEqual(gotNew, newAuth) {
		t.Fatalf("stored new authorization differs:\ncreated: %+v\nstored:  %+v", newAuth, gotNew)
	}

	// 审计相对提交前基线只新增一条授权创建事件，指向成功创建的授权并记录
	// 实际操作身份与时间；既有事件的内容与顺序原样保留。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after retry:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionGranted || last.ObjectType != "authorization" ||
		last.ObjectID != newAuth.ID || last.ActorID != doc.ID ||
		last.PatientID != p.ID || !last.OccurredAt.Equal(clk.t) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if got := auditActions(audit)[ActionGranted]; got != grantsBefore+1 {
		t.Fatalf("authorization_granted events = %d, want %d", got, grantsBefore+1)
	}

	// 患者全程未停用、两条授权均在有效期内：读取结果的变化只来自新授权
	// 成功保存。接收方现在能读到两条诊断各自的当前版本。
	res, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("diagnosis read after retry: %v", err)
	}
	if len(res.Records) != 2 {
		t.Fatalf("diagnosis read after retry = %+v, want 2 records", res)
	}
	diagByID := map[ID]EffectiveRecord{}
	for _, rec := range res.Records {
		diagByID[rec.RecordID] = rec
	}
	if got := diagByID[d1]; got.VersionID != d1v1.ID || got.Content != "诊断一内容" {
		t.Fatalf("diagnosis d1 wrong after retry: %+v", got)
	}
	if got := diagByID[d2]; got.VersionID != d2v1.ID || got.Content != "诊断二内容" {
		t.Fatalf("diagnosis d2 wrong after retry: %+v", got)
	}

	// 医嘱不再整体拒绝：明确选中的第一条医嘱可读，第二条医嘱仍不进入
	// 可见结果。
	res, err = s2.Read(rcv, p.ID, e.ID, Order)
	if err != nil {
		t.Fatalf("order read after retry: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != o1 ||
		res.Records[0].VersionID != o1v1.ID || res.Records[0].Content != "医嘱一内容" {
		t.Fatalf("order read after retry = %+v, want only selected o1", res)
	}
	if readRecordIDs(res)[o2] {
		t.Fatal("unselected order o2 must not be visible")
	}
}
