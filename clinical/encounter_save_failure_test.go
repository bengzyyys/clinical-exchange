package clinical

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestAddEncounterSaveFailureRejectsAndAllowsRetry 覆盖“内部使用者为未停用的
// 合成患者调用 AddEncounter，患者标识与发生时间都合法，但本机数据暂时无法保存”
// 的登记就诊场景。
//
// 登记必须明确返回保存错误：不能表现为成功，也不能误报为患者不存在、档案停用
// 或参数非法。保障以使用者随后能看到的正式结果为准：失败后就诊列表与完整档案
// 中的就诊集合都与提交前一致——没有新增、替换或丢失；已有就诊（其中一次已
// 保存诊断）的标识、所属患者、发生时间、创建时间与其下记录历史保持原值；
// AddEncounter 本就不产生审计，失败尝试同样不追加或改动审计；另一名患者的
// 就诊不受影响。调用失败时手中的临时就诊不是正式就诊。
//
// 保存条件恢复后重新提交合法登记必须成功：两个查询只多出成功的这一条并保留
// 全部原有就诊，成功返回的标识在两个查询中对应同一条记录。显式发生时间保留
// 时刻并转为 UTC；零值发生时间取成功登记时的当前时间，不沿用失败尝试的时间；
// 创建时间属于成功的这次操作。关闭后从同一数据位置重新打开，只看到最后一次
// 成功保存的就诊集合，失败尝试不会在重开后出现。
func TestAddEncounterSaveFailureRejectsAndAllowsRetry(t *testing.T) {
	t.Run("explicit occurred time keeps instant as UTC", func(t *testing.T) {
		// 显式给出的发生时间（+09:00 表示）必须保留它表示的同一时刻，
		// 并按现有规则转换为 UTC 保存。
		zone := time.FixedZone("UTC+9", 9*60*60)
		failedInput := time.Date(2026, 8, 15, 18, 0, 0, 0, zone) // 09:00 UTC
		successInput := time.Date(2026, 8, 20, 18, 30, 0, 0, zone)
		runEncounterSaveFailure(t, failedInput, successInput)
	})

	t.Run("zero occurred time uses successful attempt time", func(t *testing.T) {
		// 零值发生时间取“成功登记时”的当前时间，不能沿用失败尝试时的时间。
		runEncounterSaveFailure(t, time.Time{}, time.Time{})
	})
}

func runEncounterSaveFailure(t *testing.T, failedInput, successInput time.Time) {
	t.Helper()
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)}
	open := func() *Store {
		s, err := Open(dir, WithClock(func() time.Time { return clk.t }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return s
	}

	s := open()

	// 目标患者未停用，原先已有两次就诊；第一次就诊下已保存一条生效诊断
	// （带版本历史）。另一名患者持有自己的就诊，用于检验跨患者隔离。
	p, err := s.RegisterPatient(doc, "就诊保存失败患者")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.RegisterPatient(doc2, "就诊保存失败的其他患者")
	if err != nil {
		t.Fatal(err)
	}

	e1, err := s.AddEncounter(doc, p.ID, clk.t)
	if err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Hour)
	e2, err := s.AddEncounter(doc, p.ID, clk.t)
	if err != nil {
		t.Fatal(err)
	}
	diagContent := "既有诊断：高血压 I10"
	diagDraft, err := s.CreateDraft(doc, p.ID, e1.ID, Diagnosis, diagContent)
	if err != nil {
		t.Fatal(err)
	}
	diagV1, err := s.ActivateRecord(doc, diagDraft.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherEnc, err := s.AddEncounter(doc2, other.ID, clk.t.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// 提交前的正式基线：就诊列表、完整档案、已有就诊下的记录历史、审计，
	// 以及另一名患者的就诊集合。
	listBefore, err := s.ListEncounters(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	chartBefore, err := s.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	e1HistoryBefore, err := s.EncounterRecords(doc, p.ID, e1.ID)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := s.AuditEvents(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherListBefore, err := s.ListEncounters(doc2, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listBefore) != 2 || encounterByID(listBefore, e1.ID) == nil ||
		encounterByID(listBefore, e2.ID) == nil {
		t.Fatalf("unexpected baseline encounters: %+v", listBefore)
	}
	if len(chartBefore.Encounters) != 2 {
		t.Fatalf("unexpected baseline chart encounter count: %d", len(chartBefore.Encounters))
	}
	if len(otherListBefore) != 1 || encounterByID(otherListBefore, otherEnc.ID) == nil {
		t.Fatalf("unexpected baseline other patient encounters: %+v", otherListBefore)
	}
	if got := auditActions(auditBefore); got[ActionActivated] != 1 {
		t.Fatalf("unexpected baseline audit: %v", got)
	}

	// checkIntact 断言：失败尝试之后（以及重开之后），使用者能看到的正式
	// 结果与提交前完全一致。failedID 是失败调用手中临时就诊的标识，它在
	// 任何正式查询中都不允许出现。
	checkIntact := func(label string, st *Store, failedID ID) {
		t.Helper()

		// 档案仍能正常查询——但这不意味着登记已保存。
		gotPatient, err := st.GetPatient(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: get patient: %v", label, err)
		}
		if gotPatient.Deactivated || gotPatient.ID != p.ID {
			t.Fatalf("%s: patient changed: %+v", label, gotPatient)
		}

		// 就诊列表与提交前逐字一致：没有新增、替换或丢失。
		list, err := st.ListEncounters(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: list encounters: %v", label, err)
		}
		if !reflect.DeepEqual(list, listBefore) {
			t.Fatalf("%s: encounter list changed:\nbefore: %+v\nafter:  %+v", label, listBefore, list)
		}
		if encounterByID(list, failedID) != nil {
			t.Fatalf("%s: failed attempt's temporary encounter %q entered the official list", label, failedID)
		}

		// 完整档案中的就诊集合同样一致，且与列表是同一组正式就诊。
		chart, err := st.Chart(doc, p.ID)
		if err != nil {
			t.Fatalf("%s: chart: %v", label, err)
		}
		if !reflect.DeepEqual(chart.Encounters, listBefore) {
			t.Fatalf("%s: chart encounters changed:\nbefore: %+v\nafter:  %+v", label, listBefore, chart.Encounters)
		}
		if encounterByID(chart.Encounters, failedID) != nil {
			t.Fatalf("%s: failed attempt's temporary encounter %q entered the chart", label, failedID)
		}

		// 已有就诊的标识、所属患者、发生时间、创建时间保持原值。
		for _, want := range listBefore {
			got := encounterByID(list, want.ID)
			if got == nil || !reflect.DeepEqual(*got, want) {
				t.Fatalf("%s: existing encounter %q changed or missing: before=%+v after=%+v",
					label, want.ID, want, got)
			}
		}

		// 已有就诊下的记录内容与版本历史仍可正常查看，原样保留。
		e1History, err := st.EncounterRecords(doc, p.ID, e1.ID)
		if err != nil {
			t.Fatalf("%s: encounter records of e1: %v", label, err)
		}
		if !reflect.DeepEqual(e1History, e1HistoryBefore) {
			t.Fatalf("%s: e1 record history changed:\nbefore: %+v\nafter:  %+v",
				label, e1HistoryBefore, e1History)
		}
		if h := findHistory(chart, diagDraft.ID); h == nil || h.CurrentVersion == nil ||
			h.CurrentVersion.ID != diagV1.ID || h.CurrentVersion.Content != diagContent {
			t.Fatalf("%s: saved diagnosis history not viewable: %+v", label, h)
		}
		if !reflect.DeepEqual(chart.Records, chartBefore.Records) {
			t.Fatalf("%s: chart records changed:\nbefore: %+v\nafter:  %+v",
				label, chartBefore.Records, chart.Records)
		}

		// 登记就诊本就不产生审计；失败尝试既不能追加事件，也不能改动原有审计。
		if !reflect.DeepEqual(chart.AuditEvents, auditBefore) {
			t.Fatalf("%s: audit events changed:\nbefore: %+v\nafter:  %+v",
				label, auditBefore, chart.AuditEvents)
		}

		// 临时就诊不能当作正式就诊使用：按它查询记录得到“就诊不存在”。
		if _, err := st.EncounterRecords(doc, p.ID, failedID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: temporary encounter %q usable as official: err=%v, want ErrNotFound",
				label, failedID, err)
		}

		// 另一名患者的就诊不受这次失败影响。
		otherList, err := st.ListEncounters(doc2, other.ID)
		if err != nil {
			t.Fatalf("%s: list other patient encounters: %v", label, err)
		}
		if !reflect.DeepEqual(otherList, otherListBefore) {
			t.Fatalf("%s: other patient encounters changed:\nbefore: %+v\nafter:  %+v",
				label, otherListBefore, otherList)
		}
		otherChart, err := st.Chart(doc2, other.ID)
		if err != nil {
			t.Fatalf("%s: other patient chart: %v", label, err)
		}
		if !reflect.DeepEqual(otherChart.Encounters, otherListBefore) {
			t.Fatalf("%s: other patient chart encounters changed:\nbefore: %+v\nafter:  %+v",
				label, otherListBefore, otherChart.Encounters)
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

	// 身份有权、患者存在且未停用、发生时间合法，仅本地保存失败。
	failTime := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	clk.t = failTime
	failed, err := s.AddEncounter(doc, p.ID, failedInput)
	if err == nil {
		t.Fatal("encounter registration must fail when local save fails")
	}
	// 必须是明确的保存错误，不能伪装成任何业务结果：不是患者不存在、
	// 档案停用、参数非法或权限问题。
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrDeactivated) ||
		errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrAccessDenied) ||
		errors.Is(err, ErrMismatchedPatient) || errors.Is(err, ErrConflict) {
		t.Fatalf("save failure surfaced as a business error: %v", err)
	}
	// 失败不能留下半截写入的临时文件。
	if _, statErr := os.Stat(dataPath + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("failed save left a temp file: %v", statErr)
	}
	// 非 nil 错误时返回值没有正式意义：即便其中携带了调用过程中准备好的
	// 临时就诊标识，它也没有成为正式数据（下面的正式状态断言会逐一排除）。
	_ = failed

	// 失败后：列表、完整档案、记录历史、审计与另一名患者全部维持提交前状态。
	checkIntact("after failed save", s, failed.ID)

	// 关闭后从原数据位置重新打开：先恢复保存条件（还原原数据文件）。
	// 失败尝试不能在重开后出现。
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
	checkIntact("after reopen", s2, failed.ID)

	// 保存条件恢复后：对同一患者重新提交合法登记，必须正常成功。
	successTime := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	clk.t = successTime
	enc, err := s2.AddEncounter(doc, p.ID, successInput)
	if err != nil {
		t.Fatalf("retry after save recovered: %v", err)
	}
	if enc.ID == "" || enc.ID == failed.ID {
		t.Fatalf("retry must create a new official encounter distinct from the failed attempt: failed=%q got=%+v",
			failed.ID, enc)
	}
	if enc.PatientID != p.ID {
		t.Fatalf("new encounter patient = %q, want %q", enc.PatientID, p.ID)
	}
	// 创建时间属于成功的这次操作，不是失败尝试的时刻。
	if !enc.CreatedAt.Equal(successTime) || enc.CreatedAt.Equal(failTime) {
		t.Fatalf("created at %v does not belong to the successful attempt (failed at %v)",
			enc.CreatedAt, failTime)
	}
	// 发生时间：显式给出时保留同一时刻并转为 UTC；零值时取成功登记时的
	// 当前时间，不能沿用失败尝试时的时间。
	if !successInput.IsZero() {
		if !enc.OccurredAt.Equal(successInput) {
			t.Fatalf("occurred at %v lost the submitted instant %v", enc.OccurredAt, successInput)
		}
		if enc.OccurredAt.Location() != time.UTC {
			t.Fatalf("occurred at location = %v, want UTC", enc.OccurredAt.Location())
		}
	} else {
		if !enc.OccurredAt.Equal(successTime) || enc.OccurredAt.Equal(failTime) {
			t.Fatalf("zero occurred at %v must use success time %v, not failed attempt time %v",
				enc.OccurredAt, successTime, failTime)
		}
	}

	// 成功后：就诊列表只多出这一条，原有就诊全部保留；失败尝试仍不出现。
	listAfter, err := s2.ListEncounters(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listAfter) != len(listBefore)+1 {
		t.Fatalf("encounter count after retry = %d, want %d", len(listAfter), len(listBefore)+1)
	}
	if encounterByID(listAfter, failed.ID) != nil {
		t.Fatal("failed attempt's temporary encounter appeared after retry")
	}
	newInList := encounterByID(listAfter, enc.ID)
	if newInList == nil {
		t.Fatalf("new encounter %q missing from list after retry", enc.ID)
	}
	if !reflect.DeepEqual(*newInList, enc) {
		t.Fatalf("list entry differs from returned encounter:\nreturned: %+v\nlisted:   %+v",
			enc, newInList)
	}
	// 原有每一条就诊仍是原值。
	for _, want := range listBefore {
		if got := encounterByID(listAfter, want.ID); got == nil || !reflect.DeepEqual(*got, want) {
			t.Fatalf("existing encounter %q changed or lost after retry: before=%+v after=%+v",
				want.ID, want, got)
		}
	}

	// 完整档案同样只多出这一条，且成功返回的标识在两个查询结果中对应
	// 同一条记录（内容逐字一致）、患者归属与提交时一致。
	chartAfter, err := s2.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chartAfter.Encounters) != len(listBefore)+1 {
		t.Fatalf("chart encounter count after retry = %d, want %d",
			len(chartAfter.Encounters), len(listBefore)+1)
	}
	newInChart := encounterByID(chartAfter.Encounters, enc.ID)
	if newInChart == nil {
		t.Fatalf("new encounter %q missing from chart after retry", enc.ID)
	}
	if !reflect.DeepEqual(*newInChart, *newInList) {
		t.Fatalf("new encounter differs between list and chart:\nlist:  %+v\nchart: %+v",
			newInList, newInChart)
	}
	if newInChart.PatientID != p.ID {
		t.Fatalf("new encounter in chart patient = %q, want %q", newInChart.PatientID, p.ID)
	}
	if encounterByID(chartAfter.Encounters, failed.ID) != nil {
		t.Fatal("failed attempt's temporary encounter appeared in chart after retry")
	}
	for _, want := range listBefore {
		if got := encounterByID(chartAfter.Encounters, want.ID); got == nil || !reflect.DeepEqual(*got, want) {
			t.Fatalf("existing encounter %q changed or lost in chart after retry: before=%+v after=%+v",
				want.ID, want, got)
		}
	}

	// 成功的登记同样不产生审计，原有记录历史保持可查看且不变。
	if !reflect.DeepEqual(chartAfter.AuditEvents, auditBefore) {
		t.Fatalf("audit events changed after retry:\nbefore: %+v\nafter:  %+v",
			auditBefore, chartAfter.AuditEvents)
	}
	if !reflect.DeepEqual(chartAfter.Records, chartBefore.Records) {
		t.Fatalf("chart records changed after retry:\nbefore: %+v\nafter:  %+v",
			chartBefore.Records, chartAfter.Records)
	}

	// 另一名患者的就诊在成功登记后仍不受影响。
	otherListAfter, err := s2.ListEncounters(doc2, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(otherListAfter, otherListBefore) {
		t.Fatalf("other patient encounters changed after retry:\nbefore: %+v\nafter:  %+v",
			otherListBefore, otherListAfter)
	}

	// 关闭存储后从同一数据位置重新打开：仍看到最后一次成功保存的就诊集合。
	// 失败尝试不能在重开后出现，成功登记不能只留在当前句柄中。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := open()
	t.Cleanup(func() { _ = s3.Close() })

	listReopened, err := s3.ListEncounters(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listReopened, listAfter) {
		t.Fatalf("encounter list after reopen differs from saved success state:\nsaved:  %+v\nreopen: %+v",
			listAfter, listReopened)
	}
	if encounterByID(listReopened, failed.ID) != nil {
		t.Fatal("failed attempt's temporary encounter appeared after final reopen")
	}
	reopened := encounterByID(listReopened, enc.ID)
	if reopened == nil {
		t.Fatal("successfully registered encounter did not survive reopen")
	}
	if !reflect.DeepEqual(*reopened, enc) || reopened.PatientID != p.ID {
		t.Fatalf("reopened new encounter differs:\nsaved:  %+v\nreopen: %+v", enc, reopened)
	}
	chartReopened, err := s3.Chart(doc, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(chartReopened.Encounters, listAfter) {
		t.Fatalf("chart encounters after reopen differ:\nsaved:  %+v\nreopen: %+v",
			listAfter, chartReopened.Encounters)
	}
	otherReopened, err := s3.ListEncounters(doc2, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(otherReopened, otherListBefore) {
		t.Fatalf("other patient encounters changed after final reopen:\nbefore: %+v\nafter:  %+v",
			otherListBefore, otherReopened)
	}
}

// encounterByID 在就诊集合中按稳定标识查找，找不到返回 nil。
func encounterByID(encs []Encounter, id ID) *Encounter {
	for i := range encs {
		if encs[i].ID == id {
			return &encs[i]
		}
	}
	return nil
}
