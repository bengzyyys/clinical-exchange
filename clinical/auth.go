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

// GrantSelected 由内部使用者为一名接收方建立针对某患者的读取授权，
// 可同时包含整类范围（scopes）与限定记录范围（selected），两者可并存。
//
// scopes 为整类范围，允许为空（纯限定授权）；selected 为明确选中的已生效
// 记录，不允许为空——空选择不能被当成整类授权。每条选中记录必须存在、
// 属于该患者、已经生效（非草稿），且与声明的就诊、类别一致。
// 重复选择同一记录只算一次。任一选择不合法即拒绝整条授权，不保存合法部分，
// 不新增审计。时间窗、患者停用、原子落盘等规则与 Grant 相同。
func (s *Store) GrantSelected(actor Actor, patientID ID, receiverID string, scopes []Scope, selected []SelectedRecord, startsAt, expiresAt time.Time) (Authorization, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Authorization{}, ErrAccessDenied
	}
	if patientID == "" {
		return Authorization{}, fmt.Errorf("%w: patient id is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(receiverID) == "" {
		return Authorization{}, fmt.Errorf("%w: receiver id is required", ErrInvalidArgument)
	}
	if len(selected) == 0 {
		return Authorization{}, fmt.Errorf("%w: selected records must not be empty", ErrInvalidArgument)
	}
	for _, sc := range scopes {
		if sc.EncounterID == "" {
			return Authorization{}, fmt.Errorf("%w: scope encounter id is required", ErrInvalidArgument)
		}
		if !validCategory(sc.Category) {
			return Authorization{}, fmt.Errorf("%w: scope category must be %q or %q", ErrInvalidArgument, Diagnosis, Order)
		}
	}
	for _, sr := range selected {
		if strings.TrimSpace(sr.RecordID) == "" {
			return Authorization{}, fmt.Errorf("%w: selected record id must not be blank", ErrInvalidArgument)
		}
		if sr.EncounterID == "" {
			return Authorization{}, fmt.Errorf("%w: selected record encounter id is required", ErrInvalidArgument)
		}
		if !validCategory(sr.Category) {
			return Authorization{}, fmt.Errorf("%w: selected record category must be %q or %q", ErrInvalidArgument, Diagnosis, Order)
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

		// 整类范围：去重并校验就诊存在且属于该患者。
		seenScopes := map[Scope]bool{}
		cleanedScopes := make([]Scope, 0, len(scopes))
		for _, sc := range scopes {
			if _, err := requireEncounter(snap, patientID, sc.EncounterID); err != nil {
				return err
			}
			if seenScopes[sc] {
				continue
			}
			seenScopes[sc] = true
			cleanedScopes = append(cleanedScopes, sc)
		}
		sort.Slice(cleanedScopes, func(i, j int) bool {
			if cleanedScopes[i].EncounterID != cleanedScopes[j].EncounterID {
				return cleanedScopes[i].EncounterID < cleanedScopes[j].EncounterID
			}
			return cleanedScopes[i].Category < cleanedScopes[j].Category
		})

		// 限定范围：逐条校验记录存在、属于患者、已生效、与声明的就诊+类别一致。
		// 重复选择同一记录只算一次。任一不合法即拒绝整条授权。
		seenRecords := map[ID]bool{}
		cleanedSelected := make([]SelectedRecord, 0, len(selected))
		for _, sr := range selected {
			r := snap.Records[sr.RecordID]
			if r == nil {
				return fmt.Errorf("%w: record %q", ErrNotFound, sr.RecordID)
			}
			if r.PatientID != patientID {
				return fmt.Errorf("%w: record %q belongs to patient %q, not %q",
					ErrMismatchedPatient, sr.RecordID, r.PatientID, patientID)
			}
			if r.CurrentVersionID == "" {
				return fmt.Errorf("%w: record %q has no effective version; activate its draft first",
					ErrInvalidArgument, sr.RecordID)
			}
			if r.EncounterID != sr.EncounterID || r.Category != sr.Category {
				return fmt.Errorf("%w: record %q (encounter %q, category %q) does not match declared encounter %q, category %q",
					ErrInvalidArgument, sr.RecordID, r.EncounterID, r.Category, sr.EncounterID, sr.Category)
			}
			if seenRecords[sr.RecordID] {
				continue
			}
			seenRecords[sr.RecordID] = true
			cleanedSelected = append(cleanedSelected, sr)
		}
		sort.Slice(cleanedSelected, func(i, j int) bool {
			return cleanedSelected[i].RecordID < cleanedSelected[j].RecordID
		})

		now := s.now()
		a := &Authorization{
			ID:              newID("auth"),
			PatientID:       patientID,
			ReceiverID:      receiverID,
			Scopes:          cleanedScopes,
			SelectedRecords: cleanedSelected,
			StartsAt:        startsAt.UTC(),
			ExpiresAt:       expiresAt.UTC(),
			CreatedAt:       now,
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
// 返回在“本次读取时间”被有效授权覆盖的记录的当前生效版本合集：
// 整类范围覆盖该就诊+类别下的全部已生效记录；限定范围只覆盖明确选中的记录。
// 多条有效授权的覆盖范围取并集，每条记录只出现一次，按记录标识稳定排序。
// 授权必须属于该接收方与该患者、已开始、未到期且未撤回；患者档案必须未停用。
// 看不到草稿、旧版本或更正原因；授权范围内后来生效或更正后的记录会自然包含
// （读取的是当前版本），但限定范围不会自动扩大到新增记录或新增就诊。
//
// 若不存在任何覆盖该就诊+类别的有效授权，或所有相关授权都尚未开始、已到期
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
		// 先看患者是否停用。
		patientDeactivated := false
		if p := snap.Patients[patientID]; p != nil {
			patientDeactivated = p.Deactivated
		}

		// 收集所有有效授权覆盖的记录标识（并集，每条只出现一次）。
		// 不提前区分“对象不存在/属于他人”与“无授权”，统一返回拒绝，
		// 从而不向接收方泄露患者或就诊是否存在。
		coveredIDs := map[ID]bool{}
		anyCovered := false
		for _, a := range snap.Authorizations {
			if a.PatientID != patientID || a.ReceiverID != actor.ID {
				continue
			}
			if !a.ActiveAt(now) {
				continue
			}
			// 整类范围：覆盖该就诊+类别下的全部已生效记录。
			if authorizationCovers(a, encounterID, category) {
				anyCovered = true
				for _, r := range snap.Records {
					if r.EncounterID != encounterID || r.Category != category || r.PatientID != patientID {
						continue
					}
					if r.CurrentVersionID != "" {
						coveredIDs[r.ID] = true
					}
				}
			}
			// 限定范围：只覆盖明确选中的记录。
			for _, sr := range a.SelectedRecords {
				if sr.EncounterID != encounterID || sr.Category != category {
					continue
				}
				anyCovered = true
				r := snap.Records[sr.RecordID]
				if r != nil && r.PatientID == patientID && r.CurrentVersionID != "" {
					coveredIDs[r.ID] = true
				}
			}
		}

		if !anyCovered || patientDeactivated {
			// 无授权、未开始、已到期、已撤回或档案停用：明确拒绝，
			// 结果中不含任何受保护内容。
			return ErrAccessDenied
		}

		// 能被授权覆盖的就诊必然属于该患者（Grant/GrantSelected 时已校验），
		// 此处再确认一次以防任何不一致状态。
		e := snap.Encounters[encounterID]
		if e == nil || e.PatientID != patientID {
			return ErrAccessDenied
		}

		out.EncounterID = encounterID
		out.Category = category
		var recs []EffectiveRecord
		for rid := range coveredIDs {
			r := snap.Records[rid]
			if r == nil {
				continue
			}
			if eff, ok := currentEffective(snap, r); ok {
				recs = append(recs, eff)
			}
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID })
		out.Records = recs
		return nil
	})
	return out, err
}

// authorizationCovers 报告授权是否包含覆盖该就诊+类别的整类范围。
// 限定范围不由此函数判断（限定范围只覆盖明确选中的记录，不覆盖整类）。
func authorizationCovers(a *Authorization, encounterID ID, category string) bool {
	for _, sc := range a.Scopes {
		if sc.EncounterID == encounterID && sc.Category == category {
			return true
		}
	}
	return false
}

// authorizationCoversRecord 报告授权是否覆盖指定记录：
// 整类范围覆盖该就诊+类别下的全部记录；限定范围只覆盖明确选中的记录。
// 用于 CreateExchange 的逐记录校验——每条记录必须被绑定的那一条授权覆盖，
// 不能借用同一接收方的其他授权补足。
func authorizationCoversRecord(a *Authorization, recordID ID, encounterID, category string) bool {
	for _, sc := range a.Scopes {
		if sc.EncounterID == encounterID && sc.Category == category {
			return true
		}
	}
	for _, sr := range a.SelectedRecords {
		if sr.RecordID == recordID {
			return true
		}
	}
	return false
}
