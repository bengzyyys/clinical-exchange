package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖更正原因的 UTF-8 校验与保存一致性：CorrectRecord 的原因夹带任何
// 无效 UTF-8 字节（坏字节位于开头、中间、结尾，或最后一个多字节字符没有写
// 完整，即使同时含有可读中文）时，必须整体返回 ErrInvalidArgument 与零值
// Version，不能靠静默替换（U+FFFD）、截断或跳过坏字节继续保存，也不能让
// 当次历史里保留原字节、关闭重开后原因却变成另一段带替换字符的文字。
// 合法 UTF-8 原因（含中文、换行、首尾空白与用户明确输入的合法 U+FFFD）必须
// 原样保存：成功返回的新版本、内部查看的版本历史与关闭后从同一位置重开看到
// 的原因逐字相同。更正原因是解释病历变化的正式历史，同一版本保存前后的解释
// 必须完全一致。

// invalidCorrectionReasons 是各类无效 UTF-8 更正原因：坏序列出现在开头、
// 结尾、中间，夹在合法中文之间，孤立续字节，不完整多字节字符，单个非法
// 字节，overlong 编码与 UTF-16 代理项编码；另含两例——即使整段同时是可读
// 中文说明，以及在合法 U+FFFD 之后紧跟非法字节——都必须因坏字节被拒绝，
// 而不是只保存其中合法的部分。
var invalidCorrectionReasons = []struct {
	label  string
	reason string
}{
	{"开头非法字节", "\xff复诊后更正诊断说明"},
	{"结尾非法字节", "复诊后更正诊断说明\xff"},
	{"中间非法字节", "复诊后更\xff正诊断说明"},
	{"中文之间夹坏字节", "复诊确认血压控\xfe制良好，更正诊断"},
	{"孤立续字节", "复核检验结果后更正\x80诊断"},
	{"结尾不完整多字节字符", "复诊后更正诊断说明\xe6"},
	{"中间不完整多字节序列", "ab\xe6\x9d更正cd"},
	{"中文之后截断三字节序列", "复诊后更正诊断说明\xe7\xa1"},
	{"最后一个多字节字符没有写完整", "更正原因：录\xe4\xb8"},
	{"单个非法字节", "\x80"},
	{"overlong 编码", "更正\xc0\x80原因"},
	{"UTF-16 代理项编码", "第一行\n\xed\xa0\x80\n第二行"},
	{"同时含可读中文说明也不能只存合法部分", "复诊后更正诊断：血压控制良好\xff，依据化验单"},
	{"合法替换字符后紧跟非法字节", "补充更正说明\ufffd\xff"},
}

// validCorrectionReasons 是必须逐字原样保存的合法 UTF-8 更正原因。
var validCorrectionReasons = []struct {
	label  string
	reason string
}{
	{"纯中文", "复诊后血压控制稳定，更正诊断编码"},
	{"中文与全角标点", "诊断：内容与摘要不符（复核确认）"},
	{"表情字符", "化验单复核 🩸，诊断措辞调整 😷"},
	{"换行引号反斜杠", "  第一行原因\n第二行原因\t\"引号\" 与 C:\\路径  "},
	{"首尾空白保留", "\t\n  更正原因保持原样  \r\n "},
	{"用户明确输入的合法替换字符", "补充说明\ufffd（用户明确输入的 U+FFFD）"},
	{"只有合法替换字符", "\ufffd"},
	{"空白包裹合法替换字符", "  \t\ufffd \r\n "},
}

// correctionReasonFixture 准备更正原因测试所需的一条已生效至第 2 版的诊断
// 记录、一次就诊，以及一条覆盖它的有效接收方授权。
type correctionReasonFixture struct {
	pid ID
	eid ID
	rid ID
	v1  Version
	v2  Version
}

func setupCorrectionReason(t *testing.T, s *Store, clk *fakeClock) correctionReasonFixture {
	t.Helper()
	p, err := s.RegisterPatient(doc, "更正原因UTF8患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "初诊：高血压 I10")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	v2, err := s.CorrectRecord(doc, r.ID, 1, "复诊：高血压 I10（控制稳定）", "首次复核更新")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return correctionReasonFixture{pid: p.ID, eid: e.ID, rid: r.ID, v1: v1, v2: v2}
}

// TestCorrectRecordRejectsInvalidUTF8Reason 覆盖坏字节更正原因：正文合法、
// 患者未停用、版本号为当前版本号时，任何夹带无效 UTF-8 的原因都返回
// ErrInvalidArgument 与零值版本；记录仍指向提交前当前版本，正文、版本号、
// 旧版本、版本关系与已有原因原样，历史不增加版本、更正审计不增加事件、
// 接收方读取不变；坏字节从未落盘，关闭重开后仍然如此。空原因与全空白原因
// 继续按既有规则拒绝。此后把原因改为合法文字并沿用原当前版本号 2，仍能完成
// 同一次更正：只产生紧接 v2 的第 3 版，PrevID 与原因对应正确，不因先前失败
// 跳过版本号或留下额外历史；过期版本号仍返回 ErrConflict。
func TestCorrectRecordRejectsInvalidUTF8Reason(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	f := setupCorrectionReason(t, s, clk)
	content := "三次复诊：血压 118/76，控制良好"

	histBefore := findHistory(mustChart(t, s, f.pid), f.rid)
	if histBefore == nil {
		t.Fatal("history missing before corrections")
	}
	auditBefore, err := s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	readBefore, err := s.Read(rcv, f.pid, f.eid, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}

	// 各类坏序列原因都用当前版本号 2 与合法正文提交：一律 ErrInvalidArgument、
	// 零值版本，历史/审计/接收方读取与提交前逐字一致。
	for _, tc := range invalidCorrectionReasons {
		got, err := s.CorrectRecord(doc, f.rid, 2, content, tc.reason)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: CorrectRecord err = %v, want ErrInvalidArgument", tc.label, err)
		}
		if got != (Version{}) {
			t.Fatalf("%s: rejected correction returned non-zero version: %+v", tc.label, got)
		}
		hist := findHistory(mustChart(t, s, f.pid), f.rid)
		if !reflect.DeepEqual(hist, histBefore) {
			t.Fatalf("%s: history changed:\nbefore: %+v\nafter:  %+v", tc.label, histBefore, hist)
		}
		audit, err := s.AuditEvents(doc, f.pid)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: correction audit added on rejected correction", tc.label)
		}
		res, err := s.Read(rcv, f.pid, f.eid, Diagnosis)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", tc.label, readBefore, res)
		}
	}

	// 同样的坏字节原因再交一次，仍是参数错误，而不是任何形式的成功或冲突。
	if _, err := s.CorrectRecord(doc, f.rid, 2, content, invalidCorrectionReasons[0].reason); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("repeat invalid-utf8 reason err = %v, want ErrInvalidArgument", err)
	}

	// 坏字节从未落盘：关闭重开后历史与审计仍是提交前结果。
	s = reopenStore(t, s, dir, clk)
	if got := findHistory(mustChart(t, s, f.pid), f.rid); !reflect.DeepEqual(got, histBefore) {
		t.Fatalf("history changed after rejected corrections + reopen:\nbefore: %+v\nafter:  %+v", histBefore, got)
	}
	if audit, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit changed after reopen: %v %+v", err, audit)
	}

	// 空原因与全空白原因继续按既有规则拒绝，历史不变。
	for _, blank := range []string{"", " ", "\n\t\r ", "　"} {
		if _, err := s.CorrectRecord(doc, f.rid, 2, content, blank); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("blank reason %q err = %v, want ErrInvalidArgument", blank, err)
		}
	}
	if got := findHistory(mustChart(t, s, f.pid), f.rid); !reflect.DeepEqual(got, histBefore) {
		t.Fatalf("blank reason changed history: %+v", got)
	}

	// 失败的更正没有推进版本号：仍用此前的当前版本号 2 与合法原因提交，
	// 成功生成紧接 v2 的第 3 版，PrevID 指向 v2，原因逐字保存，只新增这一次
	// 真正完成的更正审计。
	clk.t = clk.t.Add(time.Hour)
	legalReason := "坏字节原因拒绝后的合法更正\n第二行"
	v3, err := s.CorrectRecord(doc, f.rid, 2, content, legalReason)
	if err != nil {
		t.Fatalf("legal correction after rejected reasons: %v", err)
	}
	if v3.Number != 3 || v3.PrevID != f.v2.ID || v3.Content != content ||
		v3.Reason != legalReason || !v3.CreatedAt.Equal(clk.t) {
		t.Fatalf("unexpected v3: %+v", v3)
	}
	hist := findHistory(mustChart(t, s, f.pid), f.rid)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != v3.ID ||
		len(hist.Versions) != 3 ||
		!reflect.DeepEqual(hist.Versions[0], f.v1) || !reflect.DeepEqual(hist.Versions[1], f.v2) ||
		!reflect.DeepEqual(hist.Versions[2], v3) {
		t.Fatalf("history after legal correction: %+v", hist)
	}
	audit, err := s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after legal correction:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionCorrected || last.ObjectID != f.rid || last.ActorID != doc.ID {
		t.Fatalf("unexpected new audit event: %+v", last)
	}

	// 合法原因配合过期版本号仍返回 ErrConflict，状态与审计不变。
	staleContent := "用过期版本号试图更正"
	if _, err := s.CorrectRecord(doc, f.rid, 2, staleContent, "过期版本号原因"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version correction err = %v, want ErrConflict", err)
	}
	if got := findHistory(mustChart(t, s, f.pid), f.rid); !reflect.DeepEqual(got, hist) {
		t.Fatalf("rejected stale correction changed history: %+v", got)
	}
	if audit2, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit2, audit) {
		t.Fatalf("rejected stale correction changed audit: %v", err)
	}

	// 接收方读到新的当前生效正文；结果结构上不含更正原因或旧版本。
	res, err := s.Read(rcv, f.pid, f.eid, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 1 || res.Records[0].VersionID != v3.ID ||
		res.Records[0].Version != 3 || res.Records[0].Content != content {
		t.Fatalf("receiver read after legal correction: %+v", res.Records)
	}

	// 关闭后从同一位置重新打开：三个版本（含各自原文、PrevID 与原因）与审计
	// 完整保持，第 3 版原因与提交时逐字一致，没有出现替换字符。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	hist = findHistory(mustChart(t, s, f.pid), f.rid)
	if hist == nil || len(hist.Versions) != 3 {
		t.Fatalf("history after reopen: %+v", hist)
	}
	if !reflect.DeepEqual(hist.Versions[0], f.v1) || !reflect.DeepEqual(hist.Versions[1], f.v2) {
		t.Fatalf("older versions changed after reopen: %+v", hist.Versions)
	}
	if got := hist.Versions[2]; got.Content != content || got.PrevID != f.v2.ID ||
		got.Reason != legalReason {
		t.Fatalf("v3 reason/content changed after reopen: %+v", got)
	}
	if audit3, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit3, audit) {
		t.Fatalf("audit changed after reopen: %v", err)
	}
}

// TestCorrectRecordValidUTF8ReasonPreservedRoundTrip 覆盖合法更正原因：成功
// 返回的新版本、内部查看该记录历史的结果，以及关闭后从同一位置重新打开取得
// 的原因，都与提交时逐字一致——包括中文、换行、前后空白，以及用户明确输入
// 的合法 U+FFFD（即使原因只有该字符或被空白包裹）。
func TestCorrectRecordValidUTF8ReasonPreservedRoundTrip(t *testing.T) {
	for _, tc := range validCorrectionReasons {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			f := setupCorrectionReason(t, s, clk)
			clk.t = clk.t.Add(time.Hour)
			content := "三次复诊：糖化血红蛋白 6.4% 🩸"

			v3, err := s.CorrectRecord(doc, f.rid, 2, content, tc.reason)
			if err != nil {
				t.Fatalf("CorrectRecord with legal reason: %v", err)
			}
			if v3.Number != 3 || v3.PrevID != f.v2.ID || v3.Reason != tc.reason ||
				v3.Content != content {
				t.Fatalf("returned version mismatch: %+v", v3)
			}

			// 当次内部查看：当前版本与历史第 3 版原因逐字保留。
			hist := findHistory(mustChart(t, s, f.pid), f.rid)
			if hist == nil || hist.CurrentVersion == nil ||
				hist.CurrentVersion.Reason != tc.reason || len(hist.Versions) != 3 {
				t.Fatalf("chart reason mismatch: %+v", hist)
			}
			if got := hist.Versions[2]; got.Reason != tc.reason || got.PrevID != f.v2.ID {
				t.Fatalf("history v3 reason mismatch: %+v", got)
			}
			// 旧版本原因不被触碰。
			if hist.Versions[0].Reason != "" || hist.Versions[1].Reason != f.v2.Reason {
				t.Fatalf("older reasons changed: %+v", hist.Versions)
			}

			// 关闭后从同一位置重新打开：原因与版本关系逐字保持。
			s = reopenStore(t, s, dir, clk)
			t.Cleanup(func() { _ = s.Close() })
			hist = findHistory(mustChart(t, s, f.pid), f.rid)
			if hist == nil || len(hist.Versions) != 3 {
				t.Fatalf("history after reopen: %+v", hist)
			}
			if got := hist.Versions[2]; got.Reason != tc.reason ||
				got.Content != content || got.PrevID != f.v2.ID {
				t.Fatalf("reason changed across reopen:\n got=%+v\nwant reason %q", got, tc.reason)
			}
			if hist.CurrentVersion == nil || hist.CurrentVersion.Reason != tc.reason {
				t.Fatalf("current reason changed across reopen: %+v", hist.CurrentVersion)
			}
		})
	}
}

// TestCorrectRecordExplicitReplacementCharIsLegal 守住“用户明确输入的合法
// U+FFFD 可以保存，它与无效字节不是一回事”：明确含 U+FFFD 的更正原因成功
// 并原样往返；对应位置换成坏字节的同形原因必须被 ErrInvalidArgument 拒绝，
// 重开后保存的仍是明确的合法 U+FFFD，二者不会变成同一个原因。
func TestCorrectRecordExplicitReplacementCharIsLegal(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 13, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	f := setupCorrectionReason(t, s, clk)

	// 用户明确输入的合法 U+FFFD 原因更正成功并原样保存。
	clk.t = clk.t.Add(time.Hour)
	explicit := "补充更正说明\ufffd"
	v3, err := s.CorrectRecord(doc, f.rid, 2, "三次复诊：血压 120/78", explicit)
	if err != nil {
		t.Fatalf("correct with explicit U+FFFD reason: %v", err)
	}
	if v3.Reason != explicit {
		t.Fatalf("saved explicit reason = %q, want %q", v3.Reason, explicit)
	}
	auditBefore, err := s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}

	// 同形位置换成坏字节的原因：当前版本号已是 3。即使业务条件都满足，也必须
	// 因坏字节返回参数错误，不能保存，也不能在重开后与合法 U+FFFD 原因混淆。
	if got, err := s.CorrectRecord(doc, f.rid, 3, "四次复诊：血压 118/76", "补充更正说明\xff"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad-byte lookalike reason err = %v, version = %+v, want ErrInvalidArgument", err, got)
	}
	hist := findHistory(mustChart(t, s, f.pid), f.rid)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != v3.ID ||
		hist.CurrentVersion.Reason != explicit || len(hist.Versions) != 3 {
		t.Fatalf("rejected lookalike changed history: %+v", hist)
	}
	if audit, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit changed after rejected lookalike: %v %+v", err, audit)
	}

	// 关闭重开：第 3 版原因仍是明确的合法 U+FFFD，而不是坏字节被替换后的结果。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	hist = findHistory(mustChart(t, s, f.pid), f.rid)
	if hist == nil || len(hist.Versions) != 3 || hist.Versions[2].Reason != explicit {
		t.Fatalf("reason changed across reopen: %+v", hist)
	}
}

// TestCorrectRecordInvalidUTF8ReasonRejectedWhenPatientDeactivated 保证原因的
// UTF-8 校验先行：患者停用后，夹带坏字节原因（正文合法、版本号合法）的更正
// 仍返回 ErrInvalidArgument，而不是停用错误，且状态不变。
func TestCorrectRecordInvalidUTF8ReasonRejectedWhenPatientDeactivated(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 14, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	f := setupCorrectionReason(t, s, clk)
	if err := s.DeactivatePatient(doc, f.pid); err != nil {
		t.Fatal(err)
	}

	if got, err := s.CorrectRecord(doc, f.rid, 2, "停用后提交的合法正文", "停用后原因\xff夹带坏字节"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("deactivated correct with bad-utf8 reason err = %v, version = %+v, want ErrInvalidArgument", err, got)
	}
	hist := findHistory(mustChart(t, s, f.pid), f.rid)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != f.v2.ID ||
		len(hist.Versions) != 2 {
		t.Fatalf("bad-utf8 reason after deactivation changed history: %+v", hist)
	}

	// 重开后坏字节仍未落盘，历史不变。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	hist = findHistory(mustChart(t, s, f.pid), f.rid)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != f.v2.ID ||
		hist.CurrentVersion.Reason != f.v2.Reason || len(hist.Versions) != 2 {
		t.Fatalf("history changed after deactivated rejection + reopen: %+v", hist)
	}
}
