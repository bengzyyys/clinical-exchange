package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖授权创建时接收方标识的 UTF-8 校验与保存一致性：接收方标识
// 夹带任何无效 UTF-8 字节（含孤立续字节、截断的多字节字符）时，Grant 与
// GrantSelective 必须整体返回 ErrInvalidArgument，返回空授权，不保存授权，
// 也不新增授权创建审计——不能靠静默替换（U+FFFD）、截断或跳过坏字节继续
// 授权，否则 JSON 落盘会把坏字节改写，重开后授权归到使用替换后标识的另一
// 接收方名下。合法 UTF-8 标识（含中文、其他多字节字符、首尾有空白但整体
// 非纯空白、用户明确输入的合法 U+FFFD）必须原样保存与精确匹配，不裁剪、
// 不改写、不合并看起来相似的标识。

// invalidReceiverIDs 是各类无效 UTF-8 接收方标识：坏序列出现在开头、结尾、
// 中间，孤立续字节，截断的多字节字符，overlong 编码与 UTF-16 代理项编码；
// 另有一例在合法 U+FFFD 之后紧跟非法字节——必须因那个坏字节被拒绝。
var invalidReceiverIDs = []struct {
	label      string
	receiverID string
}{
	{"开头非法字节", "\xffrcv-1"},
	{"结尾非法字节", "rcv-1\xff"},
	{"中间非法字节", "rcv\xff-1"},
	{"中文之间夹坏字节", "接收\xfe-1"},
	{"孤立续字节", "rcv-\x80"},
	{"结尾截断多字节字符", "接收-\xe6"},
	{"中间不完整多字节序列", "ab\xe6\x9dcd"},
	{"中文之后截断三字节序列", "接收方\xe7\xa1"},
	{"单个非法字节", "\x80"},
	{"overlong 编码", "\xc0\x80"},
	{"UTF-16 代理项编码", "接收\n\xed\xa0\x80\n"},
	{"合法替换字符后紧跟非法字节", "接收�\xff"},
}

// validReceiverIDs 是必须逐字原样保存与匹配的合法 UTF-8 接收方标识。
var validReceiverIDs = []struct {
	label      string
	receiverID string
}{
	{"纯英文数字", "rcv-0001"},
	{"中文标识", "接收方-甲-0001"},
	{"表情字符", "rcv-🏥-🧑‍⚕️"},
	{"组合字符", "réq-1"},
	{"含换行制表", "rcv\n第二行\t-1"},
	{"首尾空白保留", "  rcv-保留空白  "},
	{"用户明确输入的合法替换字符", "接收-�"},
}

// grantUTF8Fixture 是接收方标识 UTF-8 测试的夹具：一名患者、一次就诊与
// 一条已生效诊断记录。
type grantUTF8Fixture struct {
	pid ID
	eid ID
	rid ID
}

func setupGrantUTF8(t *testing.T, s *Store) grantUTF8Fixture {
	t.Helper()
	p, err := s.RegisterPatient(doc, "接收方UTF8患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断：高血压 I10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, d.ID); err != nil {
		t.Fatal(err)
	}
	return grantUTF8Fixture{pid: p.ID, eid: e.ID, rid: d.ID}
}

func grantedAuditCount(events []AuditEvent) int {
	n := 0
	for _, e := range events {
		if e.Action == ActionGranted {
			n++
		}
	}
	return n
}

// checkGrantRejected 断言一次被拒绝的授权创建：错误可识别为
// ErrInvalidArgument，返回的授权为空（无标识、无范围），且授权清单与
// 授权创建审计均未变化。
func checkGrantRejected(t *testing.T, s *Store, f grantUTF8Fixture, a Authorization, err error, auditBefore []AuditEvent) {
	t.Helper()
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("grant err = %v, want ErrInvalidArgument", err)
	}
	if a.ID != "" || a.ReceiverID != "" || len(a.Scopes) != 0 || len(a.Selections) != 0 {
		t.Fatalf("rejected grant returned non-empty authorization: %+v", a)
	}
	auths, err := s.ListAuthorizations(doc, f.pid, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 0 {
		t.Fatalf("rejected grant left authorizations: %+v", auths)
	}
	audit, err := s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit changed after rejected grant")
	}
}

// TestGrantRejectsInvalidUTF8ReceiverID 覆盖各类坏字节接收方标识：整类授权、
// 限定记录授权与二者混合的授权一律 ErrInvalidArgument，返回空授权，不保存
// 授权、不新增授权创建审计；关闭重开后授权清单与审计仍是拒绝前的结果。
func TestGrantRejectsInvalidUTF8ReceiverID(t *testing.T) {
	for _, tc := range invalidReceiverIDs {
		t.Run(tc.label, func(t *testing.T) {
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
			f := setupGrantUTF8(t, s)
			st := clk.t.Add(-time.Hour)
			en := clk.t.Add(24 * time.Hour)
			scopes := []Scope{{EncounterID: f.eid, Category: Diagnosis}}
			selections := []RecordSelection{{RecordID: f.rid, EncounterID: f.eid, Category: Diagnosis}}

			// 整类授权（Grant 公开入口）。
			auditBefore := mustAudit(t, s, f.pid)
			a, err := s.Grant(doc, f.pid, tc.receiverID, scopes, st, en)
			checkGrantRejected(t, s, f, a, err, auditBefore)

			// 限定记录授权。
			a, err = s.GrantSelective(doc, f.pid, tc.receiverID, nil, selections, st, en)
			checkGrantRejected(t, s, f, a, err, auditBefore)

			// 整类与限定混合的授权。
			a, err = s.GrantSelective(doc, f.pid, tc.receiverID, scopes, selections, st, en)
			checkGrantRejected(t, s, f, a, err, auditBefore)

			// 坏字节没有落盘：关闭重开后授权清单与审计仍是拒绝前的结果。
			s = reopenStore(t, s, dir, clk)
			auths, err := s.ListAuthorizations(doc, f.pid, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(auths) != 0 {
				t.Fatalf("rejected grant persisted after reopen: %+v", auths)
			}
			audit, err := s.AuditEvents(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after reopen")
			}

			// 被拒绝的坏标识不影响随后的合法授权：用合法标识可以正常创建。
			if _, err := s.Grant(doc, f.pid, "rcv-clean-after-reject", scopes, st, en); err != nil {
				t.Fatalf("grant after rejection: %v", err)
			}
			_ = s.Close()
		})
	}
}

// TestGrantValidUTF8ReceiverIDPreservedRoundTrip 覆盖合法接收方标识：原样
// 保存与精确匹配，不裁剪首尾空白、不改写字符形态；创建返回值、单条查询与
// 关闭重开后的清单中接收方标识逐字相同，按接收方筛选与接收方读取都只匹配
// 完全相同的标识。
func TestGrantValidUTF8ReceiverIDPreservedRoundTrip(t *testing.T) {
	for _, tc := range validReceiverIDs {
		t.Run(tc.label, func(t *testing.T) {
			dir := t.TempDir()
			clk := &fakeClock{t: time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)}
			open := func() *Store {
				s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return s
			}

			s := open()
			f := setupGrantUTF8(t, s)
			st := clk.t.Add(-time.Hour)
			en := clk.t.Add(24 * time.Hour)
			scopes := []Scope{{EncounterID: f.eid, Category: Diagnosis}}

			a, err := s.Grant(doc, f.pid, tc.receiverID, scopes, st, en)
			if err != nil {
				t.Fatalf("Grant(%q): %v", tc.receiverID, err)
			}
			if a.ReceiverID != tc.receiverID {
				t.Fatalf("returned receiver id = %q, want %q", a.ReceiverID, tc.receiverID)
			}

			// 单条查询：接收方标识逐字相同。
			got, err := s.GetAuthorization(doc, f.pid, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ReceiverID != tc.receiverID {
				t.Fatalf("GetAuthorization receiver id = %q, want %q", got.ReceiverID, tc.receiverID)
			}

			// 按接收方筛选：只匹配完全相同的标识。
			auths, err := s.ListAuthorizations(doc, f.pid, tc.receiverID)
			if err != nil {
				t.Fatal(err)
			}
			if len(auths) != 1 || auths[0].ID != a.ID || auths[0].ReceiverID != tc.receiverID {
				t.Fatalf("list by receiver = %+v, want only %q", auths, tc.receiverID)
			}

			// 接收方读取：以原标识身份可以读到授权内容。
			res, err := s.Read(ReceiverActor(tc.receiverID), f.pid, f.eid, Diagnosis)
			if err != nil {
				t.Fatalf("read as receiver: %v", err)
			}
			if len(res.Records) != 1 || res.Records[0].RecordID != f.rid {
				t.Fatalf("read records = %+v, want record %q", res.Records, f.rid)
			}

			// 关闭后从同一位置重新打开：清单中的接收方标识逐字保持，
			// 筛选与读取仍只认原标识。
			s = reopenStore(t, s, dir, clk)
			t.Cleanup(func() { _ = s.Close() })
			auths, err = s.ListAuthorizations(doc, f.pid, tc.receiverID)
			if err != nil {
				t.Fatal(err)
			}
			if len(auths) != 1 || auths[0].ID != a.ID || auths[0].ReceiverID != tc.receiverID {
				t.Fatalf("list after reopen = %+v, want only %q", auths, tc.receiverID)
			}
			got, err = s.GetAuthorization(doc, f.pid, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ReceiverID != tc.receiverID {
				t.Fatalf("GetAuthorization after reopen receiver id = %q, want %q", got.ReceiverID, tc.receiverID)
			}
			if _, err := s.Read(ReceiverActor(tc.receiverID), f.pid, f.eid, Diagnosis); err != nil {
				t.Fatalf("read as receiver after reopen: %v", err)
			}
		})
	}
}

// TestGrantInvalidReceiverIDDoesNotHitReplacementCharReceiver 覆盖撞名场景：
// 已有一条属于标识中含合法 U+FFFD 的接收方的有效授权时，用一个只有该位置
// 换成坏字节的标识申请相同范围必须失败——不能被替换后归到那个接收方名下；
// 原授权的标识、范围、期限与可读取内容不变，关闭重开后也不会多出归属于
// 该接收方的授权。
func TestGrantInvalidReceiverIDDoesNotHitReplacementCharReceiver(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 3, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	f := setupGrantUTF8(t, s)
	st := clk.t.Add(-time.Hour)
	en := clk.t.Add(24 * time.Hour)
	scopes := []Scope{{EncounterID: f.eid, Category: Diagnosis}}

	// 用户明确输入合法 U+FFFD 的标识可以正常授权。
	explicit := "接收-�"
	a1, err := s.Grant(doc, f.pid, explicit, scopes, st, en)
	if err != nil {
		t.Fatalf("grant with explicit U+FFFD: %v", err)
	}

	// 夹带非法字节的标识（替换后会变成 explicit）必须被拒绝，而不是
	// 归到那个接收方名下：返回 ErrInvalidArgument，授权清单与审计不变。
	auditBefore := mustAudit(t, s, f.pid)
	bad := "接收-\xff"
	for _, grant := range []func() (Authorization, error){
		func() (Authorization, error) { return s.Grant(doc, f.pid, bad, scopes, st, en) },
		func() (Authorization, error) {
			return s.GrantSelective(doc, f.pid, bad, nil,
				[]RecordSelection{{RecordID: f.rid, EncounterID: f.eid, Category: Diagnosis}}, st, en)
		},
	} {
		a, err := grant()
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid utf8 receiver id err = %v, want ErrInvalidArgument", err)
		}
		if a.ID != "" {
			t.Fatalf("rejected grant returned authorization %q", a.ID)
		}
	}
	auths, err := s.ListAuthorizations(doc, f.pid, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || auths[0].ID != a1.ID || auths[0].ReceiverID != explicit {
		t.Fatalf("authorizations after rejected grant = %+v, want only %q", auths, explicit)
	}
	if !reflect.DeepEqual(auths[0].Scopes, a1.Scopes) ||
		!auths[0].StartsAt.Equal(a1.StartsAt) || !auths[0].ExpiresAt.Equal(a1.ExpiresAt) {
		t.Fatalf("original authorization changed: %+v, want %+v", auths[0], a1)
	}
	audit, err := s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit changed after rejected grant")
	}

	// 原接收方可读取的内容不变。
	res, err := s.Read(ReceiverActor(explicit), f.pid, f.eid, Diagnosis)
	if err != nil {
		t.Fatalf("read as original receiver: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != f.rid {
		t.Fatalf("read records = %+v, want record %q", res.Records, f.rid)
	}

	// 关闭重开后仍只有那一条授权归属于该接收方，标识逐字相同。
	s = reopenStore(t, s, dir, clk)
	t.Cleanup(func() { _ = s.Close() })
	auths, err = s.ListAuthorizations(doc, f.pid, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || auths[0].ID != a1.ID || auths[0].ReceiverID != explicit {
		t.Fatalf("authorizations after reopen = %+v, want only %q", auths, explicit)
	}
	all, err := s.ListAuthorizations(doc, f.pid, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("extra authorizations appeared after reopen: %+v", all)
	}
}
