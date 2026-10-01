package clinical

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Grant 由内部使用者为一名接收方建立针对某患者的读取授权。
//
// 范围由明确的“就诊 + 类别（诊断/医嘱）”组合组成：不允许空范围，
// 每个就诊都必须属于该患者（跨患者就诊会被拒绝），类别必须合法。
// 时间窗为半开区间 [startsAt, expiresAt)：开始时刻即生效，到截止时刻
// 立即失效；开始不早于截止时间（相等也算）一律拒绝。患者档案停用后
// 不能新建授权。授权与审计事件在同一次原子落盘中保留。
func (s *Store) Grant(actor Actor, patientID ID, receiverID string, scopes []Scope, startsAt, expiresAt time.Time) (Authorization, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Authorization{}, ErrAccessDenied
	}
	if patientID == "" {
		return Authorization{}, fmt.Errorf("%w: patient id is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(receiverID) == "" {
		return Authorization{}, fmt.Errorf("%w: receiver id is required", ErrInvalidArgument)
	}
	if len(scopes) == 0 {
		return Authorization{}, fmt.Errorf("%w: authorization scope must not be empty", ErrInvalidArgument)
	}
	for _, sc := range scopes {
		if sc.EncounterID == "" {
			return Authorization{}, fmt.Errorf("%w: scope encounter id is required", ErrInvalidArgument)
		}
		if !validCategory(sc.Category) {
			return Authorization{}, fmt.Errorf("%w: scope category must be %q or %q", ErrInvalidArgument, Diagnosis, Order)
		}
	}
	if !startsAt.Before(expiresAt) {
		return Authorization{}, fmt.Errorf("%w: starts-at must be strictly before expires-at", ErrInvalidArgument)
	}

	var result Authorization
	err := s.mutate(func(snap *snapshot) error {
		if _, err := requireActivePatient(snap, patientID); err != nil {
			return err
		}

		// 去重并逐项校验：引用的就诊必须存在且属于该患者。
		seen := map[Scope]bool{}
		cleaned := make([]Scope, 0, len(scopes))
		for _, sc := range scopes {
			if _, err := requireEncounter(snap, patientID, sc.EncounterID); err != nil {
				return err
			}
			if seen[sc] {
				continue
			}
			seen[sc] = true
			cleaned = append(cleaned, sc)
		}
		sort.Slice(cleaned, func(i, j int) bool {
			if cleaned[i].EncounterID != cleaned[j].EncounterID {
				return cleaned[i].EncounterID < cleaned[j].EncounterID
			}
			return cleaned[i].Category < cleaned[j].Category
		})

		now := s.now()
		a := &Authorization{
			ID:         newID("auth"),
			PatientID:  patientID,
			ReceiverID: receiverID,
			Scopes:     cleaned,
			StartsAt:   startsAt.UTC(),
			ExpiresAt:  expiresAt.UTC(),
			CreatedAt:  now,
		}
		snap.Authorizations[a.ID] = a
		s.addAudit(snap, patientID, actor, ActionGranted, "authorization", a.ID, now)
		result = *a
		return nil
	})
	return result, err
}

// Revoke 提前撤回授权。撤回在当前时间生效；此后该授权不再覆盖任何读取。
// 重复撤回返回成功且不产生额外变化（不新增审计事件），与“停用”语义一致。
// 撤回其他患者的授权 ID 会得到 ErrMismatchedPatient/ErrNotFound。
func (s *Store) Revoke(actor Actor, patientID, authorizationID ID) error {
	if !actor.valid() || !actor.IsInternal() {
		return ErrAccessDenied
	}
	return s.mutate(func(snap *snapshot) error {
		a := snap.Authorizations[authorizationID]
		if a == nil {
			return fmt.Errorf("%w: authorization %q", ErrNotFound, authorizationID)
		}
		if a.PatientID != patientID {
			return fmt.Errorf("%w: authorization %q belongs to patient %q, not %q",
				ErrMismatchedPatient, authorizationID, a.PatientID, patientID)
		}
		if a.RevokedAt != nil {
			// 幂等：保持已有撤回结果，不生成额外变化或事件。
			return nil
		}
		now := s.now()
		a.RevokedAt = &now
		s.addAudit(snap, patientID, actor, ActionRevoked, "authorization", a.ID, now)
		return nil
	})
}

// GetAuthorization 返回单个授权。仅供内部使用者。
func (s *Store) GetAuthorization(actor Actor, patientID, authorizationID ID) (Authorization, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Authorization{}, ErrAccessDenied
	}
	var result Authorization
	err := s.view(func(snap *snapshot) error {
		a := snap.Authorizations[authorizationID]
		if a == nil {
			return fmt.Errorf("%w: authorization %q", ErrNotFound, authorizationID)
		}
		if a.PatientID != patientID {
			return fmt.Errorf("%w: authorization %q belongs to patient %q, not %q",
				ErrMismatchedPatient, authorizationID, a.PatientID, patientID)
		}
		result = *a
		return nil
	})
	return result, err
}

// ListAuthorizations 列出某患者（可选：某接收方）的全部授权，含已撤回者。
// 仅供内部使用者。
func (s *Store) ListAuthorizations(actor Actor, patientID ID, receiverID string) ([]Authorization, error) {
	if !actor.valid() || !actor.IsInternal() {
		return nil, ErrAccessDenied
	}
	var result []Authorization
	err := s.view(func(snap *snapshot) error {
		if _, err := requirePatient(snap, patientID); err != nil {
			return err
		}
		for _, a := range snap.Authorizations {
			if a.PatientID != patientID {
				continue
			}
			if receiverID != "" && a.ReceiverID != receiverID {
				continue
			}
			result = append(result, *a)
		}
		sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
		return nil
	})
	return result, err
}

// Read 供接收方按自己的身份读取某次就诊下某个类别（诊断/医嘱）的内容。
//
// 只返回在“本次读取时间”被有效授权覆盖的记录的当前生效版本：
// 授权必须属于该接收方与该患者、范围包含该就诊+类别、已开始、未到期且未撤回；
// 患者档案必须未停用。看不到草稿、旧版本或更正原因；授权范围内后来生效
// 或更正后的记录会自然包含（读取的是当前版本），但不会自动扩大到新增就诊。
//
// 若不存在任何覆盖该就诊+类别的授权，或所有相关授权都尚未开始、已到期
// 或已撤回，返回 ErrAccessDenied，结果中不含任何受保护内容。
func (s *Store) Read(actor Actor, patientID, encounterID ID, category string) (ReadResult, error) {
	if !actor.valid() || actor.Kind != "receiver" {
		return ReadResult{}, ErrAccessDenied
	}
	if patientID == "" || encounterID == "" {
		return ReadResult{}, fmt.Errorf("%w: patient and encounter ids are required", ErrInvalidArgument)
	}
	if !validCategory(category) {
		return ReadResult{}, fmt.Errorf("%w: category must be %q or %q", ErrInvalidArgument, Diagnosis, Order)
	}

	now := s.now()
	var out ReadResult
	err := s.view(func(snap *snapshot) error {
		// 先看是否存在覆盖该接收方 + 患者 + 就诊 + 类别的有效授权。
		// 不提前区分“对象不存在/属于他人”与“无授权”，统一返回拒绝，
		// 从而不向接收方泄露患者或就诊是否存在。
		covered := false
		patientDeactivated := false
		if p := snap.Patients[patientID]; p != nil {
			patientDeactivated = p.Deactivated
		}
		for _, a := range snap.Authorizations {
			if a.PatientID != patientID || a.ReceiverID != actor.ID {
				continue
			}
			if !a.ActiveAt(now) {
				continue
			}
			if authorizationCovers(a, encounterID, category) {
				covered = true
				break
			}
		}
		if !covered || patientDeactivated {
			// 无授权、未开始、已到期、已撤回或档案停用：明确拒绝，
			// 结果中不含任何受保护内容。
			return ErrAccessDenied
		}

		// 能被授权覆盖的就诊必然属于该患者（Grant 时已校验），
		// 此处再确认一次以防任何不一致状态。
		e := snap.Encounters[encounterID]
		if e == nil || e.PatientID != patientID {
			return ErrAccessDenied
		}

		out.EncounterID = encounterID
		out.Category = category
		var recs []EffectiveRecord
		for _, r := range snap.Records {
			if r.EncounterID != encounterID || r.Category != category || r.PatientID != patientID {
				continue
			}
			if eff, ok := currentEffective(snap, r); ok {
				recs = append(recs, eff)
			}
			// 仅有草稿、尚无生效版本的记录天然不出现。
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID })
		out.Records = recs
		return nil
	})
	return out, err
}

func authorizationCovers(a *Authorization, encounterID ID, category string) bool {
	for _, sc := range a.Scopes {
		if sc.EncounterID == encounterID && sc.Category == category {
			return true
		}
	}
	return false
}
