package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestEncounterSaveFailureKeepsEncountersAndAllowsRetry 覆盖“内部使用者为未停用
// 的合成患者登记就诊，患者标识与发生时间都合法，但本地保存失败”的场景：登记必须
// 明确返回保存错误，不能表现为成功，也不能误报为患者不存在、档案停用或参数非法。
// 失败后就诊列表与完整档案中的就诊集合与提交前一致——没有新增、替换或丢失就诊，
// 已有就诊的标识、所属患者、发生时间与创建时间保持原值，其下已保存的记录内容与
// 历史仍可查看；登记就诊本就不产生审计事件，失败尝试同样不能追加事件或改动原有
// 审计；另一名患者的就诊不受影响。调用过程中返回的临时结果不能当作正式就诊。
// 保存条件恢复后重新提交合法登记正常成功：就诊列表与完整档案只多出这一条，发生
// 时间与创建时间取自成功的这次操作（明确给出的发生时间保留其时刻并转为 UTC，
// 零值则取成功登记时的当前时间，不沿用失败尝试的时间），且关闭后从同一数据位置
// 重新打开仍只看到最后一次成功保存的就诊集合。
func TestEncounterSaveFailureKeepsEncountersAndAllowsRetry(t *testing.T) {
	t.Run("明确发生时间", func(t *testing.T) {
		// 调用者明确给出发生时间，且带非 UTC 偏移：正式结果必须保留它表示的
		// 时刻，并按现有规则转换为 UTC。
		zone := time.FixedZone("UTC+08:00", 8*3600)
		testEncounterSaveFailure(t, time.Date(2026, 6, 20, 15, 30, 0, 0, zone))
	})
	t.Run("零值发生时间", func(t *testing.T) {
		// 发生时间为零值：取成功登记时的当前时间。
		testEncounterSaveFailure(t, time.Time{})
	})
}

func testEncounterSaveFailure(t *testing.T, occurredArg time.Time) {
	t.Helper()
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()
	p, err := s.RegisterPatient(doc, "登记保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	// 患者原先已有多次就诊。
	e1, err := s.AddEncounter(doc, p.ID, clk.t.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.AddEncounter(doc, p.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// 其中一次就诊下已经保存了诊断与医嘱（生效记录）。
	diagDraft, err := s.CreateDraft(doc, p.ID, e1.ID, Diagnosis, "既有诊断内容")
	if err != nil {
		t.Fatal(err)
	}
	diagV1, err := s.ActivateRecord(doc, diagDraft.ID)
	if err != nil {
		t.Fatal(err)
	}
	orderDraft, err := s.CreateDraft(doc, p.ID, e1.ID, Order, "既有医嘱内容")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRecord(doc, orderDraft.ID); err != nil {
		t.Fatal(err)
	}
	// 另一名患者及其就诊，用于验证不受本次失败影响。
	other, err := s.RegisterPatient(doc, "不受影响患者")
	if err != nil {
		t.Fatal(err)
	}
	otherEnc, err := s.AddEncounter(doc, other.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	// 提交前的基线：就诊列表、完整档案、就诊下记录历史、审计与另一名患者。
	listBefore, err := s.ListEncounters(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listBefore) != 2 || listBefore[0].ID != e1.ID || listBefore[1].ID != e2.ID {
		t.Fatalf("unexpected baseline encounters: %+v", listBefore)
	}
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	recordsBefore, err := s.EncounterRecords(doc, p.ID, e1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recordsBefore) != 2 {
		t.Fatalf("unexpected baseline encounter records: %+v", recordsBefore)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := auditActions(auditBefore)[ActionActivated]; got != 2 {
		t.Fatalf("unexpected baseline audit: %v", auditActions(auditBefore))
	}
	otherListBefore, err := s.ListEncounters(doc, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherChartBefore, err := s.Chart(doc, other.ID)
	if err != nil {
		t.Fatal(err)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），就诊集合与提交前一致——
	// 没有新增、替换或丢失就诊；已有就诊的标识、所属患者、发生时间与创建时间
	// 保持原值；其下记录的完整历史仍可查看；审计与另一名患者全部维持提交前
	// 状态。失败调用若返回了临时结果，它也不能出现在任何正式查询结果中。
	checkIntact := func(label string, st *Store, failedEnc Encounter) {
		t.Helper()

		// 已有档案能正常查询，并不意味着失败的登记已经保存。
		if _, err := st.GetPatient(doc, p.ID); err != nil {
			t.Fatalf("%s: get patient: %v", label, err)
		}

		list, err := st.ListEncounters(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list encounters: %v", label, err)
		}
		if !reflect.DeepEqual(list, listBefore) {
			t.Fatalf("%s: encounter list changed:\nbefore: %+v\nafter:  %+v", label, listBefore, list)
		}

		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		if !reflect.DeepEqual(chart.Encounters, chartBefore.Encounters) {
			t.Fatalf("%s: chart encounters changed:\nbefore: %+v\nafter:  %+v", label, chartBefore.Encounters, chart.Encounters)
		}
		if !reflect.DeepEqual(chart.Records, chartBefore.Records) {
			t.Fatalf("%s: chart records changed:\nbefore: %+v\nafter:  %+v", label, chartBefore.Records, chart.Records)
		}
		if !reflect.DeepEqual(chart.AuditEvents, chartBefore.AuditEvents) {
			t.Fatalf("%s: chart audit changed:\nbefore: %+v\nafter:  %+v", label, chartBefore.AuditEvents, chart.AuditEvents)
		}

		// 失败调用产生的临时结果不能当作正式就诊：它的标识不出现在
		// 就诊列表与完整档案中。
		if failedEnc.ID != "" {
			for _, e := range list {
				if e.ID == failedEnc.ID {
					t.Fatalf("%s: failed registration surfaced in encounter list: %+v", label, e)
				}
			}
			for _, e := range chart.Encounters {
				if e.ID == failedEnc.ID {
					t.Fatalf("%s: failed registration surfaced in chart: %+v", label, e)
				}
			}
		}

		// 已有就诊下的记录内容与历史仍可正常查看。
		records, err := st.EncounterRecords(doc, p.ID, e1.ID)
		if err != nil {
			t.Fatalf("%s: encounter records: %v", label, err)
		}
		if !reflect.DeepEqual(records, recordsBefore) {
			t.Fatalf("%s: encounter records changed:\nbefore: %+v\nafter:  %+v", label, recordsBefore, records)
		}

		// 登记就诊原本不产生审计事件；失败尝试同样不能追加事件或改动原有审计。
		audit, err := st.AuditEvents(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: audit: %v", label, err)
		}
		if !reflect.DeepEqual(audit, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v", label, auditBefore, audit)
		}

		// 另一名患者的就诊与档案不受这次失败影响。
		otherList, err := st.ListEncounters(doc, other.ID)
		if err != nil {
			t.Fatalf("%s: other list encounters: %v", label, err)
		}
		if !reflect.DeepEqual(otherList, otherListBefore) {
			t.Fatalf("%s: other patient encounters changed:\nbefore: %+v\nafter:  %+v", label, otherListBefore, otherList)
		}
		otherChart, err := st.Chart(doc, other.ID)
		if err != nil {
			t.Fatalf("%s: other chart: %v", label, err)
		}
		if !reflect.DeepEqual(otherChart, otherChartBefore) {
			t.Fatalf("%s: other patient chart changed:\nbefore: %+v\nafter:  %+v", label, otherChartBefore, otherChart)
		}
	}

	// 制造本地保存失败：数据文件原位置被同名目录占据，原子改名必然失败，
	// 与身份、患者状态或参数合法性无关。原数据文件先挪到旁边，事后原样还原。
	dataPath := filepath.Join(dir, dataName)
	backupPath := dataPath + ".saved"
	if err := os.Rename(dataPath, backupPath); err != nil {
		t.Fatalf("backup data file: %v", err)
	}
	if err := os.Mkdir(dataPath, 0o755); err != nil {
		t.Fatalf("block data path: %v", err)
	}

	// 内部使用者、患者未停用、患者标识与发生时间都合法，仅保存失败：必须明确
	// 报错，且不能伪装成患者不存在、档案停用、参数非法或权限拒绝等业务结果。
	failTime := clk.t.Add(time.Hour)
	clk.t = failTime
	failedEnc, err := s.AddEncounter(doc, p.ID, occurredArg)
	if err == nil {
		t.Fatal("add encounter must fail when local save fails")
	}
	if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, ErrDeactivated) || errors.Is(err, ErrAccessDenied) ||
		errors.Is(err, ErrMismatchedPatient) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrActive) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}

	// 失败后：就诊集合、已有记录、审计与另一名患者全部维持提交前状态。
	checkIntact("after failed save", s, failedEnc)

	// 关闭后从原数据位置重新打开：先恢复保存条件（还原原数据文件），
	// 失败尝试不能在重新打开后出现。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dataPath); err != nil {
		t.Fatalf("unblock data path: %v", err)
	}
	if err := os.Rename(backupPath, dataPath); err != nil {
		t.Fatalf("restore data file: %v", err)
	}
	s2 := open()
	checkIntact("after reopen", s2, failedEnc)

	// 保存条件恢复后：对同一患者重新提交同样的合法登记，应当正常成功。
	successTime := failTime.Add(time.Hour)
	clk.t = successTime
	enc, err := s2.AddEncounter(doc, p.ID, occurredArg)
	if err != nil {
		t.Fatalf("add encounter after save recovered: %v", err)
	}
	if enc.ID == "" || enc.ID == failedEnc.ID {
		t.Fatalf("retry must produce a new formal encounter, got %+v (failed attempt: %+v)", enc, failedEnc)
	}
	if enc.PatientID != p.ID {
		t.Fatalf("patient attribution changed: %+v, want patient %q", enc, p.ID)
	}
	if occurredArg.IsZero() {
		// 发生时间为零值：取成功登记时的当前时间，不能沿用失败尝试时的时间。
		if !enc.OccurredAt.Equal(successTime) || enc.OccurredAt.Equal(failTime) {
			t.Fatalf("occurred time %v does not belong to the successful attempt (failed at %v)", enc.OccurredAt, failTime)
		}
	} else {
		// 发生时间由调用者明确给出：保留它表示的时刻，并按现有规则转为 UTC。
		if !enc.OccurredAt.Equal(occurredArg) {
			t.Fatalf("occurred moment %v does not preserve submitted moment %v", enc.OccurredAt, occurredArg)
		}
		if enc.OccurredAt.Location() != time.UTC {
			t.Fatalf("occurred time not converted to UTC: %+v", enc.OccurredAt)
		}
	}
	// 创建时间对应成功的这次操作，不能沿用失败尝试的时间。
	if !enc.CreatedAt.Equal(successTime) || enc.CreatedAt.Equal(failTime) {
		t.Fatalf("created time %v does not belong to the successful attempt (failed at %v)", enc.CreatedAt, failTime)
	}

	// 就诊列表与完整档案只增加这次成功登记的那一条，并保留全部原有就诊；
	// 成功返回的就诊标识在两个查询结果中对应到同一条记录。
	assertRetryVisible := func(label string, st *Store) ([]Encounter, PatientChart) {
		t.Helper()
		list, err := st.ListEncounters(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list encounters: %v", label, err)
		}
		if len(list) != len(listBefore)+1 {
			t.Fatalf("%s: encounter count = %d, want %d: %+v", label, len(list), len(listBefore)+1, list)
		}
		byID := map[ID]Encounter{}
		for _, e := range list {
			byID[e.ID] = e
		}
		for _, want := range listBefore {
			got, ok := byID[want.ID]
			if !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: existing encounter lost or changed: want %+v, got %+v (present=%v)", label, want, got, ok)
			}
		}
		got, ok := byID[enc.ID]
		if !ok || !reflect.DeepEqual(got, enc) {
			t.Fatalf("%s: successful encounter missing or different in list: want %+v, got %+v (present=%v)", label, enc, got, ok)
		}

		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		if len(chart.Encounters) != len(chartBefore.Encounters)+1 {
			t.Fatalf("%s: chart encounter count = %d, want %d", label, len(chart.Encounters), len(chartBefore.Encounters)+1)
		}
		var inChart *Encounter
		for i := range chart.Encounters {
			if chart.Encounters[i].ID == enc.ID {
				inChart = &chart.Encounters[i]
			}
		}
		if inChart == nil || !reflect.DeepEqual(*inChart, enc) {
			t.Fatalf("%s: successful encounter missing or different in chart: want %+v, got %+v", label, enc, inChart)
		}
		// 完整档案中的同一条记录：患者归属与提交时一致。
		if inChart.PatientID != p.ID {
			t.Fatalf("%s: chart encounter patient = %q, want %q", label, inChart.PatientID, p.ID)
		}
		// 既有就诊与其记录历史原样保留。
		for _, want := range chartBefore.Encounters {
			found := false
			for _, got := range chart.Encounters {
				if reflect.DeepEqual(got, want) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s: existing encounter missing from chart: %+v", label, want)
			}
		}
		if !reflect.DeepEqual(chart.Records, chartBefore.Records) {
			t.Fatalf("%s: existing records changed by retry:\nbefore: %+v\nafter:  %+v", label, chartBefore.Records, chart.Records)
		}
		// 登记就诊不产生审计事件：成功登记后审计仍与提交前一致。
		if !reflect.DeepEqual(chart.AuditEvents, chartBefore.AuditEvents) {
			t.Fatalf("%s: audit changed by encounter registration:\nbefore: %+v\nafter:  %+v", label, chartBefore.AuditEvents, chart.AuditEvents)
		}
		// 另一名患者仍不受影响。
		otherList, err := st.ListEncounters(doc, other.ID)
		if err != nil {
			t.Fatalf("%s: other list encounters: %v", label, err)
		}
		if !reflect.DeepEqual(otherList, otherListBefore) {
			t.Fatalf("%s: other patient encounters changed by retry:\nbefore: %+v\nafter:  %+v", label, otherListBefore, otherList)
		}
		return list, chart
	}

	listAfter, chartAfter := assertRetryVisible("after retry", s2)

	// 既有就诊下的记录内容与历史在成功登记后仍可正常查看。
	records, err := s2.EncounterRecords(doc, p.ID, e1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(records, recordsBefore) {
		t.Fatalf("encounter records changed by retry:\nbefore: %+v\nafter:  %+v", recordsBefore, records)
	}
	if diagHist := findHistory(chartAfter, diagDraft.ID); diagHist == nil ||
		diagHist.CurrentVersion == nil || diagHist.CurrentVersion.ID != diagV1.ID {
		t.Fatalf("existing diagnosis history lost after retry: %+v", diagHist)
	}

	// 关闭存储后再从同一数据位置打开：仍看到最后一次成功保存的就诊集合——
	// 失败尝试不出现，成功登记不只留在当前句柄中。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })
	list, err := s3.ListEncounters(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list, listAfter) {
		t.Fatalf("encounter list not preserved after reopen:\nbefore: %+v\nafter:  %+v", listAfter, list)
	}
	chart, err := s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(chart.Encounters, chartAfter.Encounters) {
		t.Fatalf("chart encounters not preserved after reopen:\nbefore: %+v\nafter:  %+v", chartAfter.Encounters, chart.Encounters)
	}
	if failedEnc.ID != "" {
		for _, e := range list {
			if e.ID == failedEnc.ID {
				t.Fatalf("failed attempt appeared after reopen: %+v", e)
			}
		}
	}
	found := false
	for _, e := range list {
		if reflect.DeepEqual(e, enc) {
			found = true
		}
	}
	if !found {
		t.Fatalf("successful encounter %q not persisted after reopen: %+v", enc.ID, list)
	}
	// 重开后审计仍无新增，另一名患者的就诊依旧原样。
	audit, err := s3.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audit, auditBefore) {
		t.Fatalf("audit changed after reopen:\nbefore: %+v\nafter:  %+v", auditBefore, audit)
	}
	otherList, err := s3.ListEncounters(doc, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(otherList, otherListBefore) || len(otherList) != 1 || otherList[0].ID != otherEnc.ID {
		t.Fatalf("other patient encounters changed after reopen:\nbefore: %+v\nafter:  %+v", otherListBefore, otherList)
	}
}
