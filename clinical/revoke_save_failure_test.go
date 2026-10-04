package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestRevokeSaveFailureKeepsStateAndAllowsRetry 覆盖“内部使用者身份合法、患者
// 未停用、目标授权存在且属于该患者、操作时间在两条授权的有效期内，但本地保存
// 失败”的授权撤回场景。
//
// 场景中同一接收方有两条当前有效的授权：目标授权按整类范围覆盖本次就诊的全部
// 诊断，另一条限定授权只覆盖其中一条；另有一份绑定目标授权、包含两条诊断创建
// 时版本的待回执交换。保存失败必须以普通错误返回（不能报告成功，也不能伪装成
// 身份无权、对象不存在或参数不合法等业务错误），且授权的撤回状态、审计与接收
// 方可见内容全部维持提交前状态——不允许出现授权已撤回但没有审计、或只有审计
// 而授权未撤回的半成品，也不能提前阻断原本允许的访问。保存条件恢复后，同一目
// 标授权必须能正常撤回：撤回时间属于这次成功操作，只新增一条撤回审计；接收方
// 随后只能读到限定授权明确允许的诊断，绑定授权已撤回的交换对接收方拒绝取包但
// 内部视图原样保留。
func TestRevokeSaveFailureKeepsStateAndAllowsRetry(t *testing.T) {
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
	p, err := s.RegisterPatient(doc, "撤回保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 同一次就诊下两条已生效诊断。
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

	// 同一接收方的两条当前有效授权：目标授权整类覆盖本次就诊的全部诊断，
	// 另一条限定授权只覆盖第一条诊断。撤回尝试与后续成功提交都在此窗口内。
	start := clk.t.Add(-time.Hour)
	end := clk.t.Add(48 * time.Hour)
	target, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	limited, err := s.GrantSelective(doc, p.ID, rcv.ID, nil,
		[]RecordSelection{sel(e.ID, Diagnosis, d1)}, start, end)
	if err != nil {
		t.Fatal(err)
	}

	// 绑定目标授权的待回执交换，固化两条记录创建时的版本。
	x, err := s.CreateExchange(doc, p.ID, rcv.ID, target.ID, []ID{d1, d2}, "req-revoke-save-failure")
	if err != nil {
		t.Fatal(err)
	}
	if x.Status != ExchangePending || len(x.Package.Records) != 2 {
		t.Fatalf("unexpected new exchange: %+v", x)
	}

	// 提交前的正式基线：两条授权、授权列表、审计、接收方读取结果、取包视图、
	// 内部交换视图与完整档案。
	targetBefore, err := s.GetAuthorization(doc, p.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	limitedBefore, err := s.GetAuthorization(doc, p.ID, limited.ID)
	if err != nil {
		t.Fatal(err)
	}
	authsBefore, err := s.ListAuthorizations(doc, p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(authsBefore) != 2 {
		t.Fatalf("unexpected baseline authorizations: %+v", authsBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore)[ActionRevoked]; got != 0 {
		t.Fatalf("baseline already has %d revoke events", got)
	}
	readBefore, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	// 接收方原本能读到两条诊断，每条只出现一次，按记录标识稳定排序。
	if len(readBefore.Records) != 2 ||
		readBefore.Records[0].RecordID == readBefore.Records[1].RecordID {
		t.Fatalf("baseline read must contain each diagnosis once: %+v", readBefore)
	}
	readByID := map[ID]EffectiveRecord{}
	for _, rec := range readBefore.Records {
		readByID[rec.RecordID] = rec
	}
	if got := readByID[d1]; got.VersionID != d1v1.ID || got.Content != "诊断一内容" {
		t.Fatalf("baseline diagnosis d1 wrong: %+v", got)
	}
	if got := readByID[d2]; got.VersionID != d2v1.ID || got.Content != "诊断二内容" {
		t.Fatalf("baseline diagnosis d2 wrong: %+v", got)
	}
	deliveryBefore, err := s.FetchPackage(rcv, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deliveryBefore.Digest != x.Digest || deliveryBefore.Status != ExchangePending ||
		len(deliveryBefore.Package.Records) != 2 {
		t.Fatalf("unexpected baseline delivery: %+v", deliveryBefore)
	}
	xBefore, err := s.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），授权、审计、接收方可见
	// 内容与既有交换全部维持提交前状态——目标授权未撤回、范围与有效期不变，
	// 限定授权不变，没有撤回事件，接收方仍能读到原来的两条诊断并取得原交换包。
	checkIntact := func(label string, st *Store) {
		t.Helper()

		gotTarget, err := st.GetAuthorization(doc, p.ID, target.ID)
		if err != nil {
			t.Fatalf("%s: get target authorization: %v", label, err)
		}
		if gotTarget.RevokedAt != nil {
			t.Fatalf("%s: target authorization revoked by failed save: %+v", label, gotTarget)
		}
		if !reflect.DeepEqual(gotTarget, targetBefore) {
			t.Fatalf("%s: target authorization changed:\nbefore: %+v\nafter:  %+v", label, targetBefore, gotTarget)
		}
		gotLimited, err := st.GetAuthorization(doc, p.ID, limited.ID)
		if err != nil {
			t.Fatalf("%s: get limited authorization: %v", label, err)
		}
		if !reflect.DeepEqual(gotLimited, limitedBefore) {
			t.Fatalf("%s: limited authorization changed:\nbefore: %+v\nafter:  %+v", label, limitedBefore, gotLimited)
		}
		auths, err := st.ListAuthorizations(doc, p.ID, "")
		if err != nil {
			t.Fatalf("%s: list authorizations: %v", label, err)
		}
		if !reflect.DeepEqual(auths, authsBefore) {
			t.Fatalf("%s: authorization list changed:\nbefore: %+v\nafter:  %+v", label, authsBefore, auths)
		}

		// 既有审计的内容与顺序保持原样，不新增撤回事件。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}
		if got := auditActions(audit)[ActionRevoked]; got != 0 {
			t.Fatalf("%s: failed save left %d revoke events", label, got)
		}

		// 接收方仍能读到原来的两条诊断：标识、当前版本、完整内容与排列
		// 顺序不变，仍能取得原交换包及其摘要。
		res, err := st.Read(rcv, p.ID, e.ID, Diagnosis)
		if err != nil {
			t.Fatalf("%s: receiver read: %v", label, err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", label, readBefore, res)
		}
		delivery, err := st.FetchPackage(rcv, x.ID)
		if err != nil {
			t.Fatalf("%s: receiver fetch package: %v", label, err)
		}
		if !reflect.DeepEqual(delivery, deliveryBefore) {
			t.Fatalf("%s: package delivery changed:\nbefore: %+v\nafter:  %+v", label, deliveryBefore, delivery)
		}

		// 内部视图中的原交换与诊断历史不变。
		gotX, err := st.GetExchange(doc, p.ID, x.ID)
		if err != nil {
			t.Fatalf("%s: get exchange: %v", label, err)
		}
		assertExchangeMatches(t, gotX, xBefore, label+": exchange")
		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		if !reflect.DeepEqual(chart.Records, chartBefore.Records) {
			t.Fatalf("%s: clinical records changed:\nbefore: %+v\nafter:  %+v",
				label, chartBefore.Records, chart.Records)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、授权归属等业务校验无关。原数据文件先挪到旁边，事后原样还原。
	failedAt := clk.t
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 身份合法、患者未停用、授权存在且属于该患者，仅本地保存失败：必须明确
	// 报错，且不能伪装成身份无权、对象不存在或参数不合法等业务结果。
	err = s.Revoke(doc, p.ID, target.ID)
	if err == nil {
		t.Fatal("revoke must fail when local save fails")
	}
	if errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrMismatchedPatient) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：授权、审计、接收方可见内容与既有交换全部维持提交前状态。
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
	// 重开后仍是提交前的正式数据：没有撤回结果，也没有撤回事件。
	checkIntact("after reopen", s2)

	// 保存条件恢复后：同一目标授权能够正常撤回。操作时间仍在两条授权的
	// 有效期内，患者未停用——访问变化只能来自撤回结果本身。
	clk.t = clk.t.Add(time.Hour)
	if err := s2.Revoke(doc, p.ID, target.ID); err != nil {
		t.Fatalf("revoke after save recovered: %v", err)
	}

	// 目标授权的撤回时间属于这次成功操作（而非失败尝试的时刻），原来的
	// 范围与有效期不变；另一条限定授权保持原样、未受牵连。
	gotTarget, err := s2.GetAuthorization(doc, p.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget.RevokedAt == nil || !gotTarget.RevokedAt.Equal(clk.t) {
		t.Fatalf("revoked-at = %v, want %v (the successful operation)", gotTarget.RevokedAt, clk.t)
	}
	if gotTarget.RevokedAt.Equal(failedAt) {
		t.Fatalf("revoked-at %v must not be the failed attempt's time", gotTarget.RevokedAt)
	}
	wantTarget := targetBefore
	wantTarget.RevokedAt = gotTarget.RevokedAt
	if !reflect.DeepEqual(gotTarget, wantTarget) {
		t.Fatalf("target authorization changed beyond revocation:\nbefore: %+v\nafter:  %+v", targetBefore, gotTarget)
	}
	gotLimited, err := s2.GetAuthorization(doc, p.ID, limited.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotLimited, limitedBefore) {
		t.Fatalf("limited authorization changed by revoking the other:\nbefore: %+v\nafter:  %+v",
			limitedBefore, gotLimited)
	}

	// 审计相对提交前基线只新增一条对应目标授权的撤回事件，记录实际操作
	// 身份与时间；既有事件的内容与顺序原样保留。
	audit, err := s2.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after revoke:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionRevoked || last.ObjectType != "authorization" ||
		last.ObjectID != target.ID || last.ActorID != doc.ID ||
		last.PatientID != p.ID || !last.OccurredAt.Equal(clk.t) {
		t.Fatalf("unexpected new audit event: %+v", last)
	}
	if got := auditActions(audit)[ActionRevoked]; got != 1 {
		t.Fatalf("authorization_revoked events = %d, want 1", got)
	}

	// 接收方随后只能读到限定授权明确允许的第一条诊断；另一条诊断的标识
	// 与内容都不再出现。
	res, err := s2.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatalf("receiver read after revoke: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != d1 ||
		res.Records[0].VersionID != d1v1.ID || res.Records[0].Content != "诊断一内容" {
		t.Fatalf("receiver read after revoke = %+v, want only d1", res)
	}
	if readRecordIDs(res)[d2] {
		t.Fatal("diagnosis d2 must not be visible after its covering authorization was revoked")
	}

	// 即使限定授权仍允许读取包内的 d1，原交换因绑定授权已撤回而拒绝取包，
	// 响应不携带包内容或摘要。
	delivery, err := s2.FetchPackage(rcv, x.ID)
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("fetch package after revoke err = %v, want ErrAccessDenied", err)
	}
	if !reflect.DeepEqual(delivery, PackageDelivery{}) {
		t.Fatalf("denied fetch must not carry package or digest, got %+v", delivery)
	}

	// 内部使用者仍能查看原交换：固化内容、摘要与待回执状态保持不变；
	// 撤回本身不登记回执，也不改写诊断历史。
	gotX, err := s2.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, gotX, xBefore, "exchange after revoke")
	if gotX.Status != ExchangePending || gotX.Receipt != nil {
		t.Fatalf("revoke must not register a receipt: status=%q receipt=%+v", gotX.Status, gotX.Receipt)
	}
	chart, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(chart.Records, chartBefore.Records) {
		t.Fatalf("revoke rewrote diagnosis history:\nbefore: %+v\nafter:  %+v",
			chartBefore.Records, chart.Records)
	}

	// 关闭后从同一数据位置重新打开：撤回结果与其审计完整保留，接收方访问
	// 与取包拒绝维持撤回后的状态。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })

	gotTarget3, err := s3.GetAuthorization(doc, p.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotTarget3, gotTarget) {
		t.Fatalf("revoked target authorization changed across reopen:\nbefore: %+v\nafter:  %+v",
			gotTarget, gotTarget3)
	}
	audit3, err := s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit3, audit) {
		t.Fatalf("audit changed across reopen:\nbefore: %+v\nafter:  %+v", audit, audit3)
	}
	res3, err := s3.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res3, res) {
		t.Fatalf("receiver read changed across reopen:\nbefore: %+v\nafter:  %+v", res, res3)
	}
	if delivery, err := s3.FetchPackage(rcv, x.ID); !errors.Is(err, ErrAccessDenied) ||
		!reflect.DeepEqual(delivery, PackageDelivery{}) {
		t.Fatalf("fetch package after reopen: res=%+v err=%v, want ErrAccessDenied without content",
			delivery, err)
	}
	gotX3, err := s3.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertExchangeMatches(t, gotX3, xBefore, "exchange after reopen")
}
