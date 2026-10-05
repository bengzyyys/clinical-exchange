package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestRepeatRevokeConfirmsWithoutSave 覆盖“授权已正式撤回后，合法的重复撤回
// 只是确认已有结果”的约定。
//
// 首次撤回完整落盘后，令本地保存条件暂时不可用（数据文件原位置被同名目录
// 占据）。在此期间：
//   - 原内部使用者再次撤回同一授权：成功返回，不尝试写盘；
//   - 另一名有权的内部使用者在更晚时刻确认同一条已撤回授权：同样成功，
//     撤回时间仍是首次成功撤回的时间，审计中该授权的撤回事件仍只有首次
//     那一条，事件身份与时间不被确认者替换；
//   - 接收方不能撤回；引用不存在的授权、把其他患者的授权用于当前患者，
//     仍按原来的明确错误失败——“已撤回”不构成这些请求成功的理由，这些
//     校验失败也不需要写盘；
//   - 另一条尚未撤回的合法授权在保存不可用时首次撤回：必须明确返回保存
//     错误，不能报告成功；授权保持未撤回、读取与取包不被提前禁止、不新增
//     撤回事件。保存恢复后重新提交才真正撤回，记录这次实际成功的时间与
//     身份，此前失败的尝试不作为成功依据。
func TestRepeatRevokeConfirmsWithoutSave(t *testing.T) {
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
	t.Cleanup(func() { _ = s.Close() })

	p, err := s.RegisterPatient(doc, "重复撤回确认患者")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.RegisterPatient(doc, "重复撤回其他患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	activate := func(content string) ID {
		t.Helper()
		r, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, content)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ActivateRecord(doc, r.ID); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	d1 := activate("诊断一内容")
	d2 := activate("诊断二内容")

	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(48 * time.Hour)
	// a1：整类覆盖全部诊断，准备首次撤回；a2：限定授权，只选出 d1，
	// 保存不可用期间保持有效，用于证明其他授权独立生效、失败尝试不提前禁权。
	a1, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.GrantSelective(doc, p.ID, rcv.ID, nil,
		[]RecordSelection{sel(e.ID, Diagnosis, d1)}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	// 一份绑定 a1 的待回执交换，用于观察撤回与失败尝试对取包的影响。
	exch, err := s.CreateExchange(doc, p.ID, rcv.ID, a1.ID, []ID{d1, d2}, "req-repeat-revoke")
	if err != nil {
		t.Fatal(err)
	}

	// ---- 首次成功撤回：完整保存授权变化与审计 ----
	firstRevokeAt := clk.t.Add(time.Hour)
	clk.t = firstRevokeAt
	if err := s.Revoke(doc, p.ID, a1.ID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	first, err := s.GetAuthorization(doc, p.ID, a1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.RevokedAt == nil || !first.RevokedAt.Equal(firstRevokeAt) {
		t.Fatalf("first revocation time = %v, want %v", first.RevokedAt, firstRevokeAt)
	}
	auditAfterFirst, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditAfterFirst)[ActionRevoked]; got != 1 {
		t.Fatalf("revoke events after first revoke = %d, want 1", got)
	}
	firstScope := append([]Scope(nil), first.Scopes...)
	firstSels := append([]RecordSelection(nil), first.Selections...)

	// ---- 令本地保存条件暂时不可用：数据文件原位置被同名目录占据 ----
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataPath) })

	// 原内部使用者重复撤回：必须成功，不依赖当前能否写入本地数据。
	later := clk.t.Add(2 * time.Hour)
	clk.t = later
	if err := s.Revoke(doc, p.ID, a1.ID); err != nil {
		t.Fatalf("repeat revoke by same actor must succeed while save unavailable: %v", err)
	}

	// 另一名有权的内部使用者在更晚时刻确认：同样成功，且撤回时间不被更新
	// 到本次调用时刻，接收方、范围、有效期保持原样。
	clk.t = later.Add(3 * time.Hour)
	if err := s.Revoke(doc2, p.ID, a1.ID); err != nil {
		t.Fatalf("repeat revoke by another internal actor must succeed: %v", err)
	}
	again, err := s.GetAuthorization(doc2, p.ID, a1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.RevokedAt == nil || !again.RevokedAt.Equal(firstRevokeAt) {
		t.Fatalf("repeat confirmation changed revocation time: got %v, want %v",
			again.RevokedAt, firstRevokeAt)
	}
	if again.ReceiverID != first.ReceiverID ||
		!reflect.DeepEqual(again.Scopes, firstScope) ||
		!reflect.DeepEqual(again.Selections, firstSels) ||
		!again.StartsAt.Equal(first.StartsAt) || !again.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("repeat confirmation changed receiver/scope/window:\nfirst: %+v\nafter: %+v",
			first, again)
	}

	// 按患者查看审计：这条授权的撤回事件仍只有首次那一条，身份与时间不变。
	audit, err := s.AuditEvents(doc2, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var revokeEvents []AuditEvent
	for _, ev := range audit {
		if ev.Action == ActionRevoked {
			revokeEvents = append(revokeEvents, ev)
		}
	}
	if len(revokeEvents) != 1 {
		t.Fatalf("revoke events = %+v, want exactly the first one", revokeEvents)
	}
	if ev := revokeEvents[0]; ev.ObjectID != a1.ID || ev.ActorID != doc.ID ||
		!ev.OccurredAt.Equal(firstRevokeAt) {
		t.Fatalf("first revoke event altered by confirmation: %+v", ev)
	}

	// 既有身份与归属规则不变：接收方不能撤回；不存在/跨患者仍明确失败，
	// “已撤回”不能让这些请求成功，也与保存条件无关。
	if err := s.Revoke(rcv, p.ID, a1.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver revoke err = %v, want ErrAccessDenied", err)
	}
	if err := s.Revoke(doc, p.ID, "auth_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing authorization while save blocked: err = %v, want ErrNotFound", err)
	}
	if err := s.Revoke(doc, p2.ID, a1.ID); !errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("cross-patient revoke while save blocked: err = %v, want ErrMismatchedPatient", err)
	}

	// 已撤回授权继续失效：a1 整类撤回后，接收方只能靠独立生效的 a2 读到 d1。
	res, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("receiver read with the independent limited grant: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != d1 {
		t.Fatalf("read after repeat confirmations = %+v, want only d1 via the other grant", res.Records)
	}
	// 绑定 a1 的交换取包仍被拒，重复确认不改写既有交换权限。
	if delivery, err := s.FetchPackage(rcv, exch.ID); !errors.Is(err, ErrAccessDenied) ||
		delivery.Digest != "" || len(delivery.Package.Records) != 0 {
		t.Fatalf("fetch bound to revoked auth: delivery=%+v err=%v", delivery, err)
	}

	// ---- 尚未撤回的 a2 在保存不可用时首次撤回：必须报保存错误 ----
	if err := s.Revoke(doc, p.ID, a2.ID); err == nil {
		t.Fatal("first revoke must fail when local save is unavailable")
	} else if errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrMismatchedPatient) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	gotA2, err := s.GetAuthorization(doc, p.ID, a2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotA2.RevokedAt != nil {
		t.Fatalf("failed first revoke must not mark authorization revoked: %+v", gotA2)
	}
	// 失败尝试不能提前禁止原授权允许的读取。
	res, err = s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("failed revoke must not cut access early: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != d1 {
		t.Fatalf("read after failed revoke = %+v, want d1 still visible", res.Records)
	}
	// 不新增撤回事件。
	auditNow, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditNow, audit) {
		t.Fatalf("failed first revoke changed audit:\nbefore: %+v\nafter:  %+v", audit, auditNow)
	}
	// 失败不能留下半截临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}
	// 此前失败的尝试不能成为直接成功的依据：保存仍不可用时再次提交仍失败。
	if err := s.Revoke(doc2, p.ID, a2.ID); err == nil {
		t.Fatal("retry while save still unavailable must not succeed on the strength of a failed attempt")
	}
	if gotA2, err := s.GetAuthorization(doc, p.ID, a2.ID); err != nil || gotA2.RevokedAt != nil {
		t.Fatalf("retry after failed attempt must leave authorization unrevoked: %+v err=%v", gotA2, err)
	}

	// ---- 恢复保存条件：随后提交才真正撤回，记录本次实际成功的时间与身份 ----
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dataPath); err != nil {
		t.Fatalf("unblock data path: %v", err)
	}
	if err := os.Rename(backupPath, dataPath); err != nil {
		t.Fatalf("restore data file: %v", err)
	}
	s = open()
	t.Cleanup(func() { _ = s.Close() })

	// 重开后：a1 仍是首次撤回的结果与唯一事件；a2 仍未撤回。
	reloadedA1, err := s.GetAuthorization(doc, p.ID, a1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedA1.RevokedAt == nil || !reloadedA1.RevokedAt.Equal(firstRevokeAt) {
		t.Fatalf("a1 after reopen: %+v", reloadedA1)
	}
	reloadedA2, err := s.GetAuthorization(doc, p.ID, a2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedA2.RevokedAt != nil {
		t.Fatalf("a2 must remain unrevoked after reopen: %+v", reloadedA2)
	}

	secondRevokeAt := clk.t.Add(5 * time.Hour)
	clk.t = secondRevokeAt
	if err := s.Revoke(doc2, p.ID, a2.ID); err != nil {
		t.Fatalf("revoke a2 after save recovered: %v", err)
	}
	doneA2, err := s.GetAuthorization(doc2, p.ID, a2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doneA2.RevokedAt == nil || !doneA2.RevokedAt.Equal(secondRevokeAt) {
		t.Fatalf("a2 revocation time = %v, want %v", doneA2.RevokedAt, secondRevokeAt)
	}

	// 最终审计：a1 的撤回事件仍属首次操作（doc、firstRevokeAt），
	// a2 新增一条属于实际成功操作（doc2、secondRevokeAt）的事件。
	finalAudit, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var finalRevokes []AuditEvent
	for _, ev := range finalAudit {
		if ev.Action == ActionRevoked {
			finalRevokes = append(finalRevokes, ev)
		}
	}
	if len(finalRevokes) != 2 {
		t.Fatalf("final revoke events = %+v, want 2", finalRevokes)
	}
	byObject := map[ID]AuditEvent{}
	for _, ev := range finalRevokes {
		byObject[ev.ObjectID] = ev
	}
	if ev := byObject[a1.ID]; ev.ActorID != doc.ID || !ev.OccurredAt.Equal(firstRevokeAt) {
		t.Fatalf("a1 revoke event must stay the first one: %+v", ev)
	}
	if ev := byObject[a2.ID]; ev.ActorID != doc2.ID || !ev.OccurredAt.Equal(secondRevokeAt) {
		t.Fatalf("a2 revoke event must record the actual successful operation: %+v", ev)
	}

	// 两条授权都撤回后：接收方读取被拒，但其他患者/档案状态不受影响。
	if _, err := s.Read(rcv, p.ID, e.ID, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("read after both revokes: err = %v, want ErrAccessDenied", err)
	}
}
