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

		var recs []*Record
		for _, r := range snap.Records {
			if r.PatientID == patientID {
				recs = append(recs, r)
			}
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })

		for _, r := range recs {
			chart.Records = append(chart.Records, buildHistory(snap, r))
		}

		for _, ev := range snap.AuditEvents {
			if ev.PatientID == patientID {
				// 审计事件按追加顺序保存，即真实发生顺序；
				// 时间戳相同（如注入时钟）时也不能打乱。
				chart.AuditEvents = append(chart.AuditEvents, *ev)
			}
		}
		return nil
	})
	return chart, err
}

// AuditEvents 仅供内部使用者按患者查看审计事件，按发生时间旧到新。
func (s *Store) AuditEvents(actor Actor, patientID ID) ([]AuditEvent, error) {
	chart, err := s.Chart(actor, patientID)
	if err != nil {
		return nil, err
	}
	return chart.AuditEvents, nil
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
		var recs []*Record
		for _, r := range snap.Records {
			if r.EncounterID == encounterID {
				recs = append(recs, r)
			}
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
		for _, r := range recs {
			out = append(out, buildHistory(snap, r))
		}
		return nil
	})
	return out, err
}

func buildHistory(snap *snapshot, r *Record) RecordHistory {
	rec := *r
	// 返回的记录只是本次查询得到的独立信息。view 直接读在活快照上，
	// 若版本标识列表共享底层数组，调用方改写返回结果里的 Record.Versions
	// （换成空值或其他记录的版本标识）就会污染正式历史。这里复制一份：
	// 本地修改不进入库内状态，先后取得的两份结果、完整档案与单次就诊的
	// 结果也互不影响；草稿没有版本，保留 nil 原样。
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
