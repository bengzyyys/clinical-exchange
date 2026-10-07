package clinical

import (
	"sort"
)

// RecordHistory 是内部使用者看到的一条记录的完整视图：
// 草稿（若有）与全部历史版本（旧到新，含更正原因）。
type RecordHistory struct {
	Record         Record
	DraftContent   string
	HasDraft       bool
	CurrentVersion *Version
	Versions       []Version // 旧到新
}

// PatientChart 是内部使用者按患者看到的完整档案。
type PatientChart struct {
	Patient     Patient
	Encounters  []Encounter
	Records     []RecordHistory
	AuditEvents []AuditEvent
}

// Chart 返回患者的完整档案，包含所有草稿、全部版本（含旧内容、版本关系
// 与更正原因）以及审计事件。仅供内部使用者；患者停用后仍可查看。
func (s *Store) Chart(actor Actor, patientID ID) (PatientChart, error) {
	if !actor.valid() || !actor.IsInternal() {
		return PatientChart{}, ErrAccessDenied
	}
	var chart PatientChart
	err := s.view(func(snap *snapshot) error {
		p, err := requirePatient(snap, patientID)
		if err != nil {
			return err
		}
		chart.Patient = *p

		for _, e := range snap.Encounters {
			if e.PatientID == patientID {
				chart.Encounters = append(chart.Encounters, *e)
			}
		}
		sortEncounters(chart.Encounters)

		chart.Records = recordHistories(snap, func(r *Record) bool {
			return r.PatientID == patientID
		})

		chart.AuditEvents = auditEventsFor(snap, patientID)
		return nil
	})
	return chart, err
}

// AuditEvents 仅供内部使用者按患者查看审计事件，按发生时间旧到新。
// 独立查询：只整理审计历史，不连带就诊、草稿与版本历史；患者停用后仍可查看。
func (s *Store) AuditEvents(actor Actor, patientID ID) ([]AuditEvent, error) {
	if !actor.valid() || !actor.IsInternal() {
		return nil, ErrAccessDenied
	}
	var events []AuditEvent
	err := s.view(func(snap *snapshot) error {
		if _, err := requirePatient(snap, patientID); err != nil {
			return err
		}
		events = auditEventsFor(snap, patientID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// auditEventsFor 收集指定患者的全部审计事件，是 AuditEvents 与 Chart 共用的
// 规则：按追加顺序（即真实发生顺序；时间戳相同时也保持操作先后）逐条拷贝，
// 返回的列表与存储侧完全脱离，调用方的本地整理不会写回正式历史。
func auditEventsFor(snap *snapshot, patientID ID) []AuditEvent {
	var events []AuditEvent
	for _, ev := range snap.AuditEvents {
		if ev.PatientID == patientID {
			events = append(events, *ev)
		}
	}
	return events
}

// EncounterRecords 仅供内部使用者查看某次就诊下的记录及完整历史。
func (s *Store) EncounterRecords(actor Actor, patientID, encounterID ID) ([]RecordHistory, error) {
	if !actor.valid() || !actor.IsInternal() {
		return nil, ErrAccessDenied
	}
	var out []RecordHistory
	err := s.view(func(snap *snapshot) error {
		if _, err := requirePatient(snap, patientID); err != nil {
			return err
		}
		if _, err := requireEncounter(snap, patientID, encounterID); err != nil {
			return err
		}
		out = recordHistories(snap, func(r *Record) bool {
			return r.EncounterID == encounterID
		})
		return nil
	})
	return out, err
}

// recordHistories 是 Chart 与 EncounterRecords 共用的记录列表整理规则：
// 按 match 收集记录，按记录标识的字符串顺序升序排列（不是录入时间顺序），
// 再逐条生成完整历史视图。两个入口只在筛选条件上不同——完整档案按患者、
// 单次就诊查询按就诊——因此同一就诊的记录在两种入口下内容、顺序与历史
// 必然一致。没有匹配记录时返回 nil，保留原有的空结果表示。
func recordHistories(snap *snapshot, match func(*Record) bool) []RecordHistory {
	var recs []*Record
	for _, r := range snap.Records {
		if match(r) {
			recs = append(recs, r)
		}
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	var out []RecordHistory
	for _, r := range recs {
		out = append(out, buildHistory(snap, r))
	}
	return out
}

func buildHistory(snap *snapshot, r *Record) RecordHistory {
	// 返回的记录必须是独立副本：Versions 切片与存储中的记录共享底层数组时，
	// 调用方改动查询结果会写回正式历史。这里深拷贝，保证修改手中的结果
	// 只影响该结果本身。
	rec := *r
	rec.Versions = append([]ID(nil), r.Versions...)
	rh := RecordHistory{
		Record:       rec,
		DraftContent: r.DraftContent,
		HasDraft:     r.HasDraft,
	}
	for _, vid := range r.Versions {
		if v := snap.Versions[vid]; v != nil {
			rh.Versions = append(rh.Versions, *v)
		}
	}
	sort.Slice(rh.Versions, func(i, j int) bool { return rh.Versions[i].Number < rh.Versions[j].Number })
	if r.CurrentVersionID != "" {
		if v := snap.Versions[r.CurrentVersionID]; v != nil {
			cv := *v
			rh.CurrentVersion = &cv
		}
	}
	return rh
}

// currentEffective 取出记录当前生效版本的对外内容；无生效版本时第二返回值为 false。
func currentEffective(snap *snapshot, r *Record) (EffectiveRecord, bool) {
	if r.CurrentVersionID == "" {
		return EffectiveRecord{}, false
	}
	v := snap.Versions[r.CurrentVersionID]
	if v == nil {
		return EffectiveRecord{}, false
	}
	return EffectiveRecord{
		RecordID:    r.ID,
		VersionID:   v.ID,
		Version:     v.Number,
		Content:     v.Content,
		EffectiveAt: v.CreatedAt,
	}, true
}

func sortEncounters(es []Encounter) {
	sort.Slice(es, func(i, j int) bool {
		if !es[i].OccurredAt.Equal(es[j].OccurredAt) {
			return es[i].OccurredAt.Before(es[j].OccurredAt)
		}
		return es[i].ID < es[j].ID
	})
}
