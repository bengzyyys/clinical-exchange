package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRetrySucceedsDespiteSaveFailure 覆盖“交换已成功保存后，本地保存条件\n// 不可用”时的重试行为：同使用者同请求号同参数必须成功返回正式保存的\n// 交换（含已登记回执与冻结包），不落盘、不重新打包；不同参数仍报\n// ErrConflict；另一使用者或新请求号的首次创建仍按既有规则校验并在\n// 保存失败时返回实际错误、不占用请求号。
func TestRetrySucceedsDespiteSaveFailure(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)}
	s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	p, _ := s.RegisterPatient(doc, "患者")
	e, _ := s.AddEncounter(doc, p.ID, time.Time{})
	rd, _ := s.CreateDraft(doc, p.ID, e.ID, Diagnosis, "诊断v1")
	if _, err := s.ActivateRecord(doc, rd.ID); err != nil {
		t.Fatal(err)
	}
	ro, _ := s.CreateDraft(doc, p.ID, e.ID, Order, "医嘱v1")
	if _, err := s.ActivateRecord(doc, ro.ID); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}, {EncounterID: e.ID, Category: Order}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour))
	// 另一条有效授权，供“另一使用者首次创建恰逢保存失败”场景使用。
	a2, _ := s.Grant(doc, p.ID, rcv.ID,
		[]Scope{{EncounterID: e.ID, Category: Diagnosis}},
		clk.t.Add(-time.Hour), clk.t.Add(48*time.Hour))

	x, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rd.ID, ro.ID}, "req-x")
	if err != nil {
		t.Fatal(err)
	}
	// 接收方登记拒绝回执。
	if _, err := s.SubmitReceipt(rcv, x.ID, x.Digest, ReceiptRejected, "原因"); err != nil {
		t.Fatal(err)
	}
	withReceipt, err := s.GetExchange(doc, p.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 之后记录被更正、授权被撤回：重试不得重新打包。
	if _, err := s.CorrectRecord(doc, rd.ID, 1, "诊断v2", "更正"); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(doc, p.ID, a.ID); err != nil {
		t.Fatal(err)
	}

	// 制造本地保存失败。
	dataPath := filepath.Join(dir, dataName)
	backup := dataPath + ".saved"
	if err := os.Rename(dataPath, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() {
		os.Remove(dataPath)
		os.Rename(backup, dataPath)
	}()

	// 同参数重试（乱序+重复）：成功返回原交换，含已登记回执。
	got, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{ro.ID, rd.ID, ro.ID}, "req-x")
	if err != nil {
		t.Fatalf("retry with same params during save failure: %v", err)
	}
	assertExchangeMatches(t, got, withReceipt, "retry during save failure")
	if got.Status != ExchangeRejected || got.Receipt == nil || got.Receipt.Reason != "原因" {
		t.Fatalf("receipt lost on retry: %+v", got)
	}
	// 包内容仍是创建时固化的 v1，不是更正后的 v2。
	for _, r := range got.Package.Records {
		if r.RecordID == rd.ID && r.Content != "诊断v1" {
			t.Fatalf("retry repackaged current version: %+v", r)
		}
	}
	// 返回副本独立：篡改不影响库内。
	tamperExchangeResult(&got)
	g2, _ := s.GetExchange(doc, p.ID, x.ID)
	assertExchangeMatches(t, g2, withReceipt, "store after tampering retry result")

	// 不同参数：ErrConflict，而不是保存错误。
	if _, err := s.CreateExchange(doc, p.ID, rcv.ID, a.ID, []ID{rd.ID}, "req-x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different params err = %v, want ErrConflict", err)
	}

	// 另一内部使用者沿用相同请求号：视为首次创建，保存失败返回实际错误。
	if _, err := s.CreateExchange(doc2, p.ID, rcv.ID, a2.ID, []ID{rd.ID}, "req-x"); err == nil {
		t.Fatal("other actor create during save failure must fail")
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrAccessDenied) {
		t.Fatalf("save failure surfaced as business error: %v", err)
	}

	// 无对应成功交换的新请求号：保存失败返回实际错误，不占用请求号。
	if _, err := s.CreateExchange(doc, p.ID, rcv.ID, a2.ID, []ID{rd.ID}, "req-new"); err == nil {
		t.Fatal("first create during save failure must fail")
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrAccessDenied) {
		t.Fatalf("save failure surfaced as business error: %v", err)
	}

	// 恢复保存条件：新请求号未被失败尝试占用，可成功创建。
	if err := os.Remove(dataPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, dataPath); err != nil {
		t.Fatal(err)
	}
	xNew, err := s.CreateExchange(doc, p.ID, rcv.ID, a2.ID, []ID{rd.ID}, "req-new")
	if err != nil {
		t.Fatalf("first create after recovery: %v", err)
	}
	if xNew.ID == "" || xNew.ID == x.ID {
		t.Fatalf("expected a fresh exchange, got %+v", xNew)
	}
}
