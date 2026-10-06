package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖诊断/医嘱正文的保存一致性：正文夹带任何无效 UTF-8 字节时，
// 新建草稿、修改草稿与更正生效记录都必须整体返回 ErrInvalidArgument，
// 不能靠静默替换（U+FFFD）、截断或跳过坏字节继续保存；合法 UTF-8 正文
// （含中文、表情、组合字符、换行、引号、反斜杠、首尾空白与用户明确输入
// 的合法 U+FFFD）必须原样保存，当次查询与关闭后从同一位置重新打开看到
// 的正文逐字相同。

// invalidContents 是各类无效 UTF-8 正文：坏序列出现在开头、结尾、中间，
// 夹在合法中文之间，孤立续字节，不完整多字节字符，单个非法字节，
// overlong 编码与 UTF-16 代理项编码；另有一例在合法 U+FFFD 之后紧跟
// 非法字节——必须因那个坏字节被拒绝，而不是把整串当成合法替换字符。
var invalidContents = []struct {
	label   string
	content string
}{
	{"开头非法字节", "\xff诊断：高血压 I10"},
	{"结尾非法字节", "诊断：高血压 I10\xff"},
	{"中间非法字节夹在中文之间", "诊\xff断：高血压"},
	{"合法中文之间夹坏字节", "高血压 I10（复\xfe核确认）"},
	{"孤立续字节", "血压 120/80\x80 mmHg"},
	{"结尾不完整多字节字符", "诊断：高血压 I10\xe6"},
	{"中间不完整多字节序列", "abc\xe6\x9ddef"},
	{"中文之后截断三字节序列", "高血压 I10（复核\xe7\xa1"},
	{"单个非法字节", "\x80"},
	{"overlong 编码", "\xc0\x80"},
	{"UTF-16 代理项编码", "第一行\n\xed\xa0\x80\n第二行"},
	{"合法替换字符后紧跟非法字节", "补充\ufffd\xff"},
}

// validContents 是必须逐字原样保存的合法 UTF-8 正文。
var validContents = []struct {
	label   string
	content string
}{
	{"中文与全角标点", "诊断：高血压 I10（复核确认）"},
	{"表情字符", "血糖偏高 🩸，建议复诊 😷"},
	{"组合字符", "e\u0301leve\u0301 与组合字符测试"},
	{"多码位表情", "值班：👩🏽‍⚕️ 随访 👨‍👩‍👧"},
	{"换行引号反斜杠", "  第一行\n第二行\t\"引号\" 与 C:\\路径  "},
	{"首尾空白", "\t\n  正文内容保持原样  \r\n "},
	{"用户明确输入的合法替换字符", "补充说明\ufffd（用户明确输入的 U+FFFD）"},
}

// TestCreateDraftRejectsInvalidUTF8 覆盖新建草稿：任何夹带无效 UTF-8 的正文
// 都返回 ErrInvalidArgument，不留下新记录、不产生任何状态变化（关闭重开
// 后同样不存在）。诊断与医嘱两个类别都适用。空白正文继续按既有规则拒绝。
func TestCreateDraftRejectsInvalidUTF8(t *testing.T) {
	for _, category := range []string{Diagnosis, Order} {
		for _, tc := range invalidContents {
			t.Run(category+"/"+tc.label, func(t *testing.T) {
				dir := t.TempDir()
				clk := &fakeClock{t: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
				open := func() *Store {
					s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
					if err != nil {
						t.Fatalf("open: %v", err)
					}
					return s
				}

				s := open()
				p, err := s.RegisterPatient(doc, "无效UTF8新建草稿患者")
				if err != nil {
					t.Fatal(err)
				}
				e, err := s.AddEncounter(doc, p.ID, time.Time{})
				if err != nil {
					t.Fatal(err)
				}

				before := countRecords(s)
				if _, err := s.CreateDraft(doc, p.ID, e.ID, category, tc.content); !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("CreateDraft(%q) err = %v, want ErrInvalidArgument", tc.content, err)
				}
				// 拒绝新建不能留下新记录：档案与记录数都与提交前一致。
				if got := countRecords(s); got != before {
					t.Fatalf("rejected create left records: count before=%d after=%d", before, got)
				}
				chart, err := s.Chart(doc, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(chart.Records) != 0 {
					t.Fatalf("rejected create left a record: %+v", chart.Records)
				}
				// 坏字节没有被落盘：关闭重开后仍无记录。
				s = reopenStore(t, s, dir, clk)
				if got := countRecords(s); got != before {
					t.Fatalf("rejected create persisted after reopen: count=%d", got)
				}
				chart, err = s.Chart(doc, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(chart.Records) != 0 {
					t.Fatalf("rejected create left a record after reopen: %+v", chart.Records)
				}
				_ = s.Close()
			})
		}
	}

	// 空串与全空白正文继续按既有规则拒绝。
	s, _ := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	for _, blank := range []string{"", " ", "\n\t\r ", "　"} {
		if _, err := s.CreateDraft(doc, pid, eid, Diagnosis, blank); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("CreateDraft blank content %q err = %v, want ErrInvalidArgument", blank, err)
		}
	}
	if got := countRecords(s); got != 0 {
		t.Fatalf("blank creates left records: %d", got)
	}

	// 接收方身份不能新建草稿：既有身份权限保持不变。
	if _, err := s.CreateDraft(rcv, pid, eid, Diagnosis, "接收方试图建草稿"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver CreateDraft err = %v, want ErrAccessDenied", err)
	}
}

// TestCreateDraftValidUTF8PreservedRoundTrip 覆盖合法正文的新建草稿：
// 当次查询与关闭后重新打开看到相同原文，不做任何字符形态统一或排版整理。
func TestCreateDraftValidUTF8PreservedRoundTrip(t *testing.T) {
	for _, tc := range validContents {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			p, err := s.RegisterPatient(doc, "合法UTF8新建草稿患者")
			if err != nil {
				t.Fatal(err)
			}
			e, err := s.AddEncounter(doc, p.ID, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, tc.content)
			if err != nil {
				t.Fatalf("CreateDraft: %v", err)
			}
			if r.DraftContent != tc.content {
				t.Fatalf("returned draft content = %q, want %q", r.DraftContent, tc.content)
			}
			chart, err := s.Chart(doc, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			hist := findHistory(chart, r.ID)
			if hist == nil || hist.DraftContent != tc.content {
				t.Fatalf("chart content = %+v, want %q", hist, tc.content)
			}

			// 关闭后从同一位置重新打开：正文逐字保持。
			s = reopenStore(t, s, dir, clk)
			t.Cleanup(func() { _ = s.Close() })
			chart, err = s.Chart(doc, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			hist = findHistory(chart, r.ID)
			if hist == nil || !hist.HasDraft || hist.DraftContent != tc.content {
				t.Fatalf("draft content after reopen = %+v, want %q", hist, tc.content)
			}

			// 生效后第 1 版固化同一份原文，重开后仍一致。
			v1, err := s.ActivateRecord(doc, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if v1.Content != tc.content {
				t.Fatalf("activated content = %q, want %q", v1.Content, tc.content)
			}
			s = reopenStore(t, s, dir, clk)
			t.Cleanup(func() { _ = s.Close() })
			chart, err = s.Chart(doc, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			hist = findHistory(chart, r.ID)
			if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.Content != tc.content {
				t.Fatalf("active content after reopen = %+v, want %q", hist, tc.content)
			}
		})
	}
}

// TestUpdateDraftRejectsInvalidUTF8 覆盖修改草稿：坏字节正文返回
// ErrInvalidArgument，原草稿的正文与状态保持不变（无版本、无审计变化），
// 重开后亦然；拒绝后仍可用合法正文成功修改，原文逐字保留。
func TestUpdateDraftRejectsInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "无效UTF8改草稿患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	original := "原草稿：血压 120/80\n待补充 "
	target, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, original)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 各类坏序列一律拒绝；个别字节不合法也不能保留其余合法部分。
	for _, tc := range invalidContents {
		if _, err := s.UpdateDraft(doc, target.ID, tc.content); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: UpdateDraft err = %v, want ErrInvalidArgument", tc.label, err)
		}
		chart, err := s.Chart(doc, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		hist := findHistory(chart, target.ID)
		if hist == nil || !hist.HasDraft || hist.DraftContent != original ||
			hist.CurrentVersion != nil || len(hist.Versions) != 0 {
			t.Fatalf("%s: rejected update changed the draft: %+v", tc.label, hist)
		}
		audit, err := s.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit changed after rejected update", tc.label)
		}
	}

	// 拒绝后重开：原草稿正文与状态仍在，坏字节从未落盘。
	s = reopenStore(t, s, dir, clk)
	chart, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, target.ID)
	if hist == nil || !hist.HasDraft || hist.DraftContent != original {
		t.Fatalf("draft changed after rejected updates + reopen: %+v", hist)
	}

	// 空白正文继续被拒绝，原草稿保留。
	if _, err := s.UpdateDraft(doc, target.ID, "  \n\t "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank update err = %v, want ErrInvalidArgument", err)
	}
	chart, err = s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hist = findHistory(chart, target.ID); hist == nil || hist.DraftContent != original {
		t.Fatalf("rejected blank update changed the draft: %+v", hist)
	}

	// 失败后可以提交一份合法正文（含表情、组合字符、换行引号反斜杠、
	// 首尾空白与合法 U+FFFD），当次与重开后逐字一致。
	valid := "\t 新正文 🩸 e\u0301\n\"引号\" C:\\路径 \ufffd  "
	updated, err := s.UpdateDraft(doc, target.ID, valid)
	if err != nil {
		t.Fatalf("valid update after rejections: %v", err)
	}
	if updated.DraftContent != valid {
		t.Fatalf("updated draft content = %q, want %q", updated.DraftContent, valid)
	}
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	chart, err = s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist = findHistory(chart, target.ID)
	if hist == nil || !hist.HasDraft || hist.DraftContent != valid {
		t.Fatalf("valid content after reopen = %+v, want %q", hist, valid)
	}

	// 生效固化最后一次成功保存的合法正文，而不是任何被拒绝的内容。
	v1, err := s.ActivateRecord(doc, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Content != valid {
		t.Fatalf("v1 content = %q, want %q", v1.Content, valid)
	}
	// 已生效记录不能再走草稿路径：既有规则保持不变。
	if _, err := s.UpdateDraft(doc, target.ID, "生效后试图覆盖"); !errors.Is(err, ErrActive) {
		t.Fatalf("update active record err = %v, want ErrActive", err)
	}
}

// TestCorrectRecordRejectsInvalidUTF8 覆盖更正生效记录：坏字节正文返回
// ErrInvalidArgument，当前版本、旧版本内容、版本关系（PrevID）与原因
// 全部保持，不生成新版本、不新增更正审计，接收方读取不变；拒绝后仍可用
// 此前的当前版本号提交合法正文完成一次真正的更正。
func TestCorrectRecordRejectsInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "无效UTF8更正患者")
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
	v2, err := s.CorrectRecord(doc, r.ID, 1, "复诊：高血压 I10（控制稳定）", "复核更新")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	histBefore := findHistory(chartBefore, r.ID)
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	readBefore, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}

	// 所有坏序列（出现在开头/中间/结尾、夹在中文之间、单个坏字节等）
	// 都用当前版本号 2 提交：一律 ErrInvalidArgument，历史与审计原样。
	for _, tc := range invalidContents {
		if _, err := s.CorrectRecord(doc, r.ID, 2, tc.content, "更正原因合法"); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: CorrectRecord err = %v, want ErrInvalidArgument", tc.label, err)
		}
		chart, err := s.Chart(doc, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		hist := findHistory(chart, r.ID)
		if hist == nil || hist.CurrentVersion == nil {
			t.Fatalf("%s: history missing", tc.label)
		}
		cur := hist.CurrentVersion
		if cur.ID != v2.ID || cur.Number != 2 || cur.Content != v2.Content ||
			cur.PrevID != v1.ID || cur.Reason != v2.Reason {
			t.Fatalf("%s: current version changed: %+v", tc.label, cur)
		}
		if hist.Record.CurrentVersionID != v2.ID || len(hist.Versions) != 2 ||
			!reflect.DeepEqual(hist.Record.Versions, []ID{v1.ID, v2.ID}) ||
			!reflect.DeepEqual(hist.Versions[0], v1) || !reflect.DeepEqual(hist.Versions[1], v2) {
			t.Fatalf("%s: version history changed: %+v", tc.label, hist)
		}
		audit, err := s.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: correction audit added on rejected correction", tc.label)
		}
		res, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res, readBefore) {
			t.Fatalf("%s: receiver read changed:\nbefore: %+v\nafter:  %+v", tc.label, readBefore, res)
		}
	}

	// 拒绝后重开：当前版本、旧版本与版本关系仍是原来的结果。
	s = reopenStore(t, s, dir, clk)
	if got := findHistory(mustChart(t, s, p.ID), r.ID); !reflect.DeepEqual(got, histBefore) {
		t.Fatalf("history changed after rejected corrections + reopen:\nbefore: %+v\nafter:  %+v", histBefore, got)
	}

	// 空串与全空白正文继续按既有规则拒绝，历史不变。
	for _, blank := range []string{"", "  \n\t ", "　"} {
		if _, err := s.CorrectRecord(doc, r.ID, 2, blank, "原因"); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("blank correction %q err = %v, want ErrInvalidArgument", blank, err)
		}
	}
	if got := findHistory(mustChart(t, s, p.ID), r.ID); !reflect.DeepEqual(got, histBefore) {
		t.Fatalf("blank correction changed history: %+v", got)
	}

	// 失败的更正没有推进版本号：仍用此前的当前版本号 2 提交合法正文，
	// 成功生成紧接 v2 的第 3 版，只新增这一次真正完成的更正审计。
	clk.t = clk.t.Add(time.Hour)
	valid := "三次复诊：血压 118/76 🩸，e\u0301 组合\n\"引号\" C:\\路径 \ufffd 随访  "
	v3, err := s.CorrectRecord(doc, r.ID, 2, valid, "坏字节拒绝后的合法更正")
	if err != nil {
		t.Fatalf("valid correction after rejections: %v", err)
	}
	if v3.Number != 3 || v3.PrevID != v2.ID || v3.Content != valid ||
		v3.Reason != "坏字节拒绝后的合法更正" || !v3.CreatedAt.Equal(clk.t) {
		t.Fatalf("unexpected v3: %+v", v3)
	}

	chart, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, r.ID)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != v3.ID ||
		len(hist.Versions) != 3 ||
		!reflect.DeepEqual(hist.Versions[0], v1) || !reflect.DeepEqual(hist.Versions[1], v2) ||
		!reflect.DeepEqual(hist.Versions[2], v3) {
		t.Fatalf("history after valid correction: %+v", hist)
	}
	audit, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(auditBefore)+1 || !reflect.DeepEqual(audit[:len(auditBefore)], auditBefore) {
		t.Fatalf("audit after valid correction:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	last := audit[len(audit)-1]
	if last.Action != ActionCorrected || last.ObjectID != r.ID || last.ActorID != doc.ID {
		t.Fatalf("unexpected new audit event: %+v", last)
	}

	// 合法正文的既有版本号校验继续成立：过期版本号返回 ErrConflict，
	// 状态与审计不变。
	if _, err := s.CorrectRecord(doc, r.ID, 2, "用过期版本号更正", "过期"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version correction err = %v, want ErrConflict", err)
	}
	chart, err = s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hist = findHistory(chart, r.ID); hist.CurrentVersion.ID != v3.ID || len(hist.Versions) != 3 {
		t.Fatalf("rejected stale correction changed history: %+v", hist)
	}
	audit2, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit2, audit) {
		t.Fatal("rejected stale correction added an audit event")
	}

	// 接收方读到新的当前版本，合法正文逐字一致。
	res, err := s.Read(rcv, p.ID, e.ID, Diagnosis)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 1 || res.Records[0].VersionID != v3.ID ||
		res.Records[0].Version != 3 || res.Records[0].Content != valid {
		t.Fatalf("receiver read after valid correction: %+v", res.Records)
	}

	// 关闭后从同一位置重新打开：三个版本（含各自原文、PrevID 与原因）
	// 与审计完整保持，当前版本仍是逐字原文的第 3 版。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	chart, err = s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist = findHistory(chart, r.ID)
	if hist == nil || len(hist.Versions) != 3 {
		t.Fatalf("history after reopen: %+v", hist)
	}
	if !reflect.DeepEqual(hist.Versions[0], v1) || !reflect.DeepEqual(hist.Versions[1], v2) {
		t.Fatalf("older versions changed after reopen: %+v", hist.Versions)
	}
	if got := hist.Versions[2]; got.Content != valid || got.PrevID != v2.ID ||
		got.Reason != "坏字节拒绝后的合法更正" {
		t.Fatalf("v3 after reopen mismatch: %+v", got)
	}
	audit3, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit3, audit) {
		t.Fatal("audit events changed after reopen")
	}
}

// TestInvalidUTF8RejectedWhenPatientDeactivated 保证 UTF-8 校验先行：
// 患者停用后夹带坏字节的写操作仍返回 ErrInvalidArgument（而非以停用错误
// 落盘或静默保存），且状态不变。
func TestInvalidUTF8RejectedWhenPatientDeactivated(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	r, err := s.CreateDraft(doc, pid, eid, Order, "医嘱草稿原文")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ActivateRecord(doc, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	if err := s.DeactivatePatient(doc, pid); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateDraft(doc, pid, eid, Order, "停用后\xff坏草稿"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("deactivated create with bad utf8 err = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.UpdateDraft(doc, r.ID, "停用后\xff坏草稿"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("update active record with bad utf8 err = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.CorrectRecord(doc, r.ID, 1, "停用后\xff坏更正", "原因"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("deactivated correct with bad utf8 err = %v, want ErrInvalidArgument", err)
	}
	chart, err := s.Chart(doc, pid)
	if err != nil {
		t.Fatal(err)
	}
	hist := findHistory(chart, r.ID)
	if hist == nil || hist.CurrentVersion == nil || hist.CurrentVersion.ID != v1.ID ||
		len(hist.Versions) != 1 {
		t.Fatalf("bad-utf8 writes after deactivation changed history: %+v", hist)
	}
}

// reopenStore 关闭现有句柄并从同一目录重新打开（沿用注入时钟）。
func reopenStore(t *testing.T, s *Store, dir string, clk *fakeClock) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(dir, WithClock(func() time.Time { return clk.t }))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return s2
}

func mustChart(t *testing.T, s *Store, patientID ID) PatientChart {
	t.Helper()
	chart, err := s.Chart(doc, patientID)
	if err != nil {
		t.Fatalf("chart: %v", err)
	}
	return chart
}
