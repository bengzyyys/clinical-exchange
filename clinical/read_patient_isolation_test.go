package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件为接收方 Read 查阅入口补“同一接收方同时持有多名患者有效授权”时的
// 患者隔离回归保障：每次读取只能使用所请求患者自己的授权，不能因为接收方
// 相同、记录类别相同，或另一名患者拥有更宽的整类范围，就让本次读取多出内容。
//
// 覆盖的约定：
//   - 同一份本地存储中两名合成患者各自有就诊、已生效诊断与未生效草稿；同一
//     接收方对甲只有选定诊断的限定授权，对乙有诊断整类授权，两份授权同时
//     有效。读甲的就诊只能看到明确选中的诊断（同类未选中者、授权后新增者、
//     草稿都不出现）；读乙的就诊能看到乙在该次就诊中的全部已生效诊断
//     （含授权后才生效者），草稿不出现；两边都不混入对方的记录；
//   - 返回的版本标识、版本号、正文与生效时间准确对应获准记录的当前生效
//     版本：已更正记录不退回旧版，也不带出更正原因；
//   - 患者与就诊必须属于同一份档案：甲的患者标识配乙的就诊标识（以及反向
//     混用）都返回 ErrAccessDenied 与全空结果，不返回内部查询使用的
//     ErrMismatchedPatient，也不能借另一份有效授权把混用请求当作合法读取；
//   - 格式合法但不存在的患者或就诊标识同样返回 ErrAccessDenied 与全空结果，
//     接收方无法通过错误种类或附带资料区分“不存在”与“无权访问”；
//   - 正常读取与被拒绝的读取都是只读的：不新增审计、不改授权、不生成记录
//     版本；之后用原本正确的患者与就诊组合再读，获准内容保持原样。

// countVersions 统计库内记录版本总数，用于核对读取不产生新版本。
func countVersions(s *Store) int {
	c := 0
	_ = s.view(func(snap *snapshot) error { c = len(snap.Versions); return nil })
	return c
}

// assertReadDeniedBlank 核对一次被拒绝的查阅：错误是且仅是 ErrAccessDenied
// （不能是内部查询使用的 ErrMismatchedPatient，也不能是泄露存在性的
// ErrNotFound），返回结果全空——不携带就诊、类别、记录标识、数量或正文。
func assertReadDeniedBlank(t *testing.T, err error, res ReadResult, label string) {
	t.Helper()
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("%s: err = %v, want ErrAccessDenied", label, err)
	}
	if errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("%s: err = %v, must not surface internal cross-patient error", label, err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("%s: err = %v, must not reveal existence", label, err)
	}
	if !reflect.DeepEqual(res, ReadResult{}) {
		t.Fatalf("%s: denied read must carry no encounter/category/record data, got %+v", label, res)
	}
}

// assertEffectiveRecord 核对一条查阅结果准确对应某条记录的当前生效版本：
// 记录标识、版本标识、版本号、正文与生效时间逐项一致。
func assertEffectiveRecord(t *testing.T, got EffectiveRecord, recordID ID, want Version, label string) {
	t.Helper()
	if got.RecordID != recordID {
		t.Fatalf("%s: record id = %q, want %q", label, got.RecordID, recordID)
	}
	if got.VersionID != want.ID {
		t.Fatalf("%s: version id = %q, want current effective %q", label, got.VersionID, want.ID)
	}
	if got.Version != want.Number {
		t.Fatalf("%s: version = %d, want %d", label, got.Version, want.Number)
	}
	if got.Content != want.Content {
		t.Fatalf("%s: content = %q, want %q", label, got.Content, want.Content)
	}
	if !got.EffectiveAt.Equal(want.CreatedAt) {
		t.Fatalf("%s: effective at = %v, want %v", label, got.EffectiveAt, want.CreatedAt)
	}
}

func TestReadIsolatesPatientsSharingOneReceiver(t *testing.T) {
	s, clk := newTestStore(t)
	t0 := clk.t // 2026-01-01 09:00 UTC

	// 同一份本地存储中的两名合成患者，各有一次就诊。
	pa, err := s.RegisterPatient(doc, "合成患者甲")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := s.RegisterPatient(doc2, "合成患者乙")
	if err != nil {
		t.Fatal(err)
	}
	a1, err := s.AddEncounter(doc, pa.ID, t0)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := s.AddEncounter(doc2, pb.ID, t0)
	if err != nil {
		t.Fatal(err)
	}

	// 每条诊断的标识、正文与版本信息都逐条可区分归属；更正原因同样专有，
	// 便于发现旧版回退、原因外泄与跨患者串台。
	const (
		aSelV1Text = "甲-选中诊断 v1：上呼吸道感染"
		aSelV2Text = "甲-选中诊断 v2：急性鼻咽炎（更正后）"
		aSelReason = "甲-选中诊断的更正原因"
		aOtherText = "甲-未选中诊断：过敏性鼻炎"
		aDraftText = "甲-未生效草稿：头晕待查"
		aLateText  = "甲-授权后新增诊断：急性咽炎"
		bOneV1Text = "乙-诊断一 v1：高血压"
		bOneV2Text = "乙-诊断一 v2：高血压 2 级（更正后）"
		bOneReason = "乙-诊断一的更正原因"
		bTwoText   = "乙-诊断二：2 型糖尿病"
		bDraftText = "乙-未生效草稿：失眠待查"
		bLateText  = "乙-授权后新增诊断：高脂血症"
	)

	// 甲：两条将生效的诊断（一条将被选中、一条同类但不选中）+ 一条永不生效的草稿。
	aSel := mustCreateDraft(t, s, pa.ID, a1.ID, Diagnosis, aSelV1Text)
	aOther := mustCreateDraft(t, s, pa.ID, a1.ID, Diagnosis, aOtherText)
	aDraft := mustCreateDraft(t, s, pa.ID, a1.ID, Diagnosis, aDraftText)
	// 乙：两条将生效的诊断 + 一条永不生效的草稿。
	bOne := mustCreateDraft(t, s, pb.ID, b1.ID, Diagnosis, bOneV1Text)
	bTwo := mustCreateDraft(t, s, pb.ID, b1.ID, Diagnosis, bTwoText)
	bDraft := mustCreateDraft(t, s, pb.ID, b1.ID, Diagnosis, bDraftText)

	clk.t = t0.Add(time.Hour) // t1：第一批生效
	aSelV1 := activateAs(t, s, doc, aSel)
	aOtherV := activateAs(t, s, doc, aOther)
	bTwoV := activateAs(t, s, doc2, bTwo)

	clk.t = clk.t.Add(time.Hour) // t2：第二批生效
	bOneV1 := activateAs(t, s, doc2, bOne)

	clk.t = clk.t.Add(time.Hour) // t3：两名患者各有一条诊断被更正到 v2
	aSelV2 := correctAs(t, s, doc, aSel, 1, aSelV2Text, aSelReason)
	bOneV2 := correctAs(t, s, doc2, bOne, 1, bOneV2Text, bOneReason)

	clk.t = clk.t.Add(time.Hour) // t4：两份授权同时建立、同时有效
	grantStart := clk.t.Add(-time.Hour)
	grantEnd := clk.t.Add(48 * time.Hour)
	// 甲：只选中 aSel 的限定授权（同类其他记录不在范围内）。
	grantA, err := s.GrantSelective(doc, pa.ID, rcv.ID, nil,
		[]RecordSelection{sel(a1.ID, Diagnosis, aSel)}, grantStart, grantEnd)
	if err != nil {
		t.Fatalf("selective grant for A: %v", err)
	}
	// 乙：诊断整类授权（该就诊下全部已生效诊断，含授权后才生效者）。
	grantB := grantAs(t, s, doc2, pb.ID, rcv.ID,
		[]Scope{{EncounterID: b1.ID, Category: Diagnosis}}, grantStart, grantEnd)

	clk.t = clk.t.Add(time.Hour) // t5：授权建立后两名患者各新增一条生效诊断
	aLate := mustCreateDraft(t, s, pa.ID, a1.ID, Diagnosis, aLateText)
	aLateV := activateAs(t, s, doc, aLate)
	bLate := mustCreateDraft(t, s, pb.ID, b1.ID, Diagnosis, bLateText)
	bLateV := activateAs(t, s, doc2, bLate)

	clk.t = clk.t.Add(time.Hour) // t6：查阅时刻，两份授权都在有效期内

	// 读取前的只读基线：审计、授权、记录与版本总量。
	auditsABefore := mustAudit(t, s, pa.ID)
	auditsBBefore := mustAudit(t, s, pb.ID)
	authsABefore, err := s.ListAuthorizations(doc, pa.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	authsBBefore, err := s.ListAuthorizations(doc2, pb.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	versionsBefore := countVersions(s)
	recordsBefore := countRecords(s)

	// ---- 读甲：只能看到明确选中的诊断，且取更正后的当前生效版本 ----

	resA, err := s.Read(rcv, pa.ID, a1.ID, Diagnosis)
	if err != nil {
		t.Fatalf("read A: %v", err)
	}
	if resA.EncounterID != a1.ID || resA.Category != Diagnosis {
		t.Fatalf("read A echo wrong: %+v", resA)
	}
	if len(resA.Records) != 1 {
		t.Fatalf("read A records = %d (%v), want exactly the selected diagnosis",
			len(resA.Records), readRecordIDs(resA))
	}
	selRec := resA.Records[0]
	// 已更正的选中记录：必须是 v2，不能退回 v1，也不能带出更正原因。
	assertEffectiveRecord(t, selRec, aSel, aSelV2, "read A selected diagnosis")
	if selRec.VersionID == aSelV1.ID || selRec.Content == aSelV1Text {
		t.Fatalf("read A reverted to the pre-correction version: %+v", selRec)
	}
	if selRec.Content == aSelReason {
		t.Fatalf("read A leaked the correction reason as content: %+v", selRec)
	}
	// 同类但未选中的记录、授权后新增的记录、草稿都不能因限定授权之外的理由出现。
	for _, leaked := range []ID{aOther, aLate, aDraft} {
		if readRecordIDs(resA)[leaked] {
			t.Fatalf("read A must not contain unselected/draft record %q: %+v", leaked, resA.Records)
		}
	}
	// 乙的整类授权更宽，但甲的读取不能因此多出乙的任何记录或正文。
	for _, er := range resA.Records {
		if er.RecordID == bOne || er.RecordID == bTwo || er.RecordID == bLate || er.RecordID == bDraft {
			t.Fatalf("patient B record %q leaked into read A", er.RecordID)
		}
		if er.Content == bOneV1Text || er.Content == bOneV2Text || er.Content == bTwoText ||
			er.Content == bLateText || er.Content == bDraftText {
			t.Fatalf("patient B content leaked into read A: %q", er.Content)
		}
	}

	// ---- 读乙：整类授权覆盖该次就诊中的全部已生效诊断（含授权后生效者） ----

	resB, err := s.Read(rcv, pb.ID, b1.ID, Diagnosis)
	if err != nil {
		t.Fatalf("read B: %v", err)
	}
	if resB.EncounterID != b1.ID || resB.Category != Diagnosis {
		t.Fatalf("read B echo wrong: %+v", resB)
	}
	wantB := map[ID]Version{bOne: bOneV2, bTwo: bTwoV, bLate: bLateV}
	if len(resB.Records) != len(wantB) {
		t.Fatalf("read B records = %d (%v), want all %d effective diagnoses of encounter B1",
			len(resB.Records), readRecordIDs(resB), len(wantB))
	}
	for i := 1; i < len(resB.Records); i++ {
		if resB.Records[i-1].RecordID > resB.Records[i].RecordID {
			t.Fatalf("read B not in stable record-id order: %+v", resB.Records)
		}
	}
	for _, er := range resB.Records {
		want, ok := wantB[er.RecordID]
		if !ok {
			t.Fatalf("read B contains unexpected record %q (%q)", er.RecordID, er.Content)
		}
		assertEffectiveRecord(t, er, er.RecordID, want, "read B record "+er.RecordID)
	}
	// 乙的已更正诊断同样取当前版本 v2，不退回 v1、不带出更正原因。
	oneRec := resB.Records[0]
	for _, er := range resB.Records {
		if er.RecordID == bOne {
			oneRec = er
		}
	}
	if oneRec.VersionID == bOneV1.ID || oneRec.Content == bOneV1Text {
		t.Fatalf("read B reverted to the pre-correction version: %+v", oneRec)
	}
	if oneRec.Content == bOneReason {
		t.Fatalf("read B leaked the correction reason as content: %+v", oneRec)
	}
	// 乙的草稿与甲的任何记录都不能混入。
	for _, leaked := range []ID{bDraft, aSel, aOther, aLate, aDraft} {
		if readRecordIDs(resB)[leaked] {
			t.Fatalf("read B must not contain draft/foreign record %q: %+v", leaked, resB.Records)
		}
	}
	for _, er := range resB.Records {
		if er.Content == aSelV1Text || er.Content == aSelV2Text || er.Content == aOtherText ||
			er.Content == aLateText || er.Content == aDraftText {
			t.Fatalf("patient A content leaked into read B: %q", er.Content)
		}
	}

	// ---- 跨患者混用：患者标识与就诊标识必须属于同一份档案 ----
	//
	// 接收方对甲、乙分别持有有效授权，但两种混用都不能借另一份授权变成
	// 合法读取：统一返回 ErrAccessDenied 与全空结果，且不能返回内部查询
	// 使用的跨患者错误。

	mixAB, err := s.Read(rcv, pa.ID, b1.ID, Diagnosis)
	assertReadDeniedBlank(t, err, mixAB, "patient A id + patient B encounter")
	mixBA, err := s.Read(rcv, pb.ID, a1.ID, Diagnosis)
	assertReadDeniedBlank(t, err, mixBA, "patient B id + patient A encounter")

	// ---- 格式合法但不存在的标识：与无权访问不可区分 ----

	ghostPat := ID("pat_000000000000000000000000")
	ghostEnc := ID("enc_000000000000000000000000")
	deniedGhostPat, err := s.Read(rcv, ghostPat, a1.ID, Diagnosis)
	assertReadDeniedBlank(t, err, deniedGhostPat, "nonexistent patient")
	deniedGhostEnc, err := s.Read(rcv, pa.ID, ghostEnc, Diagnosis)
	assertReadDeniedBlank(t, err, deniedGhostEnc, "nonexistent encounter")
	deniedGhostBoth, err := s.Read(rcv, ghostPat, ghostEnc, Diagnosis)
	assertReadDeniedBlank(t, err, deniedGhostBoth, "nonexistent patient and encounter")
	// 不存在的标识与跨患者混用给出的错误种类完全一致，接收方无法据此分辨。
	if _, mixErr := s.Read(rcv, pa.ID, b1.ID, Diagnosis); !errors.Is(mixErr, ErrAccessDenied) ||
		errors.Is(mixErr, ErrNotFound) || errors.Is(mixErr, ErrMismatchedPatient) {
		t.Fatalf("cross-patient mix must be indistinguishable from nonexistent ids, err = %v", mixErr)
	}

	// ---- 正常读取与被拒绝的读取都是只读的：不新增审计、不改授权、不生成版本 ----

	assertAuditMatches(t, mustAudit(t, s, pa.ID), auditsABefore, "patient A audits after reads")
	assertAuditMatches(t, mustAudit(t, s, pb.ID), auditsBBefore, "patient B audits after reads")
	authsAAfter, err := s.ListAuthorizations(doc, pa.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(authsAAfter, authsABefore) {
		t.Fatalf("patient A authorizations changed as a result of reads:\n before=%+v\n after =%+v",
			authsABefore, authsAAfter)
	}
	authsBAfter, err := s.ListAuthorizations(doc2, pb.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(authsBAfter, authsBBefore) {
		t.Fatalf("patient B authorizations changed as a result of reads:\n before=%+v\n after =%+v",
			authsBBefore, authsBAfter)
	}
	if got := countVersions(s); got != versionsBefore {
		t.Fatalf("reads generated record versions: before=%d after=%d", versionsBefore, got)
	}
	if got := countRecords(s); got != recordsBefore {
		t.Fatalf("reads changed record count: before=%d after=%d", recordsBefore, got)
	}

	// ---- 之后用原本正确的患者与就诊组合再读：获准内容保持原样 ----

	againA, err := s.Read(rcv, pa.ID, a1.ID, Diagnosis)
	if err != nil {
		t.Fatalf("re-read A: %v", err)
	}
	if !reflect.DeepEqual(againA, resA) {
		t.Fatalf("read A changed after denied/mixed attempts:\n first=%+v\n again=%+v", resA, againA)
	}
	againB, err := s.Read(rcv, pb.ID, b1.ID, Diagnosis)
	if err != nil {
		t.Fatalf("re-read B: %v", err)
	}
	if !reflect.DeepEqual(againB, resB) {
		t.Fatalf("read B changed after denied/mixed attempts:\n first=%+v\n again=%+v", resB, againB)
	}

	// 防止 unused（授权标识是上面授权未变化断言的对象来源）。
	_ = grantA
	_ = grantB
	_ = aOtherV
	_ = aLateV
	_ = bLateV
}
