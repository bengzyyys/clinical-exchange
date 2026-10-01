package clinical

import (
	"fmt"
	"time"
)

// RegisterPatient 登记一名合成患者并返回其稳定标识。
// 只有内部使用者可以登记患者。
func (s *Store) RegisterPatient(actor Actor, name string) (Patient, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Patient{}, ErrAccessDenied
	}
	if name == "" {
		return Patient{}, fmt.Errorf("%w: patient name is required", ErrInvalidArgument)
	}

	var result Patient
	err := s.mutate(func(snap *snapshot) error {
		now := s.now()
		p := &Patient{
			ID:        newID("pat"),
			Name:      name,
			Source:    SyntheticSource,
			CreatedAt: now,
		}
		snap.Patients[p.ID] = p
		result = *p
		return nil
	})
	return result, err
}

// AddEncounter 在患者档案下登记一次就诊。患者必须存在且未停用。
// occurredAt 为零值时取当前时间。
func (s *Store) AddEncounter(actor Actor, patientID ID, occurredAt time.Time) (Encounter, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Encounter{}, ErrAccessDenied
	}
	if patientID == "" {
		return Encounter{}, fmt.Errorf("%w: patient id is required", ErrInvalidArgument)
	}

	var result Encounter
	err := s.mutate(func(snap *snapshot) error {
		p, err := requireActivePatient(snap, patientID)
		if err != nil {
			return err
		}
		at := occurredAt
		if at.IsZero() {
			at = s.now()
		}
		e := &Encounter{
			ID:         newID("enc"),
			PatientID:  p.ID,
			OccurredAt: at.UTC(),
			CreatedAt:  s.now(),
		}
		snap.Encounters[e.ID] = e
		result = *e
		return nil
	})
	return result, err
}

// DeactivatePatient 停用患者档案。停用后不能新增就诊、改动草稿、使记录生效、
// 进行更正或新建授权，接收方也无法继续读取；内部使用者仍可查看全部历史。
//
// 重复停用返回成功且不产生额外变化（不新增审计事件）。
func (s *Store) DeactivatePatient(actor Actor, patientID ID) error {
	if !actor.valid() || !actor.IsInternal() {
		return ErrAccessDenied
	}
	return s.mutate(func(snap *snapshot) error {
		p, err := requirePatient(snap, patientID)
		if err != nil {
			return err
		}
		if p.Deactivated {
			// 幂等：保持已有结果，不生成额外变化或事件。
			return nil
		}
		now := s.now()
		p.Deactivated = true
		s.addAudit(snap, p.ID, actor, ActionDeactivated, "patient", p.ID, now)
		return nil
	})
}

// GetPatient 返回患者档案。仅供内部使用者。
func (s *Store) GetPatient(actor Actor, patientID ID) (Patient, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Patient{}, ErrAccessDenied
	}
	var result Patient
	err := s.view(func(snap *snapshot) error {
		p, err := requirePatient(snap, patientID)
		if err != nil {
			return err
		}
		result = *p
		return nil
	})
	return result, err
}

// ListEncounters 按发生时间顺序返回患者的全部就诊。仅供内部使用者。
func (s *Store) ListEncounters(actor Actor, patientID ID) ([]Encounter, error) {
	if !actor.valid() || !actor.IsInternal() {
		return nil, ErrAccessDenied
	}
	var result []Encounter
	err := s.view(func(snap *snapshot) error {
		if _, err := requirePatient(snap, patientID); err != nil {
			return err
		}
		for _, e := range snap.Encounters {
			if e.PatientID == patientID {
				result = append(result, *e)
			}
		}
		sortEncounters(result)
		return nil
	})
	return result, err
}

func requirePatient(snap *snapshot, patientID ID) (*Patient, error) {
	p := snap.Patients[patientID]
	if p == nil {
		return nil, fmt.Errorf("%w: patient %q", ErrNotFound, patientID)
	}
	return p, nil
}

func requireActivePatient(snap *snapshot, patientID ID) (*Patient, error) {
	p, err := requirePatient(snap, patientID)
	if err != nil {
		return nil, err
	}
	if p.Deactivated {
		return nil, fmt.Errorf("%w: patient %q", ErrDeactivated, patientID)
	}
	return p, nil
}

// requireEncounter 校验就诊存在且属于给定患者。
// 引用不存在的就诊返回 ErrNotFound；就诊属于其他患者返回 ErrMismatchedPatient。
func requireEncounter(snap *snapshot, patientID, encounterID ID) (*Encounter, error) {
	e := snap.Encounters[encounterID]
	if e == nil {
		return nil, fmt.Errorf("%w: encounter %q", ErrNotFound, encounterID)
	}
	if patientID != "" && e.PatientID != patientID {
		return nil, fmt.Errorf("%w: encounter %q belongs to patient %q, not %q",
			ErrMismatchedPatient, encounterID, e.PatientID, patientID)
	}
	return e, nil
}

func validCategory(c string) bool { return c == Diagnosis || c == Order }
