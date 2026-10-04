package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestRevokeSaveFailureKeepsStateAndAllowsRetry 覆盖“内部使用者提交的撤回本身
// 完全合法（身份有权、对象存在且属于该患者、授权尚未撤回），却因本地保存失败
// 而未能完成”的授权撤回场景。
//
// 前置数据：患者档案全程未停用；同一次就诊下有两条已生效诊断；同一接收方持有
// 两条当前有效的授权——准备撤回的目标授权整类覆盖这次就诊的全部诊断，另一条
// 限定授权只明确选出其中一条；接收方原本能读到两条诊断（每条只出现一次）；
// 另有一份绑定目标授权、固化这两条记录创建时版本的待回执交换。撤回失败尝试与
// 后续成功提交都落在两条授权的有效期内，因此访问变化只能来自撤回结果本身。
//
// 本地保存失败时：撤回必须以普通保存错误返回，不能报告成功，也不能伪装成身份
// 无权、对象不存在或参数不合法；目标授权在内部查询中仍未撤回，原范围、有效期
// 与另一条限定授权保持不变；既有审计的内容与顺序原样保留，不新增撤回事件；
// 接收方仍能读到原来的两条诊断（标识、当前版本、完整内容与排列顺序不变），
// 仍能取得原交换包及其摘要。
//
// 保存条件恢复后：同一目标授权可正常撤回，撤回时间属于这次成功操作，只新增
// 一条指向目标授权、记录实际操作身份与时间的撤回审计；接收方随后只能读到
// 限定授权明确允许的那一条诊断，另一条诊断的标识与内容都不再出现；即使剩余
// 授权允许读取包内一部分记录，绑定授权已撤回的原交换取包仍返回 ErrAccessDenied
// 且不携带包内容或摘要；内部使用者仍能查看原交换，固化内容、摘要与待回执状态
// 不变——撤回本身不登记回执、不改写诊断历史。
func TestRevokeSaveFailureKeepsStateAndAllowsRetry(t *testing.T) {
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
	p, err := s.RegisterPatient(doc, "撤回保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 同一次就诊下两条已生效诊断（均为第 1 版）。
	activate := func(content string) (ID, Version) {
		t.Helper()
		r, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, content)
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.ActivateRecord(doc, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID, v
	}
	d1, d1v1 := activate("诊断一内容")
	d2, d2v1 := activate("诊断二内容")

	// 两条授权的时间窗覆盖失败尝试与成功提交的全部时刻：访问变化不能由
	// 未开始、到期造成。
	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(48 * time.Hour)
	// 目标授权：整类覆盖这次就诊的全部诊断（d1 与 d2）。
	target, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	// 另一条限定授权：只明确选出 d1。
	limited, err := s.GrantSelective(doc, p.ID, rcv.ID, nil,
		[]RecordSelection{sel(e.ID, Diagnosis, d1)}, start, end)
	if err != nil {
		t.Fatal(err)
	}

	// 一份绑定目标授权、包含两条记录创建时版本（均为 v1）的待回执交换。
	exch, err := s.CreateExchange(doc, p.ID, rcv.ID, target.ID, []ID{d1, d2}, "req-revoke-save-failure")
	if err != nil {
		t.Fatal(err)
	}
	if exch.Status != ExchangePending || exch.Receipt != nil {
		t.Fatalf("baseline exchange should be pending without receipt: %+v", exch)
	}

	// ---- 提交前的正式基线 ----
	authsBefore, err := s.ListAuthorizations(doc, p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(authsBefore) != 2 {
		t.Fatalf("baseline authorizations = %+v, want 2", authsBefore)
	}
	targetBefore, err := s.GetAuthorization(doc, p.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	limitedBefore, err := s.GetAuthorization(doc, p.ID, limited.ID)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	revokesBefore := auditActions(auditBefore)[ActionRevoked]
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 接收方原本能读到两条诊断，每条只出现一次并按标识稳定排序。
	readBefore, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if len(readBefore.Records) != 2 {
		t.Fatalf("baseline read = %+v, want 2 diagnoses", readBefore)
	}
	baselineByID := map[ID]EffectiveRecord{}
	for _, rec := range readBefore.Records {
		if _, dup := baselineByID[rec.RecordID]; dup {
			t.Fatalf("baseline read lists a record twice: %+v", readBefore.Records)
		}
		baselineByID[rec.RecordID] = rec
	}
	if got := baselineByID[d1]; got.VersionID != d1v1.ID || got.Version != 1 || got.Content != "诊断一内容" {
		t.Fatalf("baseline d1 wrong: %+v", got)
	}
	if got := baselineByID[d2]; got.VersionID != d2v1.ID || got.Version != 1 || got.Content != "诊断二内容" {
		t.Fatalf("baseline d2 wrong: %+v", got)
	}
	if readBefore.Records[0].RecordID > readBefore.Records[1].RecordID {
		t.Fatalf("baseline read not in stable order: %+v", readBefore.Records)
	}
	// 接收方原本能取得原交换包及其摘要。
	deliveryBefore, err := s.FetchPackage(rcv, exch.ID)
	if err != nil {
		t.Fatalf("baseline fetch package: %v", err)
	}
	if deliveryBefore.Digest != exch.Digest || len(deliveryBefore.Package.Records) != 2 {
		t.Fatalf("baseline delivery wrong: %+v", deliveryBefore)
	}
	exchangeBefore, err := s.GetExchange(doc, p.ID, exch.ID)
	if err != nil {
		t.Fatal(err)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），授权撤回状态、审计、
	// 接收方可见内容与原交换包都与提交前逐项一致——撤回状态、撤回审计与
	// 接收方可见内容必须对应同一次成功操作，失败尝试不能提前阻断访问，
	// 也不能留下看起来已经成功的撤回事件。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		// 目标授权仍未撤回：原整类范围、时间窗保持原样。
		gotTarget, err := st.GetAuthorization(doc, p.ID, target.ID)
		if err != nil {
			t.Fatalf("%s: get target authorization: %v", label, err)
		}
		if !reflect.DeepEqual(gotTarget, targetBefore) {
			t.Fatalf("%s: target authorization changed:\nbefore: %+v\nafter:  %+v",
				label, targetBefore, gotTarget)
		}
		if gotTarget.RevokedAt != nil {
			t.Fatalf("%s: target authorization must not be revoked: %+v", label, gotTarget)
		}
		if !gotTarget.ActiveAt(clk.t) {
			t.Fatalf("%s: target authorization must stay active at %v: %+v", label, clk.t, gotTarget)
		}
		// 另一条限定授权同样保持不变且仍在有效期内。
		gotLimited, err := st.GetAuthorization(doc, p.ID, limited.ID)
		if err != nil {
			t.Fatalf("%s: get limited authorization: %v", label, err)
		}
		if !reflect.DeepEqual(gotLimited, limitedBefore) {
			t.Fatalf("%s: limited authorization changed:\nbefore: %+v\nafter:  %+v",
				label, limitedBefore, gotLimited)
		}
		if !gotLimited.ActiveAt(clk.t) {
			t.Fatalf("%s: limited authorization must stay active at %v: %+v", label, clk.t, gotLimited)
		}
		if auths, err := st.ListAuthorizations(doc, p.ID, ""); err != nil ||
			!reflect.DeepEqual(auths, authsBefore) {
			t.Fatalf("%s: authorization list changed:\nbefore: %+v\nafter:  %+v err=%v",
				label, authsBefore, auths, err)
		}

		// 既有审计的内容与顺序原样保留，不新增撤回事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		if got := auditActions(audit)[ActionRevoked]; got != revokesBefore {
			t.Fatalf("%s: authorization_revoked events = %d, want %d", label, got, revokesBefore)
		}

		// 患者档案未停用。
		patient, err := st.GetPatient(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: get patient: %v", label, err)
		}
		if patient.Deactivated {
			t.Fatalf("%s: patient must not be deactivated", label)
		}

		// 接收方仍能读到原来的两条诊断：标识、当前版本、完整内容与顺序不变。
		res, err := st.Read(rcv, p.ID, e.ID, Diagnosis)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}

		// 接收方仍能取得原交换包及其摘要。
		delivery, err := st.FetchPackage(rcv, exch.ID)
		if err != nil {
			t.Fatalf("%s: receiver fetch package: %v", label, err)
		}
		if !reflect.DeepEqual(delivery, deliveryBefore) {
			t.Fatalf("%s: package delivery changed:\nbefore: %+v\nafter:  %+v",
				label, deliveryBefore, delivery)
		}

		// 内部使用者看到的原交换固化内容、摘要与待回执状态不变。
		gotExch, err := st.GetExchange(doc, p.ID, exch.ID)
		if err != nil {
			t.Fatalf("%s: internal get exchange: %v", label, err)
		}
		assertExchangeMatches(t, gotExch, exchangeBefore, label+": exchange")
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、对象存在性或参数合法性无关。原数据文件先挪到旁边，事后还原。
	clk.t = clk.t.Add(time.Hour)
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 撤回本身完全合法，仅本地保存失败：必须返回保存错误，不能报告成功，
	// 也不能误报为身份无权、对象不存在、跨患者、档案停用或参数不合法。
	if err := s.Revoke(doc, p.ID, target.ID); err == nil {
		t.Fatal("revoke must fail when local save fails")
	} else if errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrMismatchedPatient) || errors.Is(err, ErrInvalidArgument) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：撤回状态、审计、接收方可见内容与交换包全部维持提交前状态。
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
	// 重开后仍是提交前的正式数据：失败的撤回不留任何痕迹。
	checkIntact("after reopen", s2)

	// 保存条件恢复后：同一目标授权正常撤回。时刻仍在两条授权的有效期内、
	// 患者仍未停用，因此随后的访问变化只能来自这次成功撤回。
	clk.t = clk.t.Add(time.Hour)
	if err := s2.Revoke(doc, p.ID, target.ID); err != nil {
		t.Fatalf("retry after save recovered: %v", err)
	}

	// 目标授权已撤回，撤回时间属于这次成功操作；范围与有效期原样保留。
	gotTarget, err := s2.GetAuthorization(doc, p.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget.RevokedAt == nil || !gotTarget.RevokedAt.Equal(clk.t) {
		t.Fatalf("target revocation time = %v, want %v", gotTarget.RevokedAt, clk.t)
	}
	if !reflect.DeepEqual(gotTarget.Scopes, targetBefore.Scopes) ||
		!reflect.DeepEqual(gotTarget.Selections, targetBefore.Selections) ||
		!gotTarget.StartsAt.Equal(targetBefore.StartsAt) ||
		!gotTarget.ExpiresAt.Equal(targetBefore.ExpiresAt) {
		t.Fatalf("revoked authorization scope/window changed:\nbefore: %+v\nafter:  %+v",
			targetBefore, gotTarget)
	}
	// 另一条限定授权保持原样且仍在有效期内。
	gotLimited, err := s2.GetAuthorization(doc, p.ID, limited.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotLimited, limitedBefore) {
		t.Fatalf("limited authorization changed after revoke:\nbefore: %+v\nafter:  %+v",
			limitedBefore, gotLimited)
	}

	// 审计相对提交前基线只新增一条撤回事件，指向目标授权并记录实际操作
	// 身份与时间；既有事件的内容与顺序原样保留。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after successful revoke:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionRevoked || last.ObjectType != "authorization" ||
		last.ObjectID != target.ID || last.ActorID != doc.ID ||
		last.PatientID != p.ID || !last.OccurredAt.Equal(clk.t) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if got := auditActions(audit)[ActionRevoked]; got != revokesBefore+1 {
		t.Fatalf("authorization_revoked events = %d, want %d", got, revokesBefore+1)
	}

	// 接收方随后只能读到另一条限定授权明确允许的 d1；d2 的标识与内容都
	// 不再出现在读取结果中。
	res, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("receiver read after successful revoke: %v", err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("read after revoke = %+v, want only the selected diagnosis", res.Records)
	}
	only := res.Records[0]
	if only.RecordID != d1 || only.VersionID != d1v1.ID ||
		only.Version != 1 || only.Content != "诊断一内容" {
		t.Fatalf("remaining visible diagnosis wrong: %+v", only)
	}
	for _, rec := range res.Records {
		if rec.RecordID == d2 {
			t.Fatalf("d2 record id must disappear after revoke: %+v", rec)
		}
		if rec.Content == "诊断二内容" {
			t.Fatalf("d2 content must disappear after revoke: %+v", rec)
		}
	}

	// 即使剩余的限定授权允许读取包内的一部分记录（d1），原交换绑定的目标
	// 授权已撤回：取包必须 ErrAccessDenied，响应不携带包内容或摘要。
	if delivery, err := s2.FetchPackage(rcv, exch.ID); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("fetch after bound auth revoked: delivery=%+v err=%v, want ErrAccessDenied",
			delivery, err)
	} else if delivery.Digest != "" || len(delivery.Package.Records) != 0 ||
		delivery.ExchangeID != "" || delivery.Status != "" {
		t.Fatalf("denied fetch must carry no package content or digest: %+v", delivery)
	}

	// 内部使用者仍能查看原交换：固化内容（两条记录的创建时 v1）、摘要与
	// 待回执状态保持不变；撤回本身不登记回执。
	gotExch, err := s2.GetExchange(doc, p.ID, exch.ID)
	if err != nil {
		t.Fatalf("internal get exchange after revoke: %v", err)
	}
	assertExchangeMatches(t, gotExch, exchangeBefore, "exchange after revoke")
	if gotExch.Status != ExchangePending || gotExch.Receipt != nil {
		t.Fatalf("revoke must not register a receipt or change status: %+v", gotExch)
	}
	if len(gotExch.Package.Records) != 2 || gotExch.Digest != exchangeBefore.Digest {
		t.Fatalf("frozen package/digest changed after revoke: %+v", gotExch)
	}
	pkgByID := recordsByID(gotExch.Package.Records)
	if pr := pkgByID[d1]; pr.VersionID != d1v1.ID || pr.Content != "诊断一内容" {
		t.Fatalf("frozen d1 wrong: %+v", pr)
	}
	if pr := pkgByID[d2]; pr.VersionID != d2v1.ID || pr.Content != "诊断二内容" {
		t.Fatalf("frozen d2 wrong: %+v", pr)
	}

	// 撤回不改写诊断历史：两条诊断的版本链与当前版本与提交前完全一致。
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(chart.Records, chartBefore.Records) {
		t.Fatalf("diagnosis history changed by revoke:\nbefore: %+v\nafter:  %+v",
			chartBefore.Records, chart.Records)
	}

	// 重复撤回幂等：不产生额外变化，也不新增审计事件（沿用既有撤回规则）。
	if err := s2.Revoke(doc, p.ID, gotTarget.ID); err != nil {
		t.Fatalf("repeat revoke must stay successful: %v", err)
	}
	again, err := s2.GetAuthorization(doc, p.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.RevokedAt == nil || !again.RevokedAt.Equal(clk.t) {
		t.Fatalf("repeat revoke altered revocation time: %v", again.RevokedAt)
	}
	if evs, err := s2.AuditEvents(doc, p.ID); err != nil || len(evs) != len(audit) {
		t.Fatalf("repeat revoke added audit events: before=%d after=%d err=%v",
			len(audit), len(evs), err)
	}

	// 关闭后从同一数据位置重新打开：成功撤回与其唯一审计完整保留，失败尝试
	// 不能作为额外的撤回状态或事件出现。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })

	reloaded, err := s3.GetAuthorization(doc, p.ID, target.ID)
	if err != nil {
		t.Fatalf("get revoked authorization after reopen: %v", err)
	}
	if reloaded.RevokedAt == nil || !reloaded.RevokedAt.Equal(clk.t) {
		t.Fatalf("revocation not persisted after reopen: %+v", reloaded)
	}
	if delivery, err := s3.FetchPackage(rcv, exch.ID); !errors.Is(err, ErrAccessDenied) ||
		delivery.Digest != "" || len(delivery.Package.Records) != 0 {
		t.Fatalf("fetch after reopen: delivery=%+v err=%v, want ErrAccessDenied without content",
			delivery, err)
	}
	reloadedExch, err := s3.GetExchange(doc, p.ID, exch.ID)
	if err != nil {
		t.Fatalf("get exchange after reopen: %v", err)
	}
	assertExchangeMatches(t, reloadedExch, exchangeBefore, "exchange after reopen")

	var revokeIDs []ID
	for _, ev := range mustAudit(t, s3, p.ID) {
		if ev.Action == ActionRevoked {
			revokeIDs = append(revokeIDs, ev.ObjectID)
		}
	}
	if !reflect.DeepEqual(revokeIDs, []ID{target.ID}) {
		t.Fatalf("revocation events after reopen = %v, want only [%q]", revokeIDs, target.ID)
	}
}
