package clinical

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 本文件为接收方 Read 的“多患者同库”隔离约定补回归保障：同一接收方同时持有
// 多名患者的有效授权时，每次读取仍只能使用所请求患者自己的授权——不能因为
// 接收方相同、记录类别相同，或另一名患者拥有更宽的整类范围，就让本次读取多
// 出内容。
//
// 覆盖的约定：
//   - 同一存储中两名合成患者各有就诊、已生效诊断与未生效草稿；同一接收方对甲
//     只有选定诊断的限定授权、对乙有诊断整类授权，两份授权同时有效：读甲只见
//     明确选中的诊断（同类未选中者、授权后新增者、草稿都不出现），读乙见乙在
//     该次就诊中的全部已生效诊断（含授权后生效者，不含草稿）；两边都不混入
//     对方的记录；
//   - 返回的版本标识、版本号、正文与生效时间准确对应获准记录的当前生效版本：
//     已更正的记录不退回旧版，也不带出更正原因；
//   - 患者与就诊必须属于同一份档案：把甲的患者标识与乙的就诊标识组合（或反向）
//     一律 ErrAccessDenied，结果为空，不携带就诊、类别、记录标识、数量或正文；
//     不返回内部查询使用的跨患者错误，也不能借另一份有效授权把混用请求当作
//     合法读取；
//   - 格式合法但不存在的患者或就诊标识同样返回 ErrAccessDenied 与空结果，
//     接收方无法通过错误种类或附带资料区分“不存在”与“无权访问”；
//   - 正常读取与被拒绝的读取都不新增审计、不改动授权、不生成记录版本；之后再
//     用原本正确的患者与就诊组合读取，两名患者各自的获准内容保持原样。

// ---- 夹具：两名患者、同一接收方、两种授权形态 ----

type readIsolationFixture struct {
	s   *Store
	clk *fakeClock

	pidA, encA ID
	aSel1      ID // 甲：已生效且被明确选中的诊断（随后被更正）
	aSel2      ID // 甲：已生效且被明确选中的诊断
	aOther     ID // 甲：同类但未被选中的已生效诊断
	aLate      ID // 甲：授权建立后才生效的诊断（不进限定范围）
	aDraft     ID // 甲：未生效草稿

	pidB, encB ID
	bDiag1     ID // 乙：已生效诊断
	bDiag2     ID // 乙：已生效诊断（随后被更正）
	bLate      ID // 乙：授权建立后才生效的诊断（整类范围应包含）
	bDraft     ID // 乙：未生效草稿

	grantA Authorization // 甲：限定授权（只选中 aSel1、aSel2）
	grantB Authorization // 乙：整类授权（encB 全部诊断）

	// 各记录当前生效版本的预期值（以更正后为准）。
	wantVersion map[ID]Version
}

const (
	readIsoReasonA = "甲-更正原因-不应出现在查阅结果"
	readIsoReasonB = "乙-更正原因-不应出现在查阅结果"
)

func setupReadPatientIsolation(t *testing.T) readIsolationFixture {
	t.Helper()
	s, clk := newTestStore(t)

	pA, err := s.RegisterPatient(doc, "合成患者甲")
	if err != nil {
		t.Fatal(err)
	}
	encA, err := s.AddEncounter(doc, pA.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	pB, err := s.RegisterPatient(doc, "合成患者乙")
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	encB, err := s.AddEncounter(doc, pB.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	// 甲的就诊：两条将被选中的诊断、一条同类但不选中的诊断、一条草稿。
	aSel1 := mustCreateDraft(t, s, pA.ID, encA.ID, Diagnosis, "甲-选中诊断一-初版")
	aSel2 := mustCreateDraft(t, s, pA.ID, encA.ID, Diagnosis, "甲-选中诊断二")
	aOther := mustCreateDraft(t, s, pA.ID, encA.ID, Diagnosis, "甲-未选中诊断")
	aDraft := mustCreateDraft(t, s, pA.ID, encA.ID, Diagnosis, "甲-未生效草稿")
	// 乙的就诊：两条已生效诊断、一条草稿。
	bDiag1 := mustCreateDraft(t, s, pB.ID, encB.ID, Diagnosis, "乙-诊断一")
	bDiag2 := mustCreateDraft(t, s, pB.ID, encB.ID, Diagnosis, "乙-诊断二-初版")
	bDraft := mustCreateDraft(t, s, pB.ID, encB.ID, Diagnosis, "乙-未生效草稿")

	want := map[ID]Version{}
	want[aSel2] = mustActivate(t, s, aSel2)
	want[aOther] = mustActivate(t, s, aOther)
	want[bDiag1] = mustActivate(t, s, bDiag1)
	mustActivate(t, s, aSel1)
	mustActivate(t, s, bDiag2)

	// 各更正一条：查阅必须给出更正后的当前版本，不能退回初版或带出原因。
	clk.t = clk.t.Add(time.Hour)
	want[aSel1] = mustCorrect(t, s, aSel1, 1, "甲-选中诊断一-更正版", readIsoReasonA)
	want[bDiag2] = mustCorrect(t, s, bDiag2, 1, "乙-诊断二-更正版", readIsoReasonB)

	// 同一接收方：对甲只选中两条诊断；对乙整类放开 encB 的诊断。两份同时有效。
	start, end := clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)
	grantA, err := s.GrantSelective(doc, pA.ID, rcv.ID, nil,
		[]RecordSelection{
			sel(encA.ID, Diagnosis, aSel1),
			sel(encA.ID, Diagnosis, aSel2),
		}, start, end)
	if err != nil {
		t.Fatalf("grant selective for patient A: %v", err)
	}
	grantB, err := s.Grant(doc, pB.ID, rcv.ID,
		[]Scope{{EncounterID: encB.ID, Category: Diagnosis}}, start, end)
	if err != nil {
		t.Fatalf("grant whole-category for patient B: %v", err)
	}

	// 授权建立后才生效的记录：甲的不进限定范围，乙的自然进入整类范围。
	aLate := mustCreateDraft(t, s, pA.ID, encA.ID, Diagnosis, "甲-授权后新增诊断")
	want[aLate] = mustActivate(t, s, aLate)
	bLate := mustCreateDraft(t, s, pB.ID, encB.ID, Diagnosis, "乙-授权后新增诊断")
	want[bLate] = mustActivate(t, s, bLate)

	return readIsolationFixture{
		s: s, clk: clk,
		pidA: pA.ID, encA: encA.ID,
		aSel1: aSel1, aSel2: aSel2, aOther: aOther, aLate: aLate, aDraft: aDraft,
		pidB: pB.ID, encB: encB.ID,
		bDiag1: bDiag1, bDiag2: bDiag2, bLate: bLate, bDraft: bDraft,
		grantA: grantA, grantB: grantB,
		wantVersion: want,
	}
}

// assertEffectiveMatchesVersion 核对一条查阅结果准确对应获准记录的当前生效
// 版本：版本标识、版本号、正文与生效时间逐项一致，且正文不夹带更正原因。
func assertEffectiveMatchesVersion(t *testing.T, got EffectiveRecord, want Version, label string) {
	t.Helper()
	if got.RecordID != want.RecordID {
		t.Fatalf("%s: record id = %q, want %q", label, got.RecordID, want.RecordID)
	}
	if got.VersionID != want.ID {
		t.Fatalf("%s: version id = %q, want current effective %q", label, got.VersionID, want.ID)
	}
	if got.Version != want.Number {
		t.Fatalf("%s: version number = %d, want %d", label, got.Version, want.Number)
	}
	if got.Content != want.Content {
		t.Fatalf("%s: content = %q, want current effective %q", label, got.Content, want.Content)
	}
	if !got.EffectiveAt.Equal(want.CreatedAt) {
		t.Fatalf("%s: effective-at = %v, want %v", label, got.EffectiveAt, want.CreatedAt)
	}
	if strings.Contains(got.Content, "更正原因") {
		t.Fatalf("%s: correction reason leaked into content: %q", label, got.Content)
	}
}

// assertReadExactly 核对一次查阅的结果集合恰好是 want 中的记录：不多不少、
// 按记录标识稳定排序，且每条都准确对应当前生效版本。
func assertReadExactly(t *testing.T, res ReadResult, encounterID ID, want map[ID]Version, label string) {
	t.Helper()
	if res.EncounterID != encounterID || res.Category != Diagnosis {
		t.Fatalf("%s: result header = (%q, %q), want (%q, %q)",
			label, res.EncounterID, res.Category, encounterID, Diagnosis)
	}
	if len(res.Records) != len(want) {
		t.Fatalf("%s: record count = %d (%v), want %d",
			label, len(res.Records), readRecordIDs(res), len(want))
	}
	for i := 1; i < len(res.Records); i++ {
		if res.Records[i-1].RecordID > res.Records[i].RecordID {
			t.Fatalf("%s: records not in stable id order: %+v", label, res.Records)
		}
	}
	for _, got := range res.Records {
		w, ok := want[got.RecordID]
		if !ok {
			t.Fatalf("%s: unexpected record %q in result: %+v", label, got.RecordID, got)
		}
		assertEffectiveMatchesVersion(t, got, w, label+" record "+string(got.RecordID))
	}
}

// assertDeniedEmpty 核对一次被拒绝的查阅：ErrAccessDenied，且结果为空——
// 不携带就诊、类别、记录标识、数量或正文。
func assertDeniedEmpty(t *testing.T, res ReadResult, err error, label string) {
	t.Helper()
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("%s: err = %v, want ErrAccessDenied", label, err)
	}
	if errors.Is(err, ErrMismatchedPatient) {
		t.Fatalf("%s: receiver read must not surface the internal cross-patient error: %v", label, err)
	}
	if res.EncounterID != "" || res.Category != "" || len(res.Records) != 0 {
		t.Fatalf("%s: denied read must carry no encounter/category/record data, got %+v", label, res)
	}
}

// ---- 每次读取只使用所请求患者自己的授权，互不拓宽、互不混入 ----

func TestReadUsesOnlyRequestedPatientsOwnGrants(t *testing.T) {
	f := setupReadPatientIsolation(t)

	// 读甲：只见明确选中的两条诊断。同类但未选中的 aOther、授权后新增的 aLate、
	// 未生效草稿 aDraft 都不出现；乙的整类授权再宽也不能让甲的读取多出内容。
	resA, err := f.s.Read(rcv, f.pidA, f.encA, Diagnosis)
	if err != nil {
		t.Fatalf("read patient A: %v", err)
	}
	assertReadExactly(t, resA, f.encA, map[ID]Version{
		f.aSel1: f.wantVersion[f.aSel1],
		f.aSel2: f.wantVersion[f.aSel2],
	}, "patient A read")
	for _, got := range resA.Records {
		if got.RecordID == f.aOther || got.RecordID == f.aLate || got.RecordID == f.aDraft {
			t.Fatalf("unselected/draft record %q leaked into patient A read", got.RecordID)
		}
		if !strings.HasPrefix(got.Content, "甲-") {
			t.Fatalf("foreign content leaked into patient A read: %+v", got)
		}
	}
	// 被选中的 aSel1 已更正：必须返回更正版，不能退回初版。
	if resA.Records[0].Content == "甲-选中诊断一-初版" ||
		(len(resA.Records) > 1 && resA.Records[1].Content == "甲-选中诊断一-初版") {
		t.Fatalf("corrected record fell back to the old version: %+v", resA.Records)
	}

	// 读乙：整类授权覆盖该次就诊全部已生效诊断，含授权后才生效的 bLate；
	// 草稿 bDraft 与甲的任何记录都不出现。
	resB, err := f.s.Read(rcv, f.pidB, f.encB, Diagnosis)
	if err != nil {
		t.Fatalf("read patient B: %v", err)
	}
	assertReadExactly(t, resB, f.encB, map[ID]Version{
		f.bDiag1: f.wantVersion[f.bDiag1],
		f.bDiag2: f.wantVersion[f.bDiag2],
		f.bLate:  f.wantVersion[f.bLate],
	}, "patient B read")
	for _, got := range resB.Records {
		if got.RecordID == f.bDraft {
			t.Fatalf("draft record %q leaked into patient B read", got.RecordID)
		}
		if !strings.HasPrefix(got.Content, "乙-") {
			t.Fatalf("foreign content leaked into patient B read: %+v", got)
		}
	}

	// 两份结果互不携带对方患者的任何记录标识。
	idsA := readRecordIDs(resA)
	for id := range readRecordIDs(resB) {
		if idsA[id] {
			t.Fatalf("record %q appears in both patients' reads", id)
		}
	}
}

// ---- 患者与就诊必须属于同一份档案：跨患者组合一律拒绝且结果为空 ----

func TestReadCrossPatientCombinationDenied(t *testing.T) {
	f := setupReadPatientIsolation(t)

	// 甲的患者标识 + 乙的就诊标识：接收方对乙的就诊确有整类授权、对甲的患者
	// 确有限定授权，但两份授权不能拼成一次合法读取。
	resAB, err := f.s.Read(rcv, f.pidA, f.encB, Diagnosis)
	assertDeniedEmpty(t, resAB, err, "patient A id + patient B encounter")

	// 反向混用：乙的患者标识 + 甲的就诊标识。
	resBA, err := f.s.Read(rcv, f.pidB, f.encA, Diagnosis)
	assertDeniedEmpty(t, resBA, err, "patient B id + patient A encounter")

	// 两种混用与“不存在”的拒绝在结果形态上不可区分（都是空结果）。
	if !reflect.DeepEqual(resAB, resBA) {
		t.Fatalf("cross-patient denials differ in shape:\n ab=%+v\n ba=%+v", resAB, resBA)
	}
}

// ---- 不存在的患者或就诊标识：与无权访问不可区分 ----

func TestReadNonexistentIDsDeniedIndistinguishably(t *testing.T) {
	f := setupReadPatientIsolation(t)

	// 格式合法但不存在的患者标识。
	resNoPat, err := f.s.Read(rcv, "pat_nonexistent", f.encA, Diagnosis)
	assertDeniedEmpty(t, resNoPat, err, "nonexistent patient")

	// 格式合法但不存在的就诊标识。
	resNoEnc, err := f.s.Read(rcv, f.pidA, "enc_nonexistent", Diagnosis)
	assertDeniedEmpty(t, resNoEnc, err, "nonexistent encounter")

	// 两者都不存在。
	resNeither, err := f.s.Read(rcv, "pat_nonexistent", "enc_nonexistent", Diagnosis)
	assertDeniedEmpty(t, resNeither, err, "both nonexistent")

	// 与真实的跨患者混用拒绝形态完全一致：接收方无法区分“不存在”与“无权”。
	resMixed, err := f.s.Read(rcv, f.pidA, f.encB, Diagnosis)
	assertDeniedEmpty(t, resMixed, err, "cross-patient combination")
	if !reflect.DeepEqual(resNoPat, resMixed) || !reflect.DeepEqual(resNoEnc, resMixed) {
		t.Fatalf("nonexistent-id denial distinguishable from no-access denial:\n nopat=%+v noenc=%+v mixed=%+v",
			resNoPat, resNoEnc, resMixed)
	}
}

// ---- 读取（含被拒绝的读取）无副作用：不新增审计、不改授权、不生成版本 ----

func TestReadAndDeniedReadLeaveNoSideEffects(t *testing.T) {
	f := setupReadPatientIsolation(t)

	// 基线：两名患者各自的获准内容、审计、授权清单，以及全库记录/版本数量。
	baseA, err := f.s.Read(rcv, f.pidA, f.encA, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	baseB, err := f.s.Read(rcv, f.pidB, f.encB, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	auditA := mustAudit(t, f.s, f.pidA)
	auditB := mustAudit(t, f.s, f.pidB)
	authsA, err := f.s.ListAuthorizations(doc, f.pidA, "")
	if err != nil {
		t.Fatal(err)
	}
	authsB, err := f.s.ListAuthorizations(doc, f.pidB, "")
	if err != nil {
		t.Fatal(err)
	}
	versionsBefore := countVersions(f.s)
	recordsBefore := countRecords(f.s)

	// 正常读取 + 各种被拒绝的读取，全部重复多次。
	for i := 0; i < 2; i++ {
		if _, err := f.s.Read(rcv, f.pidA, f.encA, Diagnosis); err != nil {
			t.Fatalf("normal read A: %v", err)
		}
		if _, err := f.s.Read(rcv, f.pidB, f.encB, Diagnosis); err != nil {
			t.Fatalf("normal read B: %v", err)
		}
		denied := []struct {
			label string
			pid   ID
			eid   ID
		}{
			{"repeated: A id + B encounter", f.pidA, f.encB},
			{"repeated: B id + A encounter", f.pidB, f.encA},
			{"repeated: nonexistent patient", "pat_nonexistent", f.encA},
			{"repeated: nonexistent encounter", f.pidA, "enc_nonexistent"},
		}
		for _, d := range denied {
			res, err := f.s.Read(rcv, d.pid, d.eid, Diagnosis)
			assertDeniedEmpty(t, res, err, d.label)
		}
	}

	// 不新增审计。
	if got := mustAudit(t, f.s, f.pidA); !reflect.DeepEqual(got, auditA) {
		t.Fatalf("patient A audit changed by reads:\n before=%+v\n after=%+v", auditA, got)
	}
	if got := mustAudit(t, f.s, f.pidB); !reflect.DeepEqual(got, auditB) {
		t.Fatalf("patient B audit changed by reads:\n before=%+v\n after=%+v", auditB, got)
	}
	// 不改动授权。
	if got, _ := f.s.ListAuthorizations(doc, f.pidA, ""); !reflect.DeepEqual(got, authsA) {
		t.Fatalf("patient A authorizations changed by reads:\n before=%+v\n after=%+v", authsA, got)
	}
	if got, _ := f.s.ListAuthorizations(doc, f.pidB, ""); !reflect.DeepEqual(got, authsB) {
		t.Fatalf("patient B authorizations changed by reads:\n before=%+v\n after=%+v", authsB, got)
	}
	// 不生成记录版本、不增减记录。
	if got := countVersions(f.s); got != versionsBefore {
		t.Fatalf("reads created versions: before=%d after=%d", versionsBefore, got)
	}
	if got := countRecords(f.s); got != recordsBefore {
		t.Fatalf("reads changed records: before=%d after=%d", recordsBefore, got)
	}

	// 之后用原本正确的患者与就诊组合读取，两名患者各自的获准内容保持原样。
	againA, err := f.s.Read(rcv, f.pidA, f.encA, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(againA, baseA) {
		t.Fatalf("patient A read changed after denied attempts:\n before=%+v\n after=%+v", baseA, againA)
	}
	againB, err := f.s.Read(rcv, f.pidB, f.encB, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(againB, baseB) {
		t.Fatalf("patient B read changed after denied attempts:\n before=%+v\n after=%+v", baseB, againB)
	}
}

// countVersions 统计全库已生成的版本数量（只读）。
func countVersions(s *Store) int {
	c := 0
	_ = s.view(func(snap *snapshot) error { c = len(snap.Versions); return nil })
	return c
}
