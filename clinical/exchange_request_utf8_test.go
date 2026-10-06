package clinical

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖创建交换时请求号（RequestID）的 UTF-8 一致性：
//
// 快照以 JSON 落盘，encoding/json 会把无效 UTF-8 字节静默替换成 U+FFFD。
// 若放行夹带坏字节的请求号，当次幂等检索按原字节工作，关闭重开后请求号却
// 被改写成替换字符：原请求号重试会找不到原交换而另建一份；明确包含合法
// U+FFFD 的请求号还可能与被改写的请求号撞号。因此任何位置夹带无效字节或
// 不完整多字节字符的非空请求号都必须整体返回 ErrInvalidArgument——不返回
// 交换标识或包内容、不新增交换或交换创建审计、绝不替换/截断/跳过坏字节、
// 也不能用替换后的字符串去命中已保存交换；合法请求号（含中文、表情、换行、
// 首尾空白与用户明确输入的合法 U+FFFD）原样保存、原样比较，关闭重开与
// 幂等重试都稳定指向同一份交换。

// invalidRequestIDs 是各类无效 UTF-8 请求号：坏序列出现在开头、结尾、中间，
// 夹在合法中文之间，孤立续字节，不完整多字节字符，overlong 编码与 UTF-16
// 代理项编码；另有一例在合法 U+FFFD 之后紧跟非法字节——必须因那个坏字节被
// 拒绝，而不是把整串当成合法替换字符去命中已保存交换。
var invalidRequestIDs = []struct {
	label string
	req   string
}{
	{"开头非法字节", "\xffreq-001"},
	{"结尾非法字节", "req-001\xff"},
	{"中间非法字节", "re\xffq-001"},
	{"合法中文之间夹坏字节", "请求\xff交换"},
	{"中文之后夹坏字节", "请求-\xff"},
	{"孤立续字节", "req-001\x80"},
	{"结尾不完整多字节字符", "req-001\xe6"},
	{"中间不完整多字节序列", "ab\xe6\x9dcd"},
	{"中文之后截断三字节序列", "请求交换\xe7\xa1"},
	{"单个非法字节", "\x80"},
	{"overlong 编码", "req-\xc0\x80"},
	{"UTF-16 代理项编码", "请求-\xed\xa0\x80"},
	{"坏字节后还有更多内容", "req\xff-tail"},
	{"合法替换字符后紧跟非法字节", "req-�\xff"},
}

// validRequestIDs 是必须逐字原样保存与比较的合法请求号。
var validRequestIDs = []struct {
	label string
	req   string
}{
	{"中文与全角标点", "请求号-001（复核）"},
	{"表情字符", "交换请求 🩸😷"},
	{"换行", "请求号\n第二行"},
	{"首尾空白原样保留", "  req-001  \t"},
	{"全角空白作前缀", "　请求号-002"},
	{"用户明确输入的合法替换字符", "请求-�"},
	{"合法替换字符夹在中文中间", "请求�交换"},
}

// exchangeUTF8Fixture 是一个可以反复关闭/重开的最小交换夹具：
// 一名患者、一次就诊、一条已生效诊断、一条覆盖它的当前有效授权。
type exchangeUTF8Fixture struct {
	s      *Store
	clk    *fakeClock
	dir    string
	pid    ID
	eid    ID
	rec    ID
	authID ID
}

func setupExchangeUTF8(t *testing.T) exchangeUTF8Fixture {
	t.Helper()
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)}
	s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p, err := s.RegisterPatient(doc, "请求号UTF8患者")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断内容：高血压 I10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, r.ID); err != nil {
		t.Fatal(err)
	}
	a, err := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return exchangeUTF8Fixture{s: s, clk: clk, dir: dir, pid: p.ID, eid: e.ID, rec: r.ID, authID: a.ID}
}

func (f *exchangeUTF8Fixture) create(req string) (Exchange, error) {
	return f.s.CreateExchange(doc, f.pid, rcv.ID, f.authID, []ID{f.rec}, req)
}

func (f *exchangeUTF8Fixture) reopen(t *testing.T) {
	t.Helper()
	f.s = reopenStore(t, f.s, f.dir, f.clk)
}

// TestCreateExchangeRejectsInvalidUTF8RequestID 覆盖坏字节请求号：
// 一律 ErrInvalidArgument、返回空交换、不留交换与审计；空串与全空白继续
// 拒绝；拒绝不改变已有交换的包、摘要、状态和回执；关闭重开后结果不变。
func TestCreateExchangeRejectsInvalidUTF8RequestID(t *testing.T) {
	f := setupExchangeUTF8(t)

	// 先创建一份合法交换（请求号明确包含 U+FFFD）并登记接受回执：
	// 后续所有被拒绝的提交都不能改动它的包、摘要、状态与回执。
	anchorReq := "请求-�"
	anchor, err := f.create(anchorReq)
	if err != nil {
		t.Fatalf("create anchor exchange: %v", err)
	}
	conf, err := f.s.SubmitReceipt(rcv, anchor.ID, anchor.Digest, ReceiptAccepted, "")
	if err != nil {
		t.Fatalf("submit receipt: %v", err)
	}
	if conf.Status != ExchangeAccepted {
		t.Fatalf("anchor status = %q, want %q", conf.Status, ExchangeAccepted)
	}

	auditBefore := mustAudit(t, f.s, f.pid)
	xsBefore, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range invalidRequestIDs {
		t.Run(tc.label, func(t *testing.T) {
			x, err := f.create(tc.req)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("CreateExchange(%q) err = %v, want ErrInvalidArgument", tc.req, err)
			}
			// 拒绝时不返回交换标识或包内容。
			if x.ID != "" || x.Digest != "" || len(x.Package.Records) != 0 || !x.CreatedAt.IsZero() {
				t.Fatalf("rejected create returned an exchange: %+v", x)
			}
			// 不新增交换。
			xs, err := f.s.ListExchanges(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if len(xs) != len(xsBefore) {
				t.Fatalf("rejected create changed exchange count: before=%d after=%d", len(xsBefore), len(xs))
			}
			// 不新增审计。
			audit, err := f.s.AuditEvents(doc, f.pid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(audit, auditBefore) {
				t.Fatalf("rejected create changed audit events:\nbefore=%+v\nafter= %+v", auditBefore, audit)
			}
			// 已有交换的包、摘要、状态与回执保持原样。
			got, err := f.s.GetExchange(doc, f.pid, anchor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Digest != anchor.Digest || got.Status != ExchangeAccepted ||
				got.Receipt == nil || got.Receipt.Outcome != ReceiptAccepted ||
				!reflect.DeepEqual(got.Package, anchor.Package) || got.RequestID != anchorReq {
				t.Fatalf("anchor exchange changed after rejected create:\nanchor=%+v\ngot=   %+v", anchor, got)
			}
		})
	}

	// 空串与全空白字符串继续拒绝。
	for _, blank := range []string{"", " ", "\n\t\r ", "　"} {
		if _, err := f.create(blank); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("blank request id %q err = %v, want ErrInvalidArgument", blank, err)
		}
	}
	if xs, _ := f.s.ListExchanges(doc, f.pid); len(xs) != len(xsBefore) {
		t.Fatalf("blank creates changed exchange count: %d", len(xs))
	}
	if audit, _ := f.s.AuditEvents(doc, f.pid); !reflect.DeepEqual(audit, auditBefore) {
		t.Fatal("blank creates changed audit events")
	}

	// 接收方身份提交夹带坏字节的请求号：UTF-8 校验与既有身份权限并存——
	// 这里身份先行拒绝，行为仍是 ErrAccessDenied。
	if _, err := f.s.CreateExchange(rcv, f.pid, rcv.ID, f.authID, []ID{f.rec}, "req\xff"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("receiver create err = %v, want ErrAccessDenied", err)
	}

	// 关闭后从同一位置重新打开：交换清单与审计仍是拒绝前的结果，
	// 锚点交换的请求号逐字保持（没有任何坏字节曾被落盘成 U+FFFD）。
	f.reopen(t)
	t.Cleanup(func() { _ = f.s.Close() })
	xsAfter, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xsAfter) != len(xsBefore) {
		t.Fatalf("rejected creates persisted after reopen: before=%d after=%d", len(xsBefore), len(xsAfter))
	}
	auditAfter, err := f.s.AuditEvents(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(auditAfter, auditBefore) {
		t.Fatal("audit events changed after reopen despite all creates being rejected")
	}
	gotAnchor, err := f.s.GetExchange(doc, f.pid, anchor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotAnchor.RequestID != anchorReq || gotAnchor.Status != ExchangeAccepted ||
		gotAnchor.Receipt == nil || gotAnchor.Receipt.Outcome != ReceiptAccepted ||
		gotAnchor.Digest != anchor.Digest || !reflect.DeepEqual(gotAnchor.Package, anchor.Package) {
		t.Fatalf("anchor exchange changed after reopen:\nanchor=%+v\ngot=   %+v", anchor, gotAnchor)
	}
}

// TestInvalidUTF8RequestIDNeverMatchesSavedExchange 保证坏字节不会被替换后
// 命中已保存交换：先以明确包含合法 U+FFFD 的请求号创建交换，再提交字节层面
// 与其“替换后形态”相同的坏请求号——必须 ErrInvalidArgument，而不是返回那份
// 交换；重开后同样如此，且始终只有一份交换。坏请求号也没有占用请求号：
// 之后用一个全新的合法请求号可正常再建一份。
func TestInvalidUTF8RequestIDNeverMatchesSavedExchange(t *testing.T) {
	f := setupExchangeUTF8(t)
	t.Cleanup(func() { _ = f.s.Close() })

	// 合法请求号“请求-�”。
	anchor, err := f.create("请求-�")
	if err != nil {
		t.Fatalf("create anchor: %v", err)
	}

	// 这些输入若被静默替换成 U+FFFD，就会变成锚点的请求号——必须直接拒绝，
	// 绝不能返回锚点交换。
	smugglers := []string{
		"请求-\xff",         // 单个非法字节替换后即 U+FFFD
		"请求-\xed\xa0\x80", // 代理项替换后同样是 U+FFFD
		"请求-�\xff",        // 合法 U+FFFD 后再跟坏字节
		"\xff请求-�",        // 坏字节出现在开头，含合法 U+FFFD
	}
	for _, bad := range smugglers {
		if x, err := f.create(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("CreateExchange(%q) err = %v, want ErrInvalidArgument (x=%+v)", bad, err, x)
		} else if x.ID == anchor.ID {
			t.Fatalf("bad request id %q matched saved exchange %q", bad, anchor.ID)
		}
	}

	// 重开后再试：坏字节依旧被拒，清单仍只有锚点一份。
	f.reopen(t)
	for _, bad := range smugglers {
		if x, err := f.create(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("after reopen CreateExchange(%q) err = %v, want ErrInvalidArgument", bad, err)
		} else if x.ID == anchor.ID {
			t.Fatalf("after reopen bad request id %q matched saved exchange", bad)
		}
	}
	if xs, _ := f.s.ListExchanges(doc, f.pid); len(xs) != 1 || xs[0].ID != anchor.ID {
		t.Fatalf("smuggled creates changed exchange list: %+v", xs)
	}

	// 被拒绝的坏请求号不占用任何请求号：全新合法请求号照常再建一份；
	// 两份合法交换即使一份请求号含 U+FFFD，重开后仍各自独立、不坍缩。
	other, err := f.create("请求-�-后续")
	if err != nil {
		t.Fatalf("create second exchange: %v", err)
	}
	if other.ID == anchor.ID {
		t.Fatal("second exchange must be distinct")
	}
	f.reopen(t)
	xs, err := f.s.ListExchanges(doc, f.pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 2 {
		t.Fatalf("exchanges after reopen = %d, want 2", len(xs))
	}
	byReq := map[string]ID{}
	for _, x := range xs {
		byReq[x.RequestID] = x.ID
	}
	if byReq["请求-�"] != anchor.ID {
		t.Fatalf("anchor request id not preserved: %+v", byReq)
	}
	if byReq["请求-�-后续"] != other.ID {
		t.Fatalf("second request id not preserved: %+v", byReq)
	}
}

// TestCreateExchangeValidUTF8RequestIDPreservedRoundTrip 覆盖合法请求号：
// 原样保存、幂等重试（含记录集合排列变化）、其他参数变化 ErrConflict、
// 不同内部使用者互不占用；关闭重开后请求号逐字一致、重试仍返回同一份
// 已保存结果。首尾空白不被修剪：修剪后的字符串是另一个请求号。
func TestCreateExchangeValidUTF8RequestIDPreservedRoundTrip(t *testing.T) {
	for _, tc := range validRequestIDs {
		t.Run(tc.label, func(t *testing.T) {
			f := setupExchangeUTF8(t)

			x1, err := f.create(tc.req)
			if err != nil {
				t.Fatalf("create exchange: %v", err)
			}
			if x1.RequestID != tc.req {
				t.Fatalf("request id = %q, want %q (no normalization)", x1.RequestID, tc.req)
			}
			auditBefore := len(mustAudit(t, f.s, f.pid))

			// 幂等重试：同一使用者、相同请求号与相同参数，只返回原交换，
			// 不新增审计。
			x2, err := f.create(tc.req)
			if err != nil {
				t.Fatalf("idempotent retry: %v", err)
			}
			if x2.ID != x1.ID || x2.RequestID != tc.req || x2.Digest != x1.Digest ||
				!reflect.DeepEqual(x2.Package, x1.Package) || !x2.CreatedAt.Equal(x1.CreatedAt) {
				t.Fatalf("retry did not return the saved exchange:\n%+v\n%+v", x1, x2)
			}
			if got := len(mustAudit(t, f.s, f.pid)); got != auditBefore {
				t.Fatalf("idempotent retry added audit events: %d -> %d", auditBefore, got)
			}

			// 其他参数变化仍返回 ErrConflict（这里换接收方；幂等检索先于
			// 授权绑定校验，因此无需为 rcvB 另建授权）。
			if _, err := f.s.CreateExchange(doc, f.pid, rcvB.ID, f.authID, []ID{f.rec}, tc.req); !errors.Is(err, ErrConflict) {
				t.Fatalf("different receiver with same request id err = %v, want ErrConflict", err)
			}

			// 不同内部使用者以相同请求号提交：互不占用，各得一份。
			xOther, err := f.s.CreateExchange(doc2, f.pid, rcv.ID, f.authID, []ID{f.rec}, tc.req)
			if err != nil {
				t.Fatalf("other actor with same request id: %v", err)
			}
			if xOther.ID == x1.ID || xOther.RequestID != tc.req || xOther.CreatorID != doc2.ID {
				t.Fatalf("request id must be scoped per actor: %+v", xOther)
			}

			// 关闭后从同一位置重新打开：请求号逐字保持。
			f.reopen(t)
			t.Cleanup(func() { _ = f.s.Close() })
			got, err := f.s.GetExchange(doc, f.pid, x1.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.RequestID != tc.req {
				t.Fatalf("request id after reopen = %q, want %q", got.RequestID, tc.req)
			}
			// 重开后用原请求号重试，仍返回同一份已保存交换，且不新增审计。
			auditReopen := len(mustAudit(t, f.s, f.pid))
			x3, err := f.create(tc.req)
			if err != nil {
				t.Fatalf("idempotent retry after reopen: %v", err)
			}
			if x3.ID != x1.ID || x3.RequestID != tc.req || x3.Digest != x1.Digest ||
				!reflect.DeepEqual(x3.Package, x1.Package) {
				t.Fatalf("retry after reopen did not return the saved exchange:\n%+v\n%+v", x1, x3)
			}
			if got := len(mustAudit(t, f.s, f.pid)); got != auditReopen {
				t.Fatalf("retry after reopen added audit events: %d -> %d", auditReopen, got)
			}
			// 重开后其他参数变化仍是 ErrConflict。
			if _, err := f.s.CreateExchange(doc, f.pid, rcvB.ID, f.authID, []ID{f.rec}, tc.req); !errors.Is(err, ErrConflict) {
				t.Fatalf("after reopen different receiver err = %v, want ErrConflict", err)
			}
		})
	}
}

// TestCreateExchangeRequestIDWhitespaceNotTrimmed 专门确认首尾空白不被修剪：
// 带空白的请求号与其修剪形态是两个不同请求号，分别各建一份、重开后仍独立。
func TestCreateExchangeRequestIDWhitespaceNotTrimmed(t *testing.T) {
	f := setupExchangeUTF8(t)
	t.Cleanup(func() { _ = f.s.Close() })

	padded := "  req-pad  \t"
	trimmed := "req-pad"

	x1, err := f.create(padded)
	if err != nil {
		t.Fatalf("create padded: %v", err)
	}
	if x1.RequestID != padded {
		t.Fatalf("padded request id normalized to %q", x1.RequestID)
	}
	// 修剪后的字符串从未被使用过：必须另建一份，而不是返回 x1。
	x2, err := f.create(trimmed)
	if err != nil {
		t.Fatalf("create trimmed: %v", err)
	}
	if x2.ID == x1.ID {
		t.Fatal("leading/trailing whitespace must not be trimmed when matching request ids")
	}
	if x2.RequestID != trimmed {
		t.Fatalf("trimmed request id stored as %q", x2.RequestID)
	}

	// 全空白仍拒绝，且与上面两个请求号无关。
	if _, err := f.create("  \t "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("whitespace-only request id err = %v, want ErrInvalidArgument", err)
	}

	f.reopen(t)
	if xs, _ := f.s.ListExchanges(doc, f.pid); len(xs) != 2 {
		t.Fatalf("exchanges after reopen = %d, want 2", len(xs))
	}
	if r1, err := f.create(padded); err != nil || r1.ID != x1.ID {
		t.Fatalf("padded retry after reopen: id=%q err=%v, want %q", r1.ID, err, x1.ID)
	}
	if r2, err := f.create(trimmed); err != nil || r2.ID != x2.ID {
		t.Fatalf("trimmed retry after reopen: id=%q err=%v, want %q", r2.ID, err, x2.ID)
	}
}
