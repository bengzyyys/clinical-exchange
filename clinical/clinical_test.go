package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeClock 用于固定存储的判定时间。
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedPatient(t *testing.T, s *Store, name string) *Patient {
	t.Helper()
	p, err := s.RegisterPatient("dr.lee", name)
	if err != nil {
		t.Fatalf("RegisterPatient 失败: %v", err)
	}
	return p
}

func seedVisit(t *testing.T, s *Store, patientID, label string) *Visit {
	t.Helper()
	v, err := s.AddVisit("dr.lee", patientID, label)
	if err != nil {
		t.Fatalf("AddVisit 失败: %v", err)
	}
	return v
}

// TestOpenEmptyDirectory 空目录打开后状态为空。
func TestOpenEmptyDirectory(t *testing.T) {
	s := newTestStore(t)
	if got := s.ListPatients(); len(got) != 0 {
		t.Fatalf("新存储应为空，得到 %d 位患者", len(got))
	}
}

// TestRegisterAndVisit 登记患者与就诊，稳定标识可重复获取。
func TestRegisterAndVisit(t *testing.T) {
	s := newTestStore(t)
	p := seedPatient(t, s, "张三")
	if p.ID == "" || !p.Active {
		t.Fatalf("患者档案异常: %+v", p)
	}
	v := seedVisit(t, s, p.ID, "初诊")
	if v.ID == "" || v.PatientID != p.ID {
		t.Fatalf("就诊异常: %+v", v)
	}
	got, err := s.GetPatient(p.ID)
	if err != nil || got.ID != p.ID {
		t.Fatalf("按标识获取患者失败: %v", err)
	}
	visits, err := s.ListVisits(p.ID)
	if err != nil || len(visits) != 1 || visits[0].ID != v.ID {
		t.Fatalf("就诊列表异常: %+v, %v", visits, err)
	}
}

// TestReferenceIntegrity 引用不存在对象或混用不同患者数据时明确失败，且无副作用。
func TestReferenceIntegrity(t *testing.T) {
	s := newTestStore(t)
	p1 := seedPatient(t, s, "张三")
	p2 := seedPatient(t, s, "李四")
	v2 := seedVisit(t, s, p2.ID, "复诊")

	// 就诊不存在。
	if _, err := s.AddVisit("dr.lee", "p_missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的患者应返回 ErrNotFound，得到 %v", err)
	}
	// 就诊属于其他患者：保存草稿失败。
	if _, err := s.SaveDraft("dr.lee", p1.ID, v2.ID, KindDiagnosis, "感冒"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("跨患者就诊应返回 ErrInvalid，得到 %v", err)
	}
	// 失败操作不产生记录。
	if recs, _ := s.ListRecords(p1.ID); len(recs) != 0 {
		t.Fatalf("失败操作产生了残留记录: %+v", recs)
	}
	if recs, _ := s.ListRecords(p2.ID); len(recs) != 0 {
		t.Fatalf("失败操作在另一患者下产生了记录: %+v", recs)
	}
	// 记录不属于该患者。
	rec, err := s.SaveDraft("dr.lee", p2.ID, v2.ID, KindDiagnosis, "咳嗽")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRecord(p1.ID, rec.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("跨患者记录读取应返回 ErrInvalid，得到 %v", err)
	}
	if err := s.DeleteDraft("dr.lee", p1.ID, rec.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("跨患者草稿删除应返回 ErrInvalid，得到 %v", err)
	}
	if _, err := s.Activate("dr.lee", p1.ID, rec.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("跨患者生效应返回 ErrInvalid，得到 %v", err)
	}
}

// TestDraftLifecycle 草稿增改删与生效：生效保存当时完整内容与时间，生效后草稿清空。
func TestDraftLifecycle(t *testing.T) {
	s := newTestStore(t)
	p := seedPatient(t, s, "张三")
	v := seedVisit(t, s, p.ID, "初诊")

	rec, err := s.SaveDraft("dr.lee", p.ID, v.ID, KindDiagnosis, "感冒")
	if err != nil {
		t.Fatal(err)
	}
	// 草稿对接收方不可见（无授权时直接拒绝）。
	if _, err := s.ReadForRecipient("pharmacy", p.ID, []string{v.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("无授权读取应返回 ErrNotAuthorized，得到 %v", err)
	}
	// 草稿可修改：同就诊同类别更新同一条草稿。
	rec2, err := s.SaveDraft("dr.lee", p.ID, v.ID, KindDiagnosis, "重感冒")
	if err != nil {
		t.Fatal(err)
	}
	if rec2.ID != rec.ID {
		t.Fatalf("更新草稿应作用于同一条记录，得到 %s vs %s", rec2.ID, rec.ID)
	}
	// 生效。
	ver, err := s.Activate("dr.lee", p.ID, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ver.Version != 1 || ver.Content != "重感冒" {
		t.Fatalf("生效版本异常: %+v", ver)
	}
	if ver.CreatedAt.IsZero() || ver.Operator != "dr.lee" {
		t.Fatalf("生效版本缺少时间或操作身份: %+v", ver)
	}
	got, err := s.GetRecord(p.ID, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Draft != nil || got.CurrentVersion != 1 || len(got.Versions) != 1 {
		t.Fatalf("生效后应无草稿且仅 1 个版本: %+v", got)
	}
	// 生效记录不得删除。
	if err := s.DeleteDraft("dr.lee", p.ID, rec.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("生效记录删除应返回 ErrInvalid，得到 %v", err)
	}
	// 重复生效失败。
	if _, err := s.Activate("dr.lee", p.ID, rec.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("重复生效应返回 ErrInvalid，得到 %v", err)
	}
	// 草稿删除：新建一条草稿后删除，记录消失。
	order, err := s.SaveDraft("dr.lee", p.ID, v.ID, KindOrder, "多喝水")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDraft("dr.lee", p.ID, order.ID); err != nil {
		t.Fatalf("删除草稿失败: %v", err)
	}
	if _, err := s.GetRecord(p.ID, order.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后记录应不存在，得到 %v", err)
	}
}

// TestCorrection 更正：原因必填、指明当前版本、成功生成新版本并保留历史；
// 版本已变更时拒绝并保持现状。
func TestCorrection(t *testing.T) {
	s := newTestStore(t)
	p := seedPatient(t, s, "张三")
	v := seedVisit(t, s, p.ID, "初诊")
	rec, err := s.SaveDraft("dr.lee", p.ID, v.ID, KindDiagnosis, "感冒")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate("dr.lee", p.ID, rec.ID); err != nil {
		t.Fatal(err)
	}

	// 更正原因不能为空。
	if _, err := s.Correct("dr.lee", p.ID, rec.ID, 1, "", "流感"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空原因应返回 ErrInvalid，得到 %v", err)
	}
	// 内容不能为空。
	if _, err := s.Correct("dr.lee", p.ID, rec.ID, 1, "病情变化", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空内容应返回 ErrInvalid，得到 %v", err)
	}
	// 指明的版本已不是当前版本：拒绝。
	if _, err := s.Correct("dr.lee", p.ID, rec.ID, 99, "病情变化", "流感"); !errors.Is(err, ErrConflict) {
		t.Fatalf("过期版本应返回 ErrConflict，得到 %v", err)
	}
	// 拒绝后状态不变。
	got, _ := s.GetRecord(p.ID, rec.ID)
	if got.CurrentVersion != 1 || len(got.Versions) != 1 {
		t.Fatalf("更正被拒绝后应保持现状: %+v", got)
	}
	// 成功更正为第 2 版。
	ver, err := s.Correct("dr.lee", p.ID, rec.ID, 1, "病情变化", "流感")
	if err != nil {
		t.Fatal(err)
	}
	if ver.Version != 2 || ver.Content != "流感" || ver.Reason != "病情变化" {
		t.Fatalf("更正版本异常: %+v", ver)
	}
	got, _ = s.GetRecord(p.ID, rec.ID)
	if got.CurrentVersion != 2 || len(got.Versions) != 2 {
		t.Fatalf("更正后应有 2 个版本: %+v", got)
	}
	if got.Versions[0].Content != "感冒" || got.Versions[1].Reason != "病情变化" {
		t.Fatalf("旧内容与更正原因应保留: %+v", got.Versions)
	}
	// 再更正为第 3 版，版本关系连续。
	if _, err := s.Correct("dr.lee", p.ID, rec.ID, 2, "化验确认", "甲型流感"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRecord(p.ID, rec.ID)
	if got.CurrentVersion != 3 || len(got.Versions) != 3 {
		t.Fatalf("应有 3 个版本: %+v", got)
	}
}

// TestAuthorizationValidation 授权范围校验：空范围、跨患者就诊、开始不早于截止。
func TestAuthorizationValidation(t *testing.T) {
	s := newTestStore(t)
	p1 := seedPatient(t, s, "张三")
	p2 := seedPatient(t, s, "李四")
	v1 := seedVisit(t, s, p1.ID, "初诊")
	v2 := seedVisit(t, s, p2.ID, "复诊")
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

	// 空就诊范围。
	if _, err := s.CreateAuthorization("dr.lee", p1.ID, "pharmacy", nil, []RecordKind{KindDiagnosis}, base, base.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空就诊范围应返回 ErrInvalid，得到 %v", err)
	}
	// 空类别范围。
	if _, err := s.CreateAuthorization("dr.lee", p1.ID, "pharmacy", []string{v1.ID}, nil, base, base.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空类别范围应返回 ErrInvalid，得到 %v", err)
	}
	// 跨患者就诊。
	if _, err := s.CreateAuthorization("dr.lee", p1.ID, "pharmacy", []string{v2.ID}, []RecordKind{KindDiagnosis}, base, base.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("跨患者就诊应返回 ErrInvalid，得到 %v", err)
	}
	// 开始不早于截止。
	if _, err := s.CreateAuthorization("dr.lee", p1.ID, "pharmacy", []string{v1.ID}, []RecordKind{KindDiagnosis}, base, base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("开始等于截止应返回 ErrInvalid，得到 %v", err)
	}
	if _, err := s.CreateAuthorization("dr.lee", p1.ID, "pharmacy", []string{v1.ID}, []RecordKind{KindDiagnosis}, base.Add(time.Hour), base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("开始晚于截止应返回 ErrInvalid，得到 %v", err)
	}
	// 不存在的就诊。
	if _, err := s.CreateAuthorization("dr.lee", p1.ID, "pharmacy", []string{"v_missing"}, []RecordKind{KindDiagnosis}, base, base.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的就诊应返回 ErrNotFound，得到 %v", err)
	}
	// 校验失败不产生授权。
	if auths, _ := s.ListAuthorizations(p1.ID); len(auths) != 0 {
		t.Fatalf("失败的授权创建产生了残留: %+v", auths)
	}
}

// TestRecipientRead 接收方读取：只返回有效授权覆盖的当前版本；
// 未开始/已到期/已撤回/无授权均明确拒绝；多个授权分别判断。
func TestRecipientRead(t *testing.T) {
	s := newTestStore(t)
	fc := &fakeClock{t: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)}
	s.now = fc.now
	p := seedPatient(t, s, "张三")
	v1 := seedVisit(t, s, p.ID, "初诊")
	v2 := seedVisit(t, s, p.ID, "复诊")

	d1, err := s.SaveDraft("dr.lee", p.ID, v1.ID, KindDiagnosis, "感冒")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate("dr.lee", p.ID, d1.ID); err != nil {
		t.Fatal(err)
	}
	o1, err := s.SaveDraft("dr.lee", p.ID, v1.ID, KindOrder, "多喝水")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate("dr.lee", p.ID, o1.ID); err != nil {
		t.Fatal(err)
	}
	d2, err := s.SaveDraft("dr.lee", p.ID, v2.ID, KindDiagnosis, "咳嗽")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate("dr.lee", p.ID, d2.ID); err != nil {
		t.Fatal(err)
	}

	base := fc.t
	// 覆盖 v1 诊断与医嘱的授权。
	a1, err := s.CreateAuthorization("dr.lee", p.ID, "pharmacy", []string{v1.ID}, []RecordKind{KindDiagnosis, KindOrder}, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 覆盖 v2 诊断的授权。
	a2, err := s.CreateAuthorization("dr.lee", p.ID, "pharmacy", []string{v2.ID}, []RecordKind{KindDiagnosis}, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadForRecipient("pharmacy", p.ID, []string{v1.ID, v2.ID}, []RecordKind{KindDiagnosis, KindOrder})
	if err != nil {
		t.Fatalf("有效授权下读取失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应返回 3 条当前生效记录，得到 %d: %+v", len(got), got)
	}
	// 只返回当前版本，看不到草稿/旧版本/更正原因。
	for _, r := range got {
		if r.Version != 1 || r.Content == "" {
			t.Fatalf("接收方记录异常: %+v", r)
		}
	}

	// 授权允许读取范围内后来更正后的记录（第 2 版）。
	if _, err := s.Correct("dr.lee", p.ID, d1.ID, 1, "病情变化", "流感"); err != nil {
		t.Fatal(err)
	}
	got, err = s.ReadForRecipient("pharmacy", p.ID, []string{v1.ID}, []RecordKind{KindDiagnosis})
	if err != nil || len(got) != 1 {
		t.Fatalf("更正后读取失败: %v %+v", err, got)
	}
	if got[0].Content != "流感" || got[0].Version != 2 {
		t.Fatalf("接收方应读到当前第 2 版，得到 %+v", got)
	}
	// 旧版本与更正原因对接收方不可见：返回结构中不含这些字段之外的信息。
	if got[0].Content == "感冒" {
		t.Fatal("接收方不应看到旧版本内容")
	}

	// 未开始的授权：整体拒绝且不携带内容。
	fc.t = base.Add(-2 * time.Hour)
	if _, err := s.ReadForRecipient("pharmacy", p.ID, []string{v1.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("授权未开始应返回 ErrNotAuthorized，得到 %v", err)
	}
	// 到期时刻失效。
	fc.t = base.Add(time.Hour)
	if _, err := s.ReadForRecipient("pharmacy", p.ID, []string{v1.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("授权到期应返回 ErrNotAuthorized，得到 %v", err)
	}
	// 恢复有效时间。
	fc.t = base

	// 撤回其中一个授权：不影响另一个仍然有效的授权。
	if _, err := s.RevokeAuthorization("dr.lee", p.ID, a1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadForRecipient("pharmacy", p.ID, []string{v1.ID}, []RecordKind{KindDiagnosis, KindOrder}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("a1 撤回后 v1 范围应整体拒绝，得到 %v", err)
	}
	got, err = s.ReadForRecipient("pharmacy", p.ID, []string{v2.ID}, []RecordKind{KindDiagnosis})
	if err != nil || len(got) != 1 {
		t.Fatalf("a2 仍有效，应读到 v2 诊断，得到 %v %+v", err, got)
	}
	// 重复撤回幂等，无额外变化。
	before, _ := s.ListAuditEvents(p.ID)
	again, err := s.RevokeAuthorization("dr.lee", p.ID, a1.ID)
	if err != nil || !again.Revoked {
		t.Fatalf("重复撤回应返回已撤回结果: %v", err)
	}
	after, _ := s.ListAuditEvents(p.ID)
	if len(before) != len(after) {
		t.Fatalf("重复撤回不应产生新审计事件")
	}
	// 全部撤回后整体拒绝。
	if _, err := s.RevokeAuthorization("dr.lee", p.ID, a2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadForRecipient("pharmacy", p.ID, []string{v1.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("授权全部撤回应返回 ErrNotAuthorized，得到 %v", err)
	}
	// 从未授权的接收方同样明确拒绝。
	if _, err := s.ReadForRecipient("nobody", p.ID, []string{v1.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("无对应授权应返回 ErrNotAuthorized，得到 %v", err)
	}
}

// TestAuthorizationScope 授权不自动扩大到新增就诊；跨患者读取拒绝。
func TestAuthorizationScope(t *testing.T) {
	s := newTestStore(t)
	fc := &fakeClock{t: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)}
	s.now = fc.now
	p1 := seedPatient(t, s, "张三")
	p2 := seedPatient(t, s, "李四")
	v1 := seedVisit(t, s, p1.ID, "初诊")
	vOther := seedVisit(t, s, p1.ID, "其他")
	v2 := seedVisit(t, s, p2.ID, "复诊")
	base := fc.t

	d1, _ := s.SaveDraft("dr.lee", p1.ID, v1.ID, KindDiagnosis, "感冒")
	if _, err := s.Activate("dr.lee", p1.ID, d1.ID); err != nil {
		t.Fatal(err)
	}
	dOther, _ := s.SaveDraft("dr.lee", p1.ID, vOther.ID, KindDiagnosis, "外伤")
	if _, err := s.Activate("dr.lee", p1.ID, dOther.ID); err != nil {
		t.Fatal(err)
	}
	d2, _ := s.SaveDraft("dr.lee", p2.ID, v2.ID, KindDiagnosis, "咳嗽")
	if _, err := s.Activate("dr.lee", p2.ID, d2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAuthorization("dr.lee", p1.ID, "pharmacy", []string{v1.ID}, []RecordKind{KindDiagnosis}, base.Add(-time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 授权只覆盖 v1：新增的 vOther 不自动纳入。
	if _, err := s.ReadForRecipient("pharmacy", p1.ID, []string{vOther.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("新增就诊不应自动纳入授权，得到 %v", err)
	}
	// 跨患者：pharmacy 对 p2 无授权，拒绝且不携带内容。
	if _, err := s.ReadForRecipient("pharmacy", p2.ID, []string{v2.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("跨患者无授权应返回 ErrNotAuthorized，得到 %v", err)
	}
	// 部分覆盖：返回被覆盖的内容，未覆盖类别不出现。
	got, err := s.ReadForRecipient("pharmacy", p1.ID, []string{v1.ID}, []RecordKind{KindDiagnosis, KindOrder})
	if err != nil || len(got) != 1 {
		t.Fatalf("部分覆盖应只返回覆盖内容，得到 %v %+v", err, got)
	}
}

// TestDeactivatePatient 停用后禁止一切变更与接收方读取，内部仍可查看历史；
// 重复停用幂等。
func TestDeactivatePatient(t *testing.T) {
	s := newTestStore(t)
	fc := &fakeClock{t: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)}
	s.now = fc.now
	p := seedPatient(t, s, "张三")
	v := seedVisit(t, s, p.ID, "初诊")
	rec, err := s.SaveDraft("dr.lee", p.ID, v.ID, KindDiagnosis, "感冒")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate("dr.lee", p.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	base := fc.t
	if _, err := s.CreateAuthorization("dr.lee", p.ID, "pharmacy", []string{v.ID}, []RecordKind{KindDiagnosis}, base.Add(-time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DeactivatePatient("dr.lee", p.ID); err != nil {
		t.Fatal(err)
	}
	// 停用后禁止新增就诊、改动草稿、生效、更正、新建授权。
	if _, err := s.AddVisit("dr.lee", p.ID, "复诊"); !errors.Is(err, ErrPatientInactive) {
		t.Fatalf("停用后新增就诊应返回 ErrPatientInactive，得到 %v", err)
	}
	if _, err := s.SaveDraft("dr.lee", p.ID, v.ID, KindOrder, "x"); !errors.Is(err, ErrPatientInactive) {
		t.Fatalf("停用后保存草稿应返回 ErrPatientInactive，得到 %v", err)
	}
	if _, err := s.Correct("dr.lee", p.ID, rec.ID, 1, "原因", "内容"); !errors.Is(err, ErrPatientInactive) {
		t.Fatalf("停用后更正应返回 ErrPatientInactive，得到 %v", err)
	}
	if _, err := s.CreateAuthorization("dr.lee", p.ID, "pharmacy", []string{v.ID}, []RecordKind{KindDiagnosis}, base.Add(-time.Hour), base.Add(2*time.Hour)); !errors.Is(err, ErrPatientInactive) {
		t.Fatalf("停用后新建授权应返回 ErrPatientInactive，得到 %v", err)
	}
	// 接收方不能继续读取。
	if _, err := s.ReadForRecipient("pharmacy", p.ID, []string{v.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrPatientInactive) {
		t.Fatalf("停用后接收方读取应返回 ErrPatientInactive，得到 %v", err)
	}
	// 内部使用者仍可查看历史。
	if got, err := s.GetRecord(p.ID, rec.ID); err != nil || got.CurrentVersion != 1 {
		t.Fatalf("停用后内部仍可查看历史: %v %+v", err, got)
	}
	auths, err := s.ListAuthorizations(p.ID)
	if err != nil || len(auths) != 1 {
		t.Fatalf("停用后内部仍可查看授权: %v %+v", err, auths)
	}
	// 撤回授权属于内部维护操作，停用后仍允许。
	if _, err := s.RevokeAuthorization("dr.lee", p.ID, auths[0].ID); err != nil {
		t.Fatalf("停用后撤回授权应允许: %v", err)
	}
	// 重复停用幂等：无额外审计事件。
	before, _ := s.ListAuditEvents(p.ID)
	again, err := s.DeactivatePatient("dr.lee", p.ID)
	if err != nil || again.Active {
		t.Fatalf("重复停用应返回已停用结果: %v", err)
	}
	after, _ := s.ListAuditEvents(p.ID)
	if len(before) != len(after) {
		t.Fatalf("重复停用不应产生新审计事件")
	}
}

// TestAuditTrail 生效、更正、授权创建与撤回、停用均留痕，按患者可查；
// 失败操作不产生事件。
func TestAuditTrail(t *testing.T) {
	s := newTestStore(t)
	p := seedPatient(t, s, "张三")
	v := seedVisit(t, s, p.ID, "初诊")
	rec, err := s.SaveDraft("dr.lee", p.ID, v.ID, KindDiagnosis, "感冒")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate("dr.lee", p.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Correct("dr.lee", p.ID, rec.ID, 1, "病情变化", "流感"); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	a, err := s.CreateAuthorization("dr.lee", p.ID, "pharmacy", []string{v.ID}, []RecordKind{KindDiagnosis}, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeAuthorization("dr.lee", p.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeactivatePatient("dr.lee", p.ID); err != nil {
		t.Fatal(err)
	}

	events, err := s.ListAuditEvents(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantActions := []string{"record.activate", "record.correct", "authorization.create", "authorization.revoke", "patient.deactivate"}
	if len(events) != len(wantActions) {
		t.Fatalf("应有 %d 个审计事件，得到 %d: %+v", len(wantActions), len(events), events)
	}
	for i, want := range wantActions {
		if events[i].Action != want {
			t.Fatalf("第 %d 个事件应为 %s，得到 %s", i, want, events[i].Action)
		}
		if events[i].Operator != "dr.lee" || events[i].Time.IsZero() || events[i].Object == "" {
			t.Fatalf("审计事件缺少操作身份/时间/对象: %+v", events[i])
		}
	}

	// 失败操作不产生事件。
	nBefore := len(events)
	if _, err := s.Correct("dr.lee", p.ID, rec.ID, 1, "原因", "内容"); !errors.Is(err, ErrPatientInactive) {
		t.Fatalf("停用后更正应失败，得到 %v", err)
	}
	if _, err := s.SaveDraft("dr.lee", p.ID, v.ID, KindOrder, "x"); err == nil {
		t.Fatal("停用后保存草稿应失败")
	}
	events, _ = s.ListAuditEvents(p.ID)
	if len(events) != nBefore {
		t.Fatalf("失败操作不应产生审计事件，得到 %d vs %d", len(events), nBefore)
	}

	// 审计事件按患者隔离。
	p2 := seedPatient(t, s, "李四")
	events2, err := s.ListAuditEvents(p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events2) != 0 {
		t.Fatalf("患者隔离失败：其他患者出现 %d 个事件", len(events2))
	}
}

// TestReopenPreservesState 关闭后从同一位置重新打开：档案、版本、授权状态与审计历史保留；
// 到期判断按本次读取时间计算。
func TestReopenPreservesState(t *testing.T) {
	dir := t.TempDir()
	fc := &fakeClock{t: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)}

	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s1.now = fc.now
	p := seedPatient(t, s1, "张三")
	v := seedVisit(t, s1, p.ID, "初诊")
	rec, err := s1.SaveDraft("dr.lee", p.ID, v.ID, KindDiagnosis, "感冒")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Activate("dr.lee", p.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Correct("dr.lee", p.ID, rec.ID, 1, "病情变化", "流感"); err != nil {
		t.Fatal(err)
	}
	base := fc.t
	a, err := s1.CreateAuthorization("dr.lee", p.ID, "pharmacy", []string{v.ID}, []RecordKind{KindDiagnosis}, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// 从同一位置重新打开。
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()

	// 档案保留。
	gotP, err := s2.GetPatient(p.ID)
	if err != nil || gotP.Name != "张三" || !gotP.Active {
		t.Fatalf("重开后患者档案异常: %v %+v", err, gotP)
	}
	// 版本历史保留。
	gotR, err := s2.GetRecord(p.ID, rec.ID)
	if err != nil || gotR.CurrentVersion != 2 || len(gotR.Versions) != 2 {
		t.Fatalf("重开后版本历史异常: %v %+v", err, gotR)
	}
	if gotR.Versions[1].Reason != "病情变化" {
		t.Fatalf("重开后更正原因应保留")
	}
	// 授权状态保留。
	auths, err := s2.ListAuthorizations(p.ID)
	if err != nil || len(auths) != 1 || auths[0].ID != a.ID || auths[0].Revoked {
		t.Fatalf("重开后授权状态异常: %v %+v", err, auths)
	}
	// 审计历史保留。
	events, err := s2.ListAuditEvents(p.ID)
	if err != nil || len(events) != 3 {
		t.Fatalf("重开后审计历史异常: %v %d", err, len(events))
	}
	// 到期判断按本次读取时间：2 小时后授权已失效。
	fc.t = base.Add(2 * time.Hour)
	s2.now = fc.now
	if _, err := s2.ReadForRecipient("pharmacy", p.ID, []string{v.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("重开后按新读取时间应判定授权到期，得到 %v", err)
	}
	// 当前时间仍在窗口内时可读。
	fc.t = base
	got, err := s2.ReadForRecipient("pharmacy", p.ID, []string{v.ID}, []RecordKind{KindDiagnosis})
	if err != nil || len(got) != 1 || got[0].Content != "流感" {
		t.Fatalf("重开后在有效期内应读到当前版本，得到 %v %+v", err, got)
	}
}

// TestReopenPreservesRevokedAndDeactivated 重开后撤回与停用状态保留。
func TestReopenPreservesRevokedAndDeactivated(t *testing.T) {
	dir := t.TempDir()
	fc := &fakeClock{t: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)}

	s1, _ := Open(dir)
	s1.now = fc.now
	p := seedPatient(t, s1, "张三")
	v := seedVisit(t, s1, p.ID, "初诊")
	rec, _ := s1.SaveDraft("dr.lee", p.ID, v.ID, KindDiagnosis, "感冒")
	if _, err := s1.Activate("dr.lee", p.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	base := fc.t
	a, _ := s1.CreateAuthorization("dr.lee", p.ID, "pharmacy", []string{v.ID}, []RecordKind{KindDiagnosis}, base.Add(-time.Hour), base.Add(time.Hour))
	if _, err := s1.RevokeAuthorization("dr.lee", p.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.DeactivatePatient("dr.lee", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()

	gotP, _ := s2.GetPatient(p.ID)
	if gotP.Active || gotP.DeactivatedAt == nil {
		t.Fatalf("重开后停用状态应保留: %+v", gotP)
	}
	auths, _ := s2.ListAuthorizations(p.ID)
	if !auths[0].Revoked {
		t.Fatal("重开后撤回状态应保留")
	}
	// 停用后接收方仍不能读取。
	if _, err := s2.ReadForRecipient("pharmacy", p.ID, []string{v.ID}, []RecordKind{KindDiagnosis}); !errors.Is(err, ErrPatientInactive) {
		t.Fatalf("重开后停用患者仍应拒绝读取，得到 %v", err)
	}
	// 审计历史保留（生效、授权创建、撤回、停用 4 个事件）。
	events, _ := s2.ListAuditEvents(p.ID)
	if len(events) != 4 {
		t.Fatalf("重开后审计历史应有 4 个事件，得到 %d", len(events))
	}
}

// TestOpenInvalidDir 数据目录不可创建时明确失败。
func TestOpenInvalidDir(t *testing.T) {
	// 路径的父级是一个普通文件，MkdirAll 失败。
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(f, "sub")); err == nil {
		t.Fatal("在文件路径下创建存储应失败")
	}
}

// TestStableIdentifiers 各对象标识稳定且互不相同。
func TestStableIdentifiers(t *testing.T) {
	s := newTestStore(t)
	p := seedPatient(t, s, "张三")
	v := seedVisit(t, s, p.ID, "初诊")
	r, _ := s.SaveDraft("dr.lee", p.ID, v.ID, KindDiagnosis, "感冒")
	a, _ := s.CreateAuthorization("dr.lee", p.ID, "pharmacy", []string{v.ID}, []RecordKind{KindDiagnosis}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	ids := map[string]bool{p.ID: true, v.ID: true, r.ID: true, a.ID: true}
	if len(ids) != 4 {
		t.Fatalf("标识应互不相同: %v", ids)
	}
	for _, id := range []string{p.ID, v.ID, r.ID, a.ID} {
		if id == "" || len(id) < 10 {
			t.Fatalf("标识过短或为空: %q", id)
		}
	}
}
