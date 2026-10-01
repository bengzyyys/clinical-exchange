package clinical

import "sort"

// ListAuditEvents 返回指定患者的全部审计事件，仅供内部使用者按患者查看。
// 事件按发生时间排序；每次生效、更正、授权创建与撤回、档案停用都在此留痕，
// 失败操作不产生事件。
func (s *Store) ListAuditEvents(patientID string) ([]*AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.patientLocked(patientID); err != nil {
		return nil, err
	}
	out := make([]*AuditEvent, 0)
	for _, e := range s.audits {
		if e.PatientID == patientID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.Before(out[j].Time)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
