package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖建立授权时接收方标识的 UTF-8 校验与保存一致性：接收方标识夹带
// 任何无效 UTF-8 字节（孤立续字节、截断的多字节字符等）时，Grant 与
// GrantSelective（整类、限定记录、二者混合）必须整体返回 ErrInvalidArgument，
// 返回空授权，不保存授权，也不新增授权创建审计——不能靠静默替换（U+FFFD）、
// 截断或跳过坏字节继续授权，否则落盘 JSON 时坏字节被替换成 U+FFFD，重开后
// 授权会被归到使用替换后标识的另一接收方。合法 UTF-8 标识（含中文、其他多
// 字节字符、用户明确输入的合法 U+FFFD、首尾有空白但整体非纯空白）必须原样
// 保存与精确匹配，不裁剪、不改写、不合并看起来相似的标识。

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
	{"中文之间夹坏字节", "接收方\xfe-1"},
	{"孤立续字节", "rcv-\x80"},
	{"结尾截断多字节字符", "接收方-\xe6"},
	{"中间不完整多字节序列", "ab\xe6\x9dcd"},
	{"中文之后截断三字节序列", "接收方\xe7\xa1"},
	{"单个非法字节", "\x80"},
	{"overlong 编码", "\xc0\x80"},
	{"UTF-16 代理项编码", "接收方\n\xed\xa0\x80\n"},
	{"合法替换字符后紧跟非法字节", "接收方�\xff"},
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
	{"用户明确输入的合法替换字符", "接收方-�"},
	{"首尾空白但整体非纯空白", "  rcv-保留空白  "},
}

// grantUTF8Fixture 是接收方标识 UTF-8 测试的夹具：一名患者、一次就诊与一条
// 已生效诊断记录，可用于整类、限定记录与混合授权。
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

// TestGrantRejectsInvalidUTF8ReceiverID 覆盖各类坏字节接收方标识：整类授权
// 与限定记录授权（含混合）一律 ErrInvalidArgument，返回的授权为空（无标识、
// 无范围），不保存授权，不新增授权创建审计；关闭重开后授权清单与审计仍是
// 拒绝前的结果，也不会多出归属于替换后标识的授权。
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
			auditBefore := mustAudit(t, s, f.pid)

			// 整类、限定记录、混合三种形式都必须整体拒绝。
			attempts := []struct {
				name string
				grant func() (Authorization, error)
			}{
				{"Grant 整类", func() (Authorization, error) {
					return s.Grant(doc, f.pid, tc.receiverID, scopes, st, en)
				}},
				{"GrantSelective 整类", func() (Authorization, error) {
					return s.GrantSelective(doc, f.pid, tc.receiverID, scopes, nil, st, en)
				}},
				{"GrantSelective 限定记录", func() (Authorization, error) {
					return s.GrantSelective(doc, f.pid, tc.receiverID, nil, selections, st, en)
				}},
				{"GrantSelective 混合", func() (Authorization, error) {
					return s.GrantSelective(doc, f.pid, tc.receiverID, scopes, selections, st, en)
				}},
			}
			for _, att := range attempts {
				a, err := att.grant()
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("%s(%q) err = %v, want ErrInvalidArgument", att.name, tc.receiverID, err)
				}
				if a.ID != "" || len(a.Scopes) != 0 || len(a.Selections) != 0 {
					t.Fatalf("%s rejected grant returned %+v, want empty Authorization", att.name, a)
				}
			}

			// 拒绝不能留下授权或授权创建审计。
			auths, err := s.ListAuthorizations(doc, f.pid, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(auths) != 0 {
				t.Fatalf("rejected grants left authorizations: %+v", auths)
			}
			audit, err := s.AuditEvents(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after rejected grants")
			}

			// 坏字节没有落盘：关闭重开后授权清单与审计仍是拒绝前的结果。
			s = reopenStore(t, s, dir, clk)
			auths, err = s.ListAuthorizations(doc, f.pid, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(auths) != 0 {
				t.Fatalf("rejected grants persisted after reopen: %+v", auths)
			}
			audit, err = s.AuditEvents(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("audit changed after reopen")
			}

			// 被拒绝的坏字节标识不会被替换成合法标识去建立授权：随后用对应的
			// 合法标识可以正常授权。
			if _, err := s.Grant(doc, f.pid, "rcv-clean-after-reject", scopes, st, en); err != nil {
				t.Fatalf("grant after rejection: %v", err)
			}
			_ = s.Close()
		})
	}
}

// TestGrantValidUTF8ReceiverIDPreservedRoundTrip 覆盖合法接收方标识：原样保存
// 与精确匹配，不裁剪首尾空白、不改写字符形态；创建返回值、单条查询、按接收
// 方筛选的清单与关闭重开后的清单中标识逐字相同，接收方按原标识读取命中。
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
				t.Fatalf("created authorization receiver = %q, want %q", a.ReceiverID, tc.receiverID)
			}

			got, err := s.GetAuthorization(doc, f.pid, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ReceiverID != tc.receiverID {
				t.Fatalf("GetAuthorization receiver = %q, want %q", got.ReceiverID, tc.receiverID)
			}

			// 按接收方筛选只匹配完全相同的标识。
			filtered, err := s.ListAuthorizations(doc, f.pid, tc.receiverID)
			if err != nil {
				t.Fatal(err)
			}
			if len(filtered) != 1 || filtered[0].ID != a.ID || filtered[0].ReceiverID != tc.receiverID {
				t.Fatalf("ListAuthorizations(%q) = %+v", tc.receiverID, filtered)
			}

			// 接收方按原标识读取命中授权内容。
			res, err := s.Read(ReceiverActor(tc.receiverID), f.pid, f.eid, Diagnosis)
			if err != nil {
				t.Fatalf("receiver read: %v", err)
			}
			if len(res.Records) != 1 || res.Records[0].RecordID != f.rid {
				t.Fatalf("receiver read records = %+v, want record %q", res.Records, f.rid)
			}

			// 关闭重开后标识逐字相同，筛选与读取仍命中。
			s = reopenStore(t, s, dir, clk)
			defer func() { _ = s.Close() }()
			filtered, err = s.ListAuthorizations(doc, f.pid, tc.receiverID)
			if err != nil {
				t.Fatal(err)
			}
			if len(filtered) != 1 || filtered[0].ReceiverID != tc.receiverID {
				t.Fatalf("ListAuthorizations after reopen = %+v, want receiver %q", filtered, tc.receiverID)
			}
			if _, err := s.Read(ReceiverActor(tc.receiverID), f.pid, f.eid, Diagnosis); err != nil {
				t.Fatalf("receiver read after reopen: %v", err)
			}
		})
	}
}

// TestGrantInvalidUTF8DoesNotCollideWithReplacementCharReceiver 覆盖撞名场景：
// 已有授权属于标识中含合法 U+FFFD 的接收方；再用一个只有该位置换成坏字节的
// 标识申请相同范围必须失败——原授权的标识、范围、期限与可读取内容不变，
// 关闭重开后也不会多出归属于该接收方的授权。
func TestGrantInvalidUTF8DoesNotCollideWithReplacementCharReceiver(t *testing.T) {
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

	// 用户明确输入的合法 U+FFFD 标识可以正常授权。
	explicit := "接收方-�"
	a1, err := s.Grant(doc, f.pid, explicit, scopes, st, en)
	if err != nil {
		t.Fatalf("grant with explicit U+FFFD: %v", err)
	}
	auditBefore := mustAudit(t, s, f.pid)

	// 只有该位置换成坏字节的标识（替换后会变成 explicit）必须被拒绝，
	// 而不是命中或另建：返回 ErrInvalidArgument，授权清单与审计不变。
	bad := "接收方-\xff"
	if _, err := s.Grant(doc, f.pid, bad, scopes, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("grant with bad-byte receiver err = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.GrantSelective(doc, f.pid, bad, nil,
		[]RecordSelection{{RecordID: f.rid, EncounterID: f.eid, Category: Diagnosis}}, st, en); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("selective grant with bad-byte receiver err = %v, want ErrInvalidArgument", err)
	}

	// 原授权的标识、范围与期限不变。
	got, err := s.GetAuthorization(doc, f.pid, a1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceiverID != explicit || !reflect.DeepEqual(got.Scopes, a1.Scopes) ||
		!got.StartsAt.Equal(a1.StartsAt) || !got.ExpiresAt.Equal(a1.ExpiresAt) {
		t.Fatalf("original authorization changed: %+v, want %+v", got, a1)
	}
	auths, err := s.ListAuthorizations(doc, f.pid, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || auths[0].ID != a1.ID {
		t.Fatalf("authorizations for %q = %+v, want only the original grant", explicit, auths)
	}
	audit, err := s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit changed after rejected grants")
	}

	// 原接收方可读取的内容不变。
	res, err := s.Read(ReceiverActor(explicit), f.pid, f.eid, Diagnosis)
	if err != nil {
		t.Fatalf("receiver read: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != f.rid {
		t.Fatalf("receiver read records = %+v, want record %q", res.Records, f.rid)
	}

	// 关闭重开后仍只有那一份明确含合法 U+FFFD 的授权，不会多出归属于
	// 该接收方的授权。
	s = reopenStore(t, s, dir, clk)
	defer func() { _ = s.Close() }()
	auths, err = s.ListAuthorizations(doc, f.pid, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || auths[0].ID != a1.ID || auths[0].ReceiverID != explicit {
		t.Fatalf("authorizations after reopen = %+v, want only %q for %q", auths, a1.ID, explicit)
	}
	all, err := s.ListAuthorizations(doc, f.pid, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("total authorizations after reopen = %+v, want exactly one", all)
	}
}
