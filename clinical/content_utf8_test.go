package clinical

import (
	"errors"
	"testing"
)

// 无效 UTF-8 正文样本：不完整的多字节字符、孤立的续字节，
// 分别出现在开头、中间、结尾，以及夹在合法中文之间。
var invalidUTF8Contents = []string{
	"诊断：\xc3\x28",            // 不完整的多字节序列
	"\xe4\xb8\xad文字\xe4\xb8", // 结尾截断的多字节字符
	"\x80孤立续字节",              // 开头孤立续字节
	"医嘱\x80内容",               // 中间孤立续字节
	"正文结尾\xff",               // 结尾非法字节
	"合法中文\xff\xfe夹带坏字节",      // 夹在合法中文之间
	"\xc2",                   // 只有半个多字节字符
}

func TestCreateDraftRejectsInvalidUTF8(t *testing.T) {
	for _, content := range invalidUTF8Contents {
		s, _ := newTestStore(t)
		pID, eID := setupPatientEncounter(t, s)

		_, err := s.CreateDraft(doc, pID, eID, Diagnosis, content)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("CreateDraft(%q): err = %v, want ErrInvalidArgument", content, err)
		}
		if n := countRecords(s); n != 0 {
			t.Fatalf("CreateDraft(%q): %d records left behind, want 0", content, n)
		}
		s.Close()
	}
}

func TestUpdateDraftRejectsInvalidUTF8AndKeepsDraft(t *testing.T) {
	for _, content := range invalidUTF8Contents {
		s, _ := newTestStore(t)
		pID, eID := setupPatientEncounter(t, s)
		r, err := s.CreateDraft(doc, pID, eID, Order, "原始草稿")
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}

		_, err = s.UpdateDraft(doc, r.ID, content)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("UpdateDraft(%q): err = %v, want ErrInvalidArgument", content, err)
		}
		chart, err := s.Chart(doc, pID)
		if err != nil {
			t.Fatalf("chart: %v", err)
		}
		h := findHistory(chart, r.ID)
		if h == nil || !h.HasDraft || h.DraftContent != "原始草稿" {
			t.Fatalf("UpdateDraft(%q): draft changed to %+v", content, h)
		}
		s.Close()
	}
}

func TestCorrectRecordRejectsInvalidUTF8AndKeepsHistory(t *testing.T) {
	for _, content := range invalidUTF8Contents {
		s, _ := newTestStore(t)
		pID, eID := setupPatientEncounter(t, s)
		r, err := s.CreateDraft(doc, pID, eID, Diagnosis, "首版内容")
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		v1, err := s.ActivateRecord(doc, r.ID)
		if err != nil {
			t.Fatalf("activate: %v", err)
		}

		_, err = s.CorrectRecord(doc, r.ID, v1.Number, content, "更正原因")
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("CorrectRecord(%q): err = %v, want ErrInvalidArgument", content, err)
		}

		chart, err := s.Chart(doc, pID)
		if err != nil {
			t.Fatalf("chart: %v", err)
		}
		h := findHistory(chart, r.ID)
		if h == nil || h.CurrentVersion == nil {
			t.Fatalf("CorrectRecord(%q): record history missing", content)
		}
		if len(h.Versions) != 1 || h.CurrentVersion.Number != 1 || h.CurrentVersion.Content != "首版内容" {
			t.Fatalf("CorrectRecord(%q): history changed: %+v", content, h.Versions)
		}
		if got := auditActions(chart.AuditEvents)[ActionCorrected]; got != 0 {
			t.Fatalf("CorrectRecord(%q): %d correction audits, want 0", content, got)
		}

		// 拒绝后仍可用此前的当前版本号提交合法正文，只新增这一次更正。
		v2, err := s.CorrectRecord(doc, r.ID, v1.Number, "更正后的合法内容", "更正原因")
		if err != nil {
			t.Fatalf("CorrectRecord(%q) retry with valid content: %v", content, err)
		}
		if v2.Number != 2 || v2.PrevID != v1.ID || v2.Reason != "更正原因" {
			t.Fatalf("CorrectRecord(%q) retry: version = %+v", content, v2)
		}
		chart, err = s.Chart(doc, pID)
		if err != nil {
			t.Fatalf("chart: %v", err)
		}
		h = findHistory(chart, r.ID)
		if len(h.Versions) != 2 || h.Versions[0].Content != "首版内容" || h.CurrentVersion.Number != 2 {
			t.Fatalf("CorrectRecord(%q) retry: history = %+v", content, h.Versions)
		}
		if got := auditActions(chart.AuditEvents)[ActionCorrected]; got != 1 {
			t.Fatalf("CorrectRecord(%q) retry: %d correction audits, want 1", content, got)
		}
		s.Close()
	}
}

// 合法 UTF-8 正文（含中文、表情、组合字符、换行、引号、反斜杠、首尾空白
// 以及用户明确输入的合法“�”字符）必须逐字节原样保存，关闭重开后不变。
func TestValidUTF8ContentPreservedByteForByte(t *testing.T) {
	contents := []string{
		"  患者自述头痛，伴有「眩晕」\n建议：静卧休息  ",
		"表情字符 😀🎉 与组合字符 é́",
		"引号\"双引号\"与'单引号'、反斜杠\\与路径C:\\temp",
		"用户明确输入的替换字符：�",
		"\t制表符与换行\n混排\r\n",
	}
	for _, content := range contents {
		dir := t.TempDir()
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		pID, eID := setupPatientEncounter(t, s)

		// 新建草稿 → 修改草稿 → 生效 → 更正，每一步的返回值都应是原文。
		r, err := s.CreateDraft(doc, pID, eID, Diagnosis, content)
		if err != nil {
			t.Fatalf("CreateDraft(%q): %v", content, err)
		}
		if r.DraftContent != content {
			t.Fatalf("CreateDraft(%q): stored %q", content, r.DraftContent)
		}
		r, err = s.UpdateDraft(doc, r.ID, content)
		if err != nil {
			t.Fatalf("UpdateDraft(%q): %v", content, err)
		}
		if r.DraftContent != content {
			t.Fatalf("UpdateDraft(%q): stored %q", content, r.DraftContent)
		}
		v1, err := s.ActivateRecord(doc, r.ID)
		if err != nil {
			t.Fatalf("ActivateRecord(%q): %v", content, err)
		}
		if v1.Content != content {
			t.Fatalf("ActivateRecord(%q): version content %q", content, v1.Content)
		}
		v2, err := s.CorrectRecord(doc, r.ID, v1.Number, content, "核对")
		if err != nil {
			t.Fatalf("CorrectRecord(%q): %v", content, err)
		}
		if v2.Content != content {
			t.Fatalf("CorrectRecord(%q): version content %q", content, v2.Content)
		}

		// 当次查询即为原文。
		chart, err := s.Chart(doc, pID)
		if err != nil {
			t.Fatalf("chart: %v", err)
		}
		if h := findHistory(chart, r.ID); h == nil || h.CurrentVersion.Content != content ||
			h.Versions[0].Content != content || h.Versions[1].Content != content {
			t.Fatalf("Chart(%q): history = %+v", content, h)
		}

		// 关闭后从同一位置重新打开，仍应看到逐字节相同的原文。
		if err := s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		chart2, err := s2.Chart(doc, pID)
		if err != nil {
			t.Fatalf("chart after reopen: %v", err)
		}
		h := findHistory(chart2, r.ID)
		if h == nil || h.CurrentVersion.Content != content ||
			h.Versions[0].Content != content || h.Versions[1].Content != content {
			t.Fatalf("after reopen (%q): history = %+v", content, h)
		}
		s2.Close()
	}
}
