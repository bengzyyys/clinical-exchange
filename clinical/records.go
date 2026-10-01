package clinical

import (
	"fmt"
)

// SaveDraft 保存诊断或医嘱草稿。同一就诊同一类别下已有草稿时更新其内容，
// 否则新建一条记录。引用不存在的就诊或就诊属于其他患者时明确失败，
// 不产生半条记录。患者已停用时拒绝。
func (s *Store) SaveDraft(operator, patientID, visitID string, kind RecordKind, content string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.patientLocked(patientID)
	if err != nil {
		return nil, err
	}
	if !p.Active {
		return nil, fmt.Errorf("%w: 患者 %s 已停用，不能改动草稿", ErrPatientInactive, patientID)
	}
	if operator == "" || content == "" {
		return nil, fmt.Errorf("%w: 操作身份和内容不能为空", ErrInvalid)
	}
	if !validKind(kind) {
		return nil, fmt.Errorf("%w: 记录类别 %q 无效", ErrInvalid, kind)
	}
	v, ok := s.visits[visitID]
	if !ok {
		return nil, fmt.Errorf("%w: 就诊 %s", ErrNotFound, visitID)
	}
	if v.PatientID != patientID {
		return nil, fmt.Errorf("%w: 就诊 %s 不属于患者 %s", ErrInvalid, visitID, patientID)
	}

	var rec *Record
	for _, r := range s.records {
		if r.VisitID == visitID && r.Kind == kind && r.Draft != nil {
			rec = r
			break
		}
	}
	if rec == nil {
		rec = &Record{
			ID:        newID("r_"),
			PatientID: patientID,
			VisitID:   visitID,
			Kind:      kind,
		}
		s.records[rec.ID] = rec
	}
	rec.Draft = &RecordDraft{
		Content:   content,
		Operator:  operator,
		UpdatedAt: s.now(),
	}
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return rec, nil
}

// DeleteDraft 删除一条尚未生效的草稿记录。已生效记录不得删除；
// 患者已停用时拒绝。
func (s *Store) DeleteDraft(operator, patientID, recordID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.patientLocked(patientID)
	if err != nil {
		return err
	}
	if !p.Active {
		return fmt.Errorf("%w: 患者 %s 已停用，不能删除草稿", ErrPatientInactive, patientID)
	}
	if operator == "" {
		return fmt.Errorf("%w: 操作身份不能为空", ErrInvalid)
	}
	rec, err := s.recordForPatientLocked(patientID, recordID)
	if err != nil {
		return err
	}
	if rec.Draft == nil {
		return fmt.Errorf("%w: 记录 %s 没有可删除的草稿（已生效记录不得删除）", ErrInvalid, recordID)
	}
	delete(s.records, recordID)
	return s.commitLocked()
}

// Activate 将草稿生效，保存生效当时的完整内容与时间，生成第 1 版。
// 生效记录不得被直接覆盖或删除。患者已停用时拒绝。
func (s *Store) Activate(operator, patientID, recordID string) (*RecordVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.patientLocked(patientID)
	if err != nil {
		return nil, err
	}
	if !p.Active {
		return nil, fmt.Errorf("%w: 患者 %s 已停用，不能使记录生效", ErrPatientInactive, patientID)
	}
	if operator == "" {
		return nil, fmt.Errorf("%w: 操作身份不能为空", ErrInvalid)
	}
	rec, err := s.recordForPatientLocked(patientID, recordID)
	if err != nil {
		return nil, err
	}
	if rec.Draft == nil {
		return nil, fmt.Errorf("%w: 记录 %s 没有可生效的草稿", ErrInvalid, recordID)
	}
	v := RecordVersion{
		Version:   1,
		Content:   rec.Draft.Content,
		Operator:  operator,
		CreatedAt: s.now(),
	}
	rec.Versions = []RecordVersion{v}
	rec.CurrentVersion = 1
	rec.Draft = nil
	s.addAuditLocked(patientID, operator, "record.activate", "record:"+rec.ID, "草稿生效为第 1 版")
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	out := rec.Versions[0]
	return &out, nil
}

// Correct 对已生效记录进行更正。必须提供非空原因并指明当前版本；
// 成功后生成同一记录的新版本，保留旧内容、版本关系和原因。
// 若指定版本已不是当前版本，拒绝更正并保持现状。患者已停用时拒绝。
func (s *Store) Correct(operator, patientID, recordID string, currentVersion int, reason, content string) (*RecordVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.patientLocked(patientID)
	if err != nil {
		return nil, err
	}
	if !p.Active {
		return nil, fmt.Errorf("%w: 患者 %s 已停用，不能更正记录", ErrPatientInactive, patientID)
	}
	if operator == "" || reason == "" || content == "" {
		return nil, fmt.Errorf("%w: 操作身份、更正原因和内容不能为空", ErrInvalid)
	}
	rec, err := s.recordForPatientLocked(patientID, recordID)
	if err != nil {
		return nil, err
	}
	if rec.CurrentVersion == 0 {
		return nil, fmt.Errorf("%w: 记录 %s 尚未生效，不能更正", ErrInvalid, recordID)
	}
	if currentVersion != rec.CurrentVersion {
		return nil, fmt.Errorf("%w: 记录 %s 指定版本 %d 已不是当前版本 %d", ErrConflict, recordID, currentVersion, rec.CurrentVersion)
	}
	newVersion := rec.CurrentVersion + 1
	v := RecordVersion{
		Version:   newVersion,
		Content:   content,
		Reason:    reason,
		Operator:  operator,
		CreatedAt: s.now(),
	}
	rec.Versions = append(rec.Versions, v)
	rec.CurrentVersion = newVersion
	s.addAuditLocked(patientID, operator, "record.correct", "record:"+rec.ID,
		fmt.Sprintf("第 %d 版更正为第 %d 版", currentVersion, newVersion))
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	out := rec.Versions[newVersion-1]
	return &out, nil
}

// GetRecord 返回记录的完整信息（含草稿与全部版本历史），供内部使用者查看。
func (s *Store) GetRecord(patientID, recordID string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.patientLocked(patientID); err != nil {
		return nil, err
	}
	return s.recordForPatientLocked(patientID, recordID)
}

// ListRecords 返回指定患者的全部记录（含草稿与完整历史），按标识排序。
func (s *Store) ListRecords(patientID string) ([]*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.patientLocked(patientID); err != nil {
		return nil, err
	}
	out := make([]*Record, 0)
	for _, r := range s.records {
		if r.PatientID == patientID {
			out = append(out, r)
		}
	}
	sortByID(out, func(r *Record) string { return r.ID })
	return out, nil
}
