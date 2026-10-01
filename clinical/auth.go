package clinical

import (
	"fmt"
	"time"
)

// CreateAuthorization 为接收方建立患者的读取授权。
// 范围由明确的就诊集合与记录类别集合组成，并给出开始与截止时间。
// 空范围、跨患者就诊、开始不早于截止时间都应拒绝。患者已停用时拒绝。
func (s *Store) CreateAuthorization(operator, patientID, recipient string, visitIDs []string, categories []RecordKind, start, end time.Time) (*Authorization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.patientLocked(patientID)
	if err != nil {
		return nil, err
	}
	if !p.Active {
		return nil, fmt.Errorf("%w: 患者 %s 已停用，不能新建授权", ErrPatientInactive, patientID)
	}
	if operator == "" || recipient == "" {
		return nil, fmt.Errorf("%w: 操作身份和接收方不能为空", ErrInvalid)
	}
	if len(visitIDs) == 0 || len(categories) == 0 {
		return nil, fmt.Errorf("%w: 授权范围不能为空（就诊与类别都要明确）", ErrInvalid)
	}
	if !start.Before(end) {
		return nil, fmt.Errorf("%w: 开始时间必须早于截止时间", ErrInvalid)
	}
	visits := dedupStrings(visitIDs)
	for _, vid := range visits {
		v, ok := s.visits[vid]
		if !ok {
			return nil, fmt.Errorf("%w: 就诊 %s", ErrNotFound, vid)
		}
		if v.PatientID != patientID {
			return nil, fmt.Errorf("%w: 就诊 %s 不属于患者 %s，不能纳入授权", ErrInvalid, vid, patientID)
		}
	}
	kinds := dedupKinds(categories)
	for _, c := range kinds {
		if !validKind(c) {
			return nil, fmt.Errorf("%w: 记录类别 %q 无效", ErrInvalid, c)
		}
	}

	a := &Authorization{
		ID:         newID("a_"),
		PatientID:  patientID,
		Recipient:  recipient,
		Visits:     visits,
		Categories: kinds,
		StartAt:    start,
		EndAt:      end,
		CreatedAt:  s.now(),
		CreatedBy:  operator,
	}
	s.auths[a.ID] = a
	s.addAuditLocked(patientID, operator, "authorization.create", "authorization:"+a.ID,
		fmt.Sprintf("接收方 %s，%d 个就诊，%d 个类别", recipient, len(visits), len(kinds)))
	if err := s.commitLocked(); err != nil {
		return nil, err
	}
	return a, nil
}

// RevokeAuthorization 提前撤回授权。撤回其中一个不影响其他仍然有效的授权。
// 重复撤回返回已有结果，不产生额外变化与审计事件。
func (s *Store) RevokeAuthorization(operator, patientID, authID string) (*Authorization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if operator == "" {
		return nil, fmt.Errorf("%w: 操作身份不能为空", ErrInvalid)
	}
	if _, err := s.patientLocked(patientID); err != nil {
		return nil, err
	}
	a, ok := s.auths[authID]
	if !ok {
		return nil, fmt.Errorf("%w: 授权 %s", ErrNotFound, authID)
	}
	if a.PatientID != patientID {
		return nil, fmt.Errorf("%w: 授权 %s 不属于患者 %s", ErrInvalid, authID, patientID)
	}
	if !a.Revoked {
		a.Revoked = true
		now := s.now()
		a.RevokedAt = &now
		a.RevokedBy = &operator
		s.addAuditLocked(patientID, operator, "authorization.revoke", "authorization:"+a.ID, "撤回授权")
		if err := s.commitLocked(); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// ListAuthorizations 返回指定患者的全部授权（含已撤回），按标识排序。
func (s *Store) ListAuthorizations(patientID string) ([]*Authorization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.patientLocked(patientID); err != nil {
		return nil, err
	}
	out := make([]*Authorization, 0)
	for _, a := range s.auths {
		if a.PatientID == patientID {
			out = append(out, a)
		}
	}
	sortByID(out, func(a *Authorization) string { return a.ID })
	return out, nil
}

// ReadForRecipient 接收方按自己的身份读取指定就诊的指定类别，
// 只返回当前有效授权覆盖的记录的当前生效版本（看不到草稿、旧版本与更正原因）。
// 授权在开始时刻生效，到截止时刻失效，也可提前撤回；多个授权分别判断。
// 若请求范围没有任何有效授权覆盖，返回明确拒绝且不携带受保护内容。
// 授权允许读取范围内后来生效或更正后的记录，但不自动扩大到新增就诊。
func (s *Store) ReadForRecipient(recipient, patientID string, visitIDs []string, categories []RecordKind) ([]RecipientRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if recipient == "" {
		return nil, fmt.Errorf("%w: 接收方身份不能为空", ErrInvalid)
	}
	p, err := s.patientLocked(patientID)
	if err != nil {
		return nil, err
	}
	if !p.Active {
		return nil, fmt.Errorf("%w: 患者 %s 已停用，接收方不能继续读取", ErrPatientInactive, patientID)
	}
	if len(visitIDs) == 0 || len(categories) == 0 {
		return nil, fmt.Errorf("%w: 读取范围不能为空", ErrInvalid)
	}
	for _, c := range categories {
		if !validKind(c) {
			return nil, fmt.Errorf("%w: 记录类别 %q 无效", ErrInvalid, c)
		}
	}

	now := s.now()
	type pair struct {
		visit string
		kind  RecordKind
	}
	covered := map[pair]bool{}
	for _, a := range s.auths {
		if a.PatientID != patientID || a.Recipient != recipient || a.Revoked {
			continue
		}
		// 开始时刻生效，到截止时刻失效：[StartAt, EndAt)。
		if now.Before(a.StartAt) || !now.Before(a.EndAt) {
			continue
		}
		for _, vid := range a.Visits {
			for _, c := range a.Categories {
				covered[pair{vid, c}] = true
			}
		}
	}

	out := make([]RecipientRecord, 0)
	anyCovered := false
	for _, vid := range visitIDs {
		v, ok := s.visits[vid]
		if !ok || v.PatientID != patientID {
			continue
		}
		for _, c := range categories {
			if !covered[pair{vid, c}] {
				continue
			}
			anyCovered = true
			for _, r := range s.records {
				if r.PatientID == patientID && r.VisitID == vid && r.Kind == c && r.CurrentVersion > 0 {
					cur := r.Versions[r.CurrentVersion-1]
					out = append(out, RecipientRecord{
						RecordID: r.ID,
						VisitID:  vid,
						Kind:     c,
						Version:  cur.Version,
						Content:  cur.Content,
					})
				}
			}
		}
	}
	if !anyCovered {
		return nil, fmt.Errorf("%w: 接收方 %s 对患者 %s 的指定范围无有效授权", ErrNotAuthorized, recipient, patientID)
	}
	return out, nil
}
