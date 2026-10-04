package clinical

import (
	"errors"
	"testing"
	"time"
)

// readAfterWait 模拟 Read 因同一本地存储上的其他操作而等待：调用方先持有
// Store 锁，再发起 Read（Read 因而阻塞在锁上），随后把时钟推进到 checkTime
// 才放行。锁的 happens-before 关系保证 Read 拿到锁后读到的必然是推进后的
// 时刻——权限核对只能发生在 checkTime。
func readAfterWait(t *testing.T, s *Store, clk *fakeClock, checkTime time.Time, read func() (ReadResult, error)) (ReadResult, error) {
	t.Helper()
	s.mu.Lock()
	type outcome struct {
		res ReadResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := read()
		done <- outcome{res, err}
	}()
	// 等待 Read 真正阻塞在 Store 锁上。短暂等待仅用于让 goroutine 抵达
	// 阻塞点；即使调度延迟，推进时钟仍在持锁期间完成，Read 获锁后读到的
	// 时刻由互斥锁保证为 checkTime，断言本身不依赖该等待时长。
	time.Sleep(50 * time.Millisecond)
	clk.t = checkTime
	s.mu.Unlock()
	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(5 * time.Second):
		t.Fatal("read did not return after the lock was released")
		return ReadResult{}, nil
	}
}

// 等待期间授权到期：请求发出时仍在有效期内，核对权限时恰好到截止时刻，
// 必须拒绝，且结果不携带任何记录标识、版本、数量或内容。
func TestReadWaitAuthorizationExpires(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	rec, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断v1")
	if _, err := s.ActivateRecord(doc, rec.ID); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}}, start, end); err != nil {
		t.Fatal(err)
	}

	// 请求发出时（09:59）授权有效；实际核对权限时已是 10:00 截止时刻。
	clk.t = end.Add(-time.Minute)
	res, err := readAfterWait(t, s, clk, end, func() (ReadResult, error) {
		return s.Read(rcv, pid, eid, Diagnosis)
	})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
	if res.EncounterID != "" || res.Category != "" || len(res.Records) != 0 {
		t.Fatalf("denied read must carry no ids/count/content, got %+v", res)
	}
}

// 等待期间授权进入有效期：请求发出时授权尚未开始，核对权限时恰好到开始
// 时刻，应允许读取其覆盖的内容（不能沿用请求发出时的时间拒绝）。
func TestReadWaitAuthorizationBecomesActive(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	rec, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断v1")
	if _, err := s.ActivateRecord(doc, rec.ID); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}}, start, end); err != nil {
		t.Fatal(err)
	}

	// 请求发出时（09:59）授权尚未开始；核对时刻正好 10:00，授权生效。
	clk.t = start.Add(-time.Minute)
	res, err := readAfterWait(t, s, clk, start, func() (ReadResult, error) {
		return s.Read(rcv, pid, eid, Diagnosis)
	})
	if err != nil {
		t.Fatalf("authorization active at check time must allow read: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].RecordID != rec.ID {
		t.Fatalf("unexpected records: %+v", res.Records)
	}
}

// 等待期间整类授权到期、另一条限定授权仍有效：只返回后者明确选中的记录，
// 已到期的整类授权不能带出同次就诊的其他记录，也不能因其一到期而拒绝全部。
func TestReadWaitOnlyStillActiveGrantsMerge(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	d2, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}

	wholeEnd := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	// 整类授权 10:00 到期；限定授权覆盖 d1 到 12:00。
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}},
		wholeEnd.Add(-2*time.Hour), wholeEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GrantSelective(doc, pid, rcv.ID, nil,
		[]RecordSelection{sel(eid, Diagnosis, d1.ID)},
		wholeEnd.Add(-2*time.Hour), wholeEnd.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 请求发出时整类授权仍有效；核对时刻整类已到期，只剩 d1 的限定授权。
	clk.t = wholeEnd.Add(-time.Minute)
	res, err := readAfterWait(t, s, clk, wholeEnd, func() (ReadResult, error) {
		return s.Read(rcv, pid, eid, Diagnosis)
	})
	if err != nil {
		t.Fatalf("selective grant still active must allow partial read: %v", err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("records = %d, want exactly 1", len(res.Records))
	}
	if res.Records[0].RecordID != d1.ID {
		t.Fatalf("got record %q, want only selectively granted %q", res.Records[0].RecordID, d1.ID)
	}
}

// 核对时刻仍有效的整类授权与限定授权重叠时，记录只出现一次并保持稳定顺序。
func TestReadWaitActiveGrantsOverlapDedup(t *testing.T) {
	s, clk := newTestStore(t)
	pid, eid := setupPatientEncounter(t, s)
	d1, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断1")
	d2, _ := s.CreateDraft(doc, pid, eid, Diagnosis, "诊断2")
	if _, err := s.ActivateRecord(doc, d1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, d2.ID); err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// 两条授权在核对时刻均有效：整类覆盖全部，限定仅明确选中 d1。
	if _, err := s.Grant(doc, pid, rcv.ID,
		[]Scope{{EncounterID: eid, Category: Diagnosis}}, base, base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GrantSelective(doc, pid, rcv.ID, nil,
		[]RecordSelection{sel(eid, Diagnosis, d1.ID)},
		base, base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}

	clk.t = base
	check := base.Add(time.Minute)
	res, err := readAfterWait(t, s, clk, check, func() (ReadResult, error) {
		return s.Read(rcv, pid, eid, Diagnosis)
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 授权创建顺序与范围不影响结果：每条记录只出现一次，共两条。
	got := map[ID]bool{}
	var prev ID
	for _, r := range res.Records {
		if got[r.RecordID] {
			t.Fatalf("record %q appears more than once: %+v", r.RecordID, res.Records)
		}
		got[r.RecordID] = true
		if prev != "" && r.RecordID < prev {
			t.Fatalf("records not in stable id order: %+v", res.Records)
		}
		prev = r.RecordID
	}
	if len(got) != 2 || !got[d1.ID] || !got[d2.ID] {
		t.Fatalf("want exactly {d1,d2} once, got %+v", res.Records)
	}
}
