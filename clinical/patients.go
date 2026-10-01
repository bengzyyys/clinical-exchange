package clinical

import (
	"fmt"
)

// RegisterPatient 登记一名合成患者，返回带稳定标识的患者档案。
func (s *Store) RegisterPatient(operator, name string) (*Patient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if operator == "" || name == "" {
		return nil, fmt.Errorf("%w: 操作身份和患者姓名不能为空", ErrInvalid)
	}
	p := &Patient{
		ID:        newID("p_"),
		Name:      name,
		Active:    true,
		CreatedAt: s.now(),
	}
	s.patients[p.ID] = p
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return p, nil
}

// AddVisit 在患者名下登记一次就诊。患者已停用时拒绝。
func (s *Store) AddVisit(operator, patientID, label string) (*Visit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.patientLocked(patientID)
	if err != nil {
		return nil, err
	}
	if !p.Active {
		return nil, fmt.Errorf("%w: 患者 %s 已停用，不能新增就诊", ErrPatientInactive, patientID)
	}
	if operator == "" || label == "" {
		return nil, fmt.Errorf("%w: 操作身份和就诊标签不能为空", ErrInvalid)
	}
	v := &Visit{
		ID:        newID("v_"),
		PatientID: patientID,
		Label:     label,
		CreatedAt: s.now(),
	}
	s.visits[v.ID] = v
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return v, nil
}

// DeactivatePatient 停用患者档案。停用后不能新增就诊、改动草稿、
// 使记录生效、更正或新建授权，接收方也不能继续读取；内部查看不受影响。
// 重复停用返回已有结果，不产生额外变化与审计事件。
func (s *Store) DeactivatePatient(operator, patientID string) (*Patient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if operator == "" {
		return nil, fmt.Errorf("%w: 操作身份不能为空", ErrInvalid)
	}
	p, err := s.patientLocked(patientID)
	if err != nil {
		return nil, err
	}
	if p.Active {
		p.Active = false
		now := s.now()
		p.DeactivatedAt = &now
		s.addAuditLocked(patientID, operator, "patient.deactivate", "patient:"+p.ID, "停用患者档案")
		if err := s.commitLocked(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// GetPatient 返回患者档案。
func (s *Store) GetPatient(patientID string) (*Patient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.patientLocked(patientID)
}

// ListPatients 返回全部患者档案，按标识排序。
func (s *Store) ListPatients() []*Patient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedPatients()
}

// ListVisits 返回指定患者的就诊，按标识排序。
func (s *Store) ListVisits(patientID string) ([]*Visit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.patientLocked(patientID); err != nil {
		return nil, err
	}
	out := make([]*Visit, 0)
	for _, v := range s.visits {
		if v.PatientID == patientID {
			out = append(out, v)
		}
	}
	sortByID(out, func(v *Visit) string { return v.ID })
	return out, nil
}
