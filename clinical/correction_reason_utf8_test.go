package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖更正原因的 UTF-8 校验与保存一致性：内部使用者通过 CorrectRecord
// 更正已生效记录时，原因夹带任何无效 UTF-8 字节（含不完整多字节字符），
// 必须整体返回 ErrInvalidArgument 与零值版本，不能靠静默替换（U+FFFD）、
// 截断或跳过坏字节继续保存，也不能让当次历史里是原字节、关闭重开后原因
// 却变成另一段带替换字符的文字。合法 UTF-8 原因（含中文、换行、前后空白
// 与用户明确输入的合法 U+FFFD）必须原样保存：成功返回的新版本、内部查看
// 的历史与关闭重开后的原因逐字相同。空原因与仅含空白的原因继续按既有
// 规则拒绝；合法原因配合过期版本号仍返回 ErrConflict。

// invalidCorrectionReasons 是各类无效 UTF-8 更正原因：坏序列出现在开头、
// 结尾、中间，夹在合法中文之间，孤立续字节，不完整多字节字符，单个非法
// 字节，overlong 编码与 UTF-16 代理项编码；另有一例在合法 U+FFFD 之后
// 紧跟非法字节——必须因那个坏字节被拒绝；还有一例可读中文说明之后跟坏
// 字节——不能只保存其中合法的部分。
var invalidCorrectionReasons = []struct {
	label  string
	reason string
}{
	{"开头非法字节", "\xff复诊后修正诊断措辞"},
	{"结尾非法字节", "复诊后修正诊断措辞\xff"},
	{"中间非法字节", "复诊\xff后修正诊断措辞"},
	{"中文之间夹坏字节", "复核补录\xfe说明：措辞修正"},
	{"孤立续字节", "复核补录说明\x80"},
	{"结尾不完整多字节字符", "复核补录说明\xe6"},
	{"中间不完整多字节序列", "ab\xe6\x9d复核cd"},
	{"中文之后截断三字节序列", "复核补录说明\xe7\xa1"},
	{"最后一个多字节字符没有写完整", "更正原因\xe4\xb8"},
	{"单个非法字节", "\x80"},
	{"overlong 编码", "复核\xc0\x80原因"},
	{"UTF-16 代理项编码", "第一行\n\xed\xa0\x80\n第二行"},
	{"合法替换字符后紧跟非法字节", "补充\ufffd\xff"},
	{"可读中文说明夹带坏字节", "据最新化验结果，修正诊断：\n高血压 I10（\xe6控制稳定）"},
}

// validCorrectionReasons 是必须逐字原样保存的合法 UTF-8 更正原因。
var validCorrectionReasons = []struct {
	label  string
	reason string
}{
	{"纯中文", "复诊后修正诊断措辞"},
	{"中文与全角标点", "原因：诊断措辞调整（复核确认）"},
	{"表情字符", "据新结果修正 🩸，措辞调整 😷"},
	{"组合字符", "e\u0301 与组合字符原因测试"},
	{"换行引号反斜杠", "  第一行\n第二行\t\"引号\" 与 C:\\路径  "},
	{"前后空白保留", "\t\n  更正原因保持原样  \r\n "},
	{"用户明确输入的合法替换字符", "补充\ufffd（用户明确输入的 U+FFFD）"},
}

// correctionReasonFixture 建立更正原因 UTF-8 测试所需的状态：一名患者、
// 一次就诊、一条已有两个版本的已生效诊断记录，以及一条覆盖该记录的有效
// 授权。返回两个版本与就诊标识，便于逐字比对历史与接收方读取。
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
	d, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "初诊：高血压 I10")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	v2, err := s.CorrectRecord(doc, d.ID, 1, "复诊：高血压 I10（控制稳定）", "首次复核更新")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return correctionReasonFixture{pid: p.ID, eid: e.ID, rid: d.ID, v1: v1, v2: v2}
}

// TestCorrectRecordRejectsInvalidUTF8Reason 覆盖各类坏字节更正原因：患者未
// 停用、正文合法、版本号均为当前值时，只要原因不是完整合法 UTF-8，就返回
// ErrInvalidArgument 与零值版本；记录仍指向提交前的当前版本，正文、版本号、
// 旧版本与已有原因全部保持原样，历史不增加版本、更正审计不增加事件，接收方
// 读取不变，关闭重开后依然如此。把原因改为合法文字后，仍可凭此前的当前
// 版本号完成同一次更正：只产生紧接原版本的新版本，版本号不被失败尝试跳过。
func TestCorrectRecordRejectsInvalidUTF8Reason(t *testing.T) {
	for _, tc := range invalidCorrectionReasons {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			f := setupCorrectionReason(t, s, clk)
			content := "三次复诊：血压 118/76，措辞再次调整"

			histBefore := findHistory(mustChart(t, s, f.pid), f.rid)
			auditBefore, err := s.AuditEvents(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			readBefore, err := s.Read(rcv, f.pid, f.eid, Diagnosis)
			if err != nil {
				t.Fatal(err)
			}

			// 坏字节原因：ErrInvalidArgument，且返回的版本必须是零值，
			// 不能携带任何新生成的版本标识或编号。
			got, err := s.CorrectRecord(doc, f.rid, 2, content, tc.reason)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("CorrectRecord(reason=%q) err = %v, want ErrInvalidArgument", tc.reason, err)
			}
			if got != (Version{}) {
				t.Fatalf("rejected correction returned non-empty version: %+v", got)
			}

			// 同样的坏字节原因再交一次，仍是参数错误，而不是任何形式的成功。
			if got, err := s.CorrectRecord(doc, f.rid, 2, content, tc.reason); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("repeat invalid-utf8 reason err = %v, version = %+v, want ErrInvalidArgument", err, got)
			}

			// 当次内部视图：当前版本、旧版本内容、版本关系与已有原因原样，
			// 没有新版本；审计不新增；接收方读取不变。
			hist := findHistory(mustChart(t, s, f.pid), f.rid)
			if hist == nil || hist.CurrentVersion == nil {
				t.Fatalf("history missing")
			}
			cur := hist.CurrentVersion
			if cur.ID != f.v2.ID || cur.Number != 2 || cur.Content != f.v2.Content ||
				cur.PrevID != f.v1.ID || cur.Reason != f.v2.Reason {
				t.Fatalf("current version changed: %+v", cur)
			}
			if hist.Record.CurrentVersionID != f.v2.ID || len(hist.Versions) != 2 ||
				!reflect.DeepEqual(hist.Record.Versions, []ID{f.v1.ID, f.v2.ID}) ||
				!reflect.DeepEqual(hist.Versions[0], f.v1) || !reflect.DeepEqual(hist.Versions[1], f.v2) {
				t.Fatalf("version history changed: %+v", hist)
			}
			if audit, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after rejected correction: %v %+v", err, audit)
			}
			if res, err := s.Read(rcv, f.pid, f.eid, Diagnosis); err != nil || !reflect.DeepEqual(res, readBefore) {
				t.Fatalf("receiver read changed:\nbefore: %+v\nafter:  %+v err=%v", readBefore, res, err)
			}

			// 坏字节没有落盘：关闭重开后历史与审计仍是提交前的样子。
			s = reopenStore(t, s, dir, clk)
			if got := findHistory(mustChart(t, s, f.pid), f.rid); !reflect.DeepEqual(got, histBefore) {
				t.Fatalf("history changed after rejected correction + reopen:\nbefore: %+v\nafter:  %+v", histBefore, got)
			}
			if audit, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after reopen: %v", err)
			}

			// 把原因改为合法文字后，仍用此前的当前版本号 2 提交同一次更正：
			// 成功，只产生紧接 v2 的第 3 版，PrevID 与原因对应正确。
			clk.t = clk.t.Add(time.Hour)
			legalReason := "坏字节原因拒绝后的合法更正"
			v3, err := s.CorrectRecord(doc, f.rid, 2, content, legalReason)
			if err != nil {
				t.Fatalf("legal correction after rejected attempt: %v", err)
			}
			if v3.Number != 3 || v3.PrevID != f.v2.ID || v3.Content != content ||
				v3.Reason != legalReason || !v3.CreatedAt.Equal(clk.t) {
				t.Fatalf("unexpected v3: %+v", v3)
			}
			hist = findHistory(mustChart(t, s, f.pid), f.rid)
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
			_ = s.Close()
		})
	}

	// 空原因与仅含空白的原因继续按既有规则拒绝，历史不变。
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)}
	s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := setupCorrectionReason(t, s, clk)
	histBefore := findHistory(mustChart(t, s, f.pid), f.rid)
	for _, blank := range []string{"", " ", "\n\t\r ", "　"} {
		if got, err := s.CorrectRecord(doc, f.rid, 2, "合法正文", blank); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("blank reason %q err = %v, version = %+v, want ErrInvalidArgument", blank, err, got)
		}
	}
	if got := findHistory(mustChart(t, s, f.pid), f.rid); !reflect.DeepEqual(got, histBefore) {
		t.Fatalf("blank reasons changed history: %+v", got)
	}
}

// TestCorrectRecordValidUTF8ReasonPreservedRoundTrip 覆盖合法更正原因：原样
// 保存，不去前后空白、不改写字符形态；成功返回的新版本、当次内部查看到的
// 原因与关闭后从同一位置重新打开取得的原因逐字一致，上一版本关系正确。
func TestCorrectRecordValidUTF8ReasonPreservedRoundTrip(t *testing.T) {
	for _, tc := range validCorrectionReasons {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			f := setupCorrectionReason(t, s, clk)
			content := "三次复诊：血压 118/76，诊断措辞再次调整"

			clk.t = clk.t.Add(time.Hour)
			v3, err := s.CorrectRecord(doc, f.rid, 2, content, tc.reason)
			if err != nil {
				t.Fatalf("CorrectRecord(reason=%q): %v", tc.reason, err)
			}
			// 返回的新版本原因逐字保留。
			if v3.Number != 3 || v3.PrevID != f.v2.ID || v3.Content != content ||
				v3.Reason != tc.reason {
				t.Fatalf("returned v3 mismatch: %+v", v3)
			}

			// 当次内部历史：新版本与旧版本的原因都逐字保留。
			hist := findHistory(mustChart(t, s, f.pid), f.rid)
			if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != v3.ID ||
				len(hist.Versions) != 3 {
				t.Fatalf("history after correction: %+v", hist)
			}
			if hist.CurrentVersion.Reason != tc.reason || hist.Versions[2].Reason != tc.reason {
				t.Fatalf("stored reason = %q, want %q", hist.CurrentVersion.Reason, tc.reason)
			}
			if hist.Versions[1].Reason != f.v2.Reason || hist.Versions[0].Reason != "" {
				t.Fatalf("older reasons changed: %+v", hist.Versions)
			}

			// 接收方只读到当前生效正文，不因本次修复获得查看原因或旧版本的权限。
			res, err := s.Read(rcv, f.pid, f.eid, Diagnosis)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Records) != 1 || res.Records[0].VersionID != v3.ID ||
				res.Records[0].Version != 3 || res.Records[0].Content != content {
				t.Fatalf("receiver read: %+v", res.Records)
			}

			// 关闭后从同一位置重新打开：原因逐字一致，版本关系不变。
			s = reopenStore(t, s, dir, clk)
			t.Cleanup(func() { _ = s.Close() })
			hist = findHistory(mustChart(t, s, f.pid), f.rid)
			if hist == nil || len(hist.Versions) != 3 {
				t.Fatalf("history after reopen: %+v", hist)
			}
			if !reflect.DeepEqual(hist.Versions[0], f.v1) || !reflect.DeepEqual(hist.Versions[1], f.v2) {
				t.Fatalf("older versions changed after reopen: %+v", hist.Versions)
			}
			got := hist.Versions[2]
			if got.Content != content || got.PrevID != f.v2.ID || got.Reason != tc.reason {
				t.Fatalf("v3 reason changed across reopen:\n got=%q\nwant=%q (full: %+v)",
					got.Reason, tc.reason, got)
			}
		})
	}
}

// TestCorrectRecordExplicitReplacementCharIsLegal 守住“用户明确输入的合法
// U+FFFD 可以保存，它与无效字节不是一回事”：明确含 U+FFFD 的原因更正成功
// 并原样往返；只有对应位置换成坏字节的同形原因必须被 ErrInvalidArgument
// 拒绝，不能与已保存的那份原因混为一谈，也不能在重开后变成同一段文字。
func TestCorrectRecordExplicitReplacementCharIsLegal(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 7, 7, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	f := setupCorrectionReason(t, s, clk)
	content := "三次复诊：补充化验结果后修正"

	// 用户明确输入的合法 U+FFFD 原因可以正常更正。
	clk.t = clk.t.Add(time.Hour)
	explicit := "补充\ufffd说明"
	v3, err := s.CorrectRecord(doc, f.rid, 2, content, explicit)
	if err != nil {
		t.Fatalf("correction with explicit U+FFFD reason: %v", err)
	}
	auditBefore, err := s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}

	// 同形位置换成坏字节的原因：参数错误，历史停在 v3，审计不增加。
	bad := "补充\xff说明"
	if got, err := s.CorrectRecord(doc, f.rid, 3, content+"（再次）", bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad-byte lookalike reason err = %v, version = %+v, want ErrInvalidArgument", err, got)
	}
	hist := findHistory(mustChart(t, s, f.pid), f.rid)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != v3.ID ||
		hist.CurrentVersion.Reason != explicit || len(hist.Versions) != 3 {
		t.Fatalf("saved reason/history changed after rejected lookalike: %+v", hist)
	}
	if audit, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit changed after rejected lookalike: %v", err)
	}

	// 关闭重开：保存的仍是明确的合法 U+FFFD 原因，而不是坏字节被替换后的结果。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	hist = findHistory(mustChart(t, s, f.pid), f.rid)
	if hist == nil || hist.CurrentVersion.Reason != explicit {
		t.Fatalf("reason changed across reopen: %+v", hist.CurrentVersion)
	}
}

// TestCorrectRecordInvalidUTF8ReasonKeepsConflictRules 保证原因 UTF-8 校验不
// 改变既有入口与冲突规则：合法原因配合过期版本号仍返回 ErrConflict（且不
// 产生任何状态变化）；校验在停用判定之前，患者停用后夹带坏字节的原因仍返回
// ErrInvalidArgument。
func TestCorrectRecordInvalidUTF8ReasonKeepsConflictRules(t *testing.T) {
	s, clk := newTestStore(t)
	f := setupCorrectionReason(t, s, clk)

	// 合法原因 + 过期版本号：ErrConflict，历史与审计不变。
	histBefore := findHistory(mustChart(t, s, f.pid), f.rid)
	auditBefore, err := s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.CorrectRecord(doc, f.rid, 1, "用过期版本号更正", "过期版本号更正"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version correction err = %v, version = %+v, want ErrConflict", err, got)
	}
	if got := findHistory(mustChart(t, s, f.pid), f.rid); !reflect.DeepEqual(got, histBefore) {
		t.Fatalf("rejected conflict changed history: %+v", got)
	}
	if audit, err := s.AuditEvents(doc, f.pid); err != nil || !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("rejected conflict added audit: %v %+v", err, audit)
	}

	// 患者停用后，坏字节原因仍按参数错误拒绝（UTF-8 校验先行），历史不变；
	// 合法原因则因停用失败，同样不产生新版本。
	if err := s.DeactivatePatient(doc, f.pid); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CorrectRecord(doc, f.rid, 2, "停用后试图更正", "停用后\xff坏原因"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("deactivated correction with bad-utf8 reason err = %v, version = %+v, want ErrInvalidArgument", err, got)
	}
	hist := findHistory(mustChart(t, s, f.pid), f.rid)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != f.v2.ID ||
		len(hist.Versions) != 2 {
		t.Fatalf("bad-utf8 reason after deactivation changed history: %+v", hist)
	}

	// 接收方身份不能使用更正入口：既有权限规则不因原因校验而改变。
	if _, err := s.CorrectRecord(rcv, f.rid, 2, "接收方试图更正", "原因"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver correction err = %v, want ErrAccessDenied", err)
	}
}
