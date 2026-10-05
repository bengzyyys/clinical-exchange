package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestRepeatDeactivateConfirmsWithoutSave 覆盖“患者档案已正式停用后，合法的
// 重复停用只是确认已有结果”的约定。
//
// 首次停用完整落盘后，令本地保存条件暂时不可用（数据文件原位置被同名目录
// 占据）。在此期间：
//   - 原内部使用者再次停用同一患者：成功返回，不尝试写盘；
//   - 另一名有权的内部使用者在更晚时刻确认同一名已停用患者：同样成功，
//     停用审计仍只有首次那一条，事件身份与时间不被确认者替换，患者档案、
//     就诊、草稿、版本历史、授权与交换内容保持原状，接收方仍受停用后的
//     读取限制，内部使用者仍能按原权限查看历史；
//   - 接收方不能停用；不存在的患者仍返回 ErrNotFound——“已停用”不构成
//     这些请求成功的理由，这些校验失败也不需要写盘；
//   - 同存储中另一名尚未停用的合法患者在保存不可用时首次停用：必须明确
//     返回保存错误，不能报告成功；患者保持未停用、接收方读取不被提前
//     禁止、不新增停用事件。保存恢复后重新提交才真正停用，记录这次实际
//     成功的时间与身份，此前失败的尝试不作为成功依据。
//
// 存储关闭后，合法内部使用者的确认请求仍返回 ErrClosed。
func TestRepeatDeactivateConfirmsWithoutSave(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	t.Cleanup(func() { _ = s.Close() })

	p, err := s.RegisterPatient(doc, "重复停用确认患者")
	if err != nil {
		t.Fatal(err)
	}
	// 同存储中另一名尚未停用的患者：保存不可用期间其首次停用必须失败。
	p2, err := s.RegisterPatient(doc, "重复停用其他患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.AddEncounter(doc, p2.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	draftContent := "尚未生效的草稿内容"
	draft, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, draftContent)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断：糖尿病 E11")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 接收方对 p 持有覆盖整类诊断的有效授权：停用后仍应被拒。
	auth, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 接收方对 p2 持有有效授权：p2 的失败停用尝试不能提前禁止读取。
	auth2, err := s.Grant(doc, p2.ID, rcv.ID,
		[]Scope{{EncounterID: e2.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.CreateDraft(doc, p2.ID, e2.ID, Diagnosis, "其他患者诊断")
	if err != nil {
		t.Fatal(err)
	}
	v2p2, err := s.ActivateRecord(doc, r2.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 一份绑定 p 授权的交换：重复确认不改写其停用后的取包限制。
	exch, err := s.CreateExchange(doc, p.ID, rcv.ID, auth.ID, []ID{r.ID}, "req-repeat-deactivate")
	if err != nil {
		t.Fatal(err)
	}

	// ---- 首次成功停用 p：完整保存患者状态与审计 ----
	firstAt := clk.t.Add(time.Hour)
	clk.t = firstAt
	if err := s.DeactivatePatient(doc, p.ID); err != nil {
		t.Fatalf("first deactivate: %v", err)
	}
	first, err := s.GetPatient(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Deactivated {
		t.Fatal("patient must be deactivated after first submit")
	}
	auditAfterFirst, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditAfterFirst)[ActionDeactivated]; got != 1 {
		t.Fatalf("deactivate events after first = %d, want 1", got)
	}

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

	// 原内部使用者重复停用：必须成功，不依赖当前能否写入本地数据。
	later := firstAt.Add(2 * time.Hour)
	clk.t = later
	if err := s.DeactivatePatient(doc, p.ID); err != nil {
		t.Fatalf("repeat deactivate by same actor must succeed while save unavailable: %v", err)
	}
	// 不能留下半截写入的临时文件——确认路径根本不应尝试写盘。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("repeat confirmation attempted a save, temp file present: %v", statErr)
	}

	// 另一名有权的内部使用者在更晚时刻确认：同样成功，停用结果不被更新。
	confirmAt := later.Add(3 * time.Hour)
	clk.t = confirmAt
	if err := s.DeactivatePatient(doc2, p.ID); err != nil {
		t.Fatalf("repeat deactivate by another internal actor must succeed: %v", err)
	}
	again, err := s.GetPatient(doc2, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Deactivated {
		t.Fatal("patient must still show deactivated after repeat confirmation")
	}

	// 按患者查看审计：停用事件仍只有首次那一条，身份、时间、对象、事件标识不变，
	// 事件数量与排列顺序保持原样。
	audit, err := s.AuditEvents(doc2, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var deactivateEvents []AuditEvent
	for _, ev := range audit {
		if ev.Action == ActionDeactivated {
			deactivateEvents = append(deactivateEvents, ev)
		}
	}
	if len(deactivateEvents) != 1 {
		t.Fatalf("deactivate events = %+v, want exactly the first one", deactivateEvents)
	}
	if ev := deactivateEvents[0]; ev.ObjectType != "patient" || ev.ObjectID != p.ID ||
		ev.PatientID != p.ID || ev.ActorID != doc.ID || !ev.OccurredAt.Equal(firstAt) {
		t.Fatalf("first deactivate event altered by confirmation: %+v", ev)
	}
	if !reflect.DeepEqual(audit, auditAfterFirst) {
		t.Fatalf("audit changed by repeat confirmation:\nfirst: %+v\nafter: %+v",
			auditAfterFirst, audit)
	}

	// 身份与对象限制保留：接收方不能停用；不存在的患者仍 ErrNotFound；
	// “已停用”不能让这些请求成功，也与保存条件无关。
	if err := s.DeactivatePatient(rcv, p.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver deactivate err = %v, want ErrAccessDenied", err)
	}
	if err := s.DeactivatePatient(doc, "pat_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deactivate missing patient while save blocked: err = %v, want ErrNotFound", err)
	}

	// 已停用档案继续生效：接收方读取被拒且不携带内容，绑定授权的取包仍被拒；
	// 授权本身仍存在、未撤回且在有效期内。
	if denied, err := s.Read(rcv, p.ID, e.ID, Diagnosis); !errors.Is(err, ErrAccessDenied) ||
		len(denied.Records) != 0 || denied.EncounterID != "" || denied.Category != "" {
		t.Fatalf("receiver read after repeat confirmation: res=%+v err=%v, want ErrAccessDenied without content",
			denied, err)
	}
	if delivery, err := s.FetchPackage(rcv, exch.ID); !errors.Is(err, ErrAccessDenied) ||
		delivery.Digest != "" || len(delivery.Package.Records) != 0 {
		t.Fatalf("fetch after repeat confirmation: delivery=%+v err=%v, want ErrAccessDenied without content",
			delivery, err)
	}
	gotAuth, err := s.GetAuthorization(doc, p.ID, auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth.RevokedAt != nil || !gotAuth.ActiveAt(confirmAt) {
		t.Fatalf("authorization must remain intact and active while reads stay denied: %+v", gotAuth)
	}
	// 内部使用者仍能查看原有草稿与完整版本历史，重复确认不改写本地数据。
	chart, err := s.Chart(doc2, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !chart.Patient.Deactivated {
		t.Fatal("chart should show deactivated")
	}
	dh := findHistory(chart, draft.ID)
	if dh == nil || !dh.HasDraft || dh.DraftContent != draftContent ||
		dh.CurrentVersion != nil {
		t.Fatalf("draft no longer intact after repeat confirmation: %+v", dh)
	}
	ah := findHistory(chart, r.ID)
	if ah == nil || ah.CurrentVersion == nil || ah.CurrentVersion.ID != v1.ID ||
		len(ah.Versions) != 1 {
		t.Fatalf("version history no longer intact after repeat confirmation: %+v", ah)
	}

	// ---- p2 尚未停用：保存不可用时首次停用必须报保存错误 ----
	if err := s.DeactivatePatient(doc, p2.ID); err == nil {
		t.Fatal("first deactivate must fail when local save is unavailable")
	} else if errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrMismatchedPatient) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrClosed) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	gotP2, err := s.GetPatient(doc, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotP2.Deactivated {
		t.Fatalf("failed first deactivate must not mark patient deactivated: %+v", gotP2)
	}
	// 失败尝试不能提前禁止原授权允许的读取。
	res, err := s.Read(rcv, p2.ID, e2.ID, Diagnosis)
	if err != nil {
		t.Fatalf("failed deactivate must not cut access early: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != r2.ID ||
		res.Records[0].VersionID != v2p2.ID {
		t.Fatalf("read after failed deactivate = %+v, want the current version still visible", res)
	}
	// 不新增停用事件。
	auditP2, err := s.AuditEvents(doc, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditP2)[ActionDeactivated]; got != 0 {
		t.Fatalf("p2 deactivate events after failed attempt = %d, want 0", got)
	}
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}
	// 此前失败的尝试不能成为直接成功的依据：保存仍不可用时再次提交仍失败，
	// 由另一名内部使用者在更晚时刻提交也一样。
	clk.t = confirmAt.Add(time.Hour)
	if err := s.DeactivatePatient(doc2, p2.ID); err == nil {
		t.Fatal("retry while save still unavailable must not succeed on the strength of a failed attempt")
	}
	if gotP2, err := s.GetPatient(doc, p2.ID); err != nil || gotP2.Deactivated {
		t.Fatalf("retry after failed attempt must leave patient active: %+v err=%v", gotP2, err)
	}

	// 存储关闭后：合法内部使用者的确认请求仍返回 ErrClosed，不能仅凭此前
	// 停用过就报告成功。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.DeactivatePatient(doc, p.ID); !errors.Is(err, ErrClosed) {
		t.Fatalf("deactivate on closed store err = %v, want ErrClosed", err)
	}

	// ---- 恢复保存条件：随后提交 p2 才真正停用，记录实际成功的时间与身份 ----
	if err := os.Remove(dataPath); err != nil {
		t.Fatalf("unblock data path: %v", err)
	}
	if err := os.Rename(backupPath, dataPath); err != nil {
		t.Fatalf("restore data file: %v", err)
	}
	s = open()
	t.Cleanup(func() { _ = s.Close() })

	// 重开后：p 仍是首次停用的结果与唯一事件；p2 仍未停用，其授权读取仍有效。
	reloadedP, err := s.GetPatient(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloadedP.Deactivated {
		t.Fatalf("p deactivated state not preserved after reopen: %+v", reloadedP)
	}
	reloadedP2, err := s.GetPatient(doc, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedP2.Deactivated {
		t.Fatalf("p2 must remain active after reopen: %+v", reloadedP2)
	}
	pEvents, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pDeactTimes []time.Time
	for _, ev := range pEvents {
		if ev.Action == ActionDeactivated {
			pDeactTimes = append(pDeactTimes, ev.OccurredAt)
		}
	}
	if len(pDeactTimes) != 1 || !pDeactTimes[0].Equal(firstAt) {
		t.Fatalf("p deactivate events after reopen = %v, want only [%v]", pDeactTimes, firstAt)
	}

	successAt := confirmAt.Add(5 * time.Hour)
	clk.t = successAt
	if err := s.DeactivatePatient(doc2, p2.ID); err != nil {
		t.Fatalf("deactivate p2 after save recovered: %v", err)
	}
	doneP2, err := s.GetPatient(doc2, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !doneP2.Deactivated {
		t.Fatal("p2 must be deactivated after successful submit")
	}
	finalAudit, err := s.AuditEvents(doc, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	var p2Deacts []AuditEvent
	for _, ev := range finalAudit {
		if ev.Action == ActionDeactivated {
			p2Deacts = append(p2Deacts, ev)
		}
	}
	if len(p2Deacts) != 1 {
		t.Fatalf("p2 deactivate events = %+v, want 1", p2Deacts)
	}
	if ev := p2Deacts[0]; ev.ActorID != doc2.ID || !ev.OccurredAt.Equal(successAt) ||
		ev.ObjectID != p2.ID || ev.PatientID != p2.ID {
		t.Fatalf("p2 deactivate event must record the actual successful operation: %+v", ev)
	}
	// p2 停用后接收方读取被拒，原授权仍存在。
	if _, err := s.Read(rcv, p2.ID, e2.ID, Diagnosis); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver read after p2 deactivation: err = %v, want ErrAccessDenied", err)
	}
	gotAuth2, err := s.GetAuthorization(doc, p2.ID, auth2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth2.RevokedAt != nil {
		t.Fatalf("p2 authorization must remain unrevoked after deactivation: %+v", gotAuth2)
	}

	// 最终确认：p 的停用事件仍属首次操作（doc、firstAt），不被后续确认者替换。
	finalPEvents, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var finalPDeacts []AuditEvent
	for _, ev := range finalPEvents {
		if ev.Action == ActionDeactivated {
			finalPDeacts = append(finalPDeacts, ev)
		}
	}
	if len(finalPDeacts) != 1 {
		t.Fatalf("final p deactivate events = %+v, want 1", finalPDeacts)
	}
	if ev := finalPDeacts[0]; ev.ActorID != doc.ID || !ev.OccurredAt.Equal(firstAt) ||
		ev.ID != deactivateEvents[0].ID {
		t.Fatalf("first p deactivate event must stay unchanged: %+v", ev)
	}
}
