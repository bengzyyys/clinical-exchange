package clinical

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Grant 由内部使用者为一名接收方建立针对某患者的整类读取授权。
//
// 这是 [Store.GrantSelective] 的整类形式：范围由明确的“就诊 + 类别
// （诊断/医嘱）”组合构成，覆盖各就诊+类别下的全部已生效记录（含授权
// 建立后才生效的记录）。不允许空范围，每个就诊都必须属于该患者
// （跨患者就诊会被拒绝），类别必须合法。时间窗为半开区间
// [startsAt, expiresAt)：开始时刻即生效，到截止时刻立即失效；
// 开始不早于截止时间（相等也算）一律拒绝。患者档案停用后不能新建授权。
// 授权与审计事件在同一次原子落盘中保留。
func (s *Store) Grant(actor Actor, patientID ID, receiverID string, scopes []Scope, startsAt, expiresAt time.Time) (Authorization, error) {
	return s.GrantSelective(actor, patientID, receiverID, scopes, nil, startsAt, expiresAt)
}

// GrantSelective 由内部使用者为一名接收方建立针对某患者的读取授权，
// 整类范围（scopes）与限定记录范围（selections）可以并存，同一条授权
// 可以包含多个就诊、类别。
//
// scopes 的每项按整类授权：覆盖该就诊+类别下全部已生效记录，并自然
// 包含授权后新增生效的记录；selections 的每项明确选出一条已生效记录，
// 被选记录更正后授权覆盖它的当前版本，但同一就诊同一类别的其他记录、
// 后来新增并生效的记录都不会自动进入限定范围。重复选择同一记录只算一次。
//
// scopes 与 selections 不能同时为空：空选择不代表整类授权。scope 中的
// 就诊必须存在且属于该患者，类别必须合法。每条 selection 都必须显式
// 声明记录所属的就诊与类别，且：记录不存在返回 ErrNotFound；记录属于
// 其他患者返回 ErrMismatchedPatient；空选择、空白标识、选择了尚未生效
// 的草稿、记录与声明的就诊或类别不符返回 ErrInvalidArgument。任一选择
// 不合法就拒绝整条授权：不保存合法部分，也不新增审计。
//
// 时间窗、停用档案与审计规则与 [Store.Grant] 相同。
func (s *Store) GrantSelective(actor Actor, patientID ID, receiverID string, scopes []Scope, selections []RecordSelection, startsAt, expiresAt time.Time) (Authorization, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Authorization{}, ErrAccessDenied
	}
	if patientID == "" {
		return Authorization{}, fmt.Errorf("%w: patient id is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(receiverID) == "" {
		return Authorization{}, fmt.Errorf("%w: receiver id is required", ErrInvalidArgument)
	}
	if len(scopes) == 0 && len(selections) == 0 {
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
	for _, sel := range selections {
		if strings.TrimSpace(sel.RecordID) == "" {
			return Authorization{}, fmt.Errorf("%w: selection record id is required", ErrInvalidArgument)
		}
		if strings.TrimSpace(sel.EncounterID) == "" {
			return Authorization{}, fmt.Errorf("%w: selection encounter id is required", ErrInvalidArgument)
		}
		if !validCategory(sel.Category) {
			return Authorization{}, fmt.Errorf("%w: selection category must be %q or %q", ErrInvalidArgument, Diagnosis, Order)
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

		// 去重并逐项校验整类范围：引用的就诊必须存在且属于该患者。
		seenScope := map[Scope]bool{}
		cleanedScopes := make([]Scope, 0, len(scopes))
		for _, sc := range scopes {
			if _, err := requireEncounter(snap, patientID, sc.EncounterID); err != nil {
				return err
			}
			if seenScope[sc] {
				continue
			}
			seenScope[sc] = true
			cleanedScopes = append(cleanedScopes, sc)
		}
		sort.Slice(cleanedScopes, func(i, j int) bool {
			if cleanedScopes[i].EncounterID != cleanedScopes[j].EncounterID {
				return cleanedScopes[i].EncounterID < cleanedScopes[j].EncounterID
			}
			return cleanedScopes[i].Category < cleanedScopes[j].Category
		})

		// 逐项校验限定范围：就诊必须属于该患者；记录必须存在、属于该患者、
		// 已生效，且实际就诊与类别与声明一致。任一不合法即整体拒绝。
		seenRecord := map[ID]bool{}
		cleanedSelections := make([]RecordSelection, 0, len(selections))
		for _, sel := range selections {
			if _, err := requireEncounter(snap, patientID, sel.EncounterID); err != nil {
				return err
			}
			r := snap.Records[sel.RecordID]
			if r == nil {
				return fmt.Errorf("%w: record %q", ErrNotFound, sel.RecordID)
			}
			if r.PatientID != patientID {
				return fmt.Errorf("%w: record %q belongs to patient %q, not %q",
					ErrMismatchedPatient, sel.RecordID, r.PatientID, patientID)
			}
			if r.CurrentVersionID == "" {
				return fmt.Errorf("%w: record %q has no effective version; activate its draft first",
					ErrInvalidArgument, sel.RecordID)
			}
			if r.EncounterID != sel.EncounterID || r.Category != sel.Category {
				return fmt.Errorf("%w: record %q belongs to encounter %q category %q, not declared encounter %q category %q",
					ErrInvalidArgument, sel.RecordID, r.EncounterID, r.Category, sel.EncounterID, sel.Category)
			}
			if seenRecord[sel.RecordID] {
				// 重复选择同一记录只算一次。
				continue
			}
			seenRecord[sel.RecordID] = true
			cleanedSelections = append(cleanedSelections, sel)
		}
		sort.Slice(cleanedSelections, func(i, j int) bool {
			return cleanedSelections[i].RecordID < cleanedSelections[j].RecordID
		})

		now := s.now()
		a := &Authorization{
			ID:         newID("auth"),
			PatientID:  patientID,
			ReceiverID: receiverID,
			Scopes:     cleanedScopes,
			Selections: cleanedSelections,
			StartsAt:   startsAt.UTC(),
			ExpiresAt:  expiresAt.UTC(),
			CreatedAt:  now,
		}
		snap.Authorizations[a.ID] = a
		s.addAudit(snap, patientID, actor, ActionGranted, "authorization", a.ID, now)
		result = cloneAuthorizationValue(a)
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
		result = cloneAuthorizationValue(a)
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
			result = append(result, cloneAuthorizationValue(a))
		}
		sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
		return nil
	})
	return result, err
}

// Read 供接收方按自己的身份读取某次就诊下某个类别（诊断/医嘱）的内容。
//
// 结果是该接收方在“本次读取时间”所有当前有效授权允许记录的合集。“本次读取
// 时间”指真正开始核对权限的时刻：请求发出后若因等待同一存储上的其他操作而
// 排队，以等待结束、开始核对时的同一时刻统一判断本次涉及的各条授权是否有效，
// 等待本身不会延长或推迟授权的有效期。每条记录
// 只出现一次并按记录标识稳定排序：授权必须属于该接收方与该患者、已开始、
// 未到期且未撤回；整类范围（Scope）允许该就诊+类别下的全部已生效记录
// （含授权后才生效者），限定范围（RecordSelection）仅允许明确选中的记录。
// 同一条记录被多条授权（含整类与限定重叠）允许也只出现一次；被选记录被
// 更正后读取其当前生效版本。看不到草稿、旧版本或更正原因。
//
// 若不存在任何覆盖所请求就诊+类别的有效授权（全部尚未开始、已到期或已
// 撤回），或患者档案已停用，返回 ErrAccessDenied：结果中不含任何未授权
// 记录的标识、数量或内容。撤回整类授权后，只剩其他有效授权明确允许的记录。
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

	var out ReadResult
	err := s.view(func(snap *snapshot) error {
		// 查阅时刻在进入临界区、真正开始核对权限时才取样：请求发出后若因
		// 等待其他操作而排队，等待时间不能延长或推迟授权的有效判断——
		// 本次读取涉及的所有授权统一按这同一时刻判断是否有效。
		now := s.now()
		// 合并该患者、该接收方所有当前有效授权的覆盖：整类范围与限定范围的
		// 解释与创建交换共用同一套判定（grantCoverage），不在这里另写一份。
		// 不提前区分“对象不存在/属于他人”与“无授权”，统一返回拒绝，从而不
		// 向接收方泄露患者或就诊是否存在。
		coverage := newGrantCoverage()
		patientDeactivated := false
		if p := snap.Patients[patientID]; p != nil {
			patientDeactivated = p.Deactivated
		}
		for _, a := range snap.Authorizations {
			if a.PatientID != patientID || a.ReceiverID != actor.ID {
				continue
			}
			if !a.ActiveAt(now) {
				// 未开始、已到期或已撤回的授权不参与覆盖。
				continue
			}
			coverage.add(a)
		}
		if patientDeactivated || !coverage.coversRequest(encounterID, category) {
			// 无授权、未开始、已到期、已撤回或档案停用：明确拒绝，
			// 结果中不含任何受保护内容。整类授权覆盖该就诊+类别但其中暂时
			// 没有生效记录时，coversRequest 仍为真，随后成功返回空集合，
			// 不会把“有授权无记录”误判成拒绝。
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
			// 整类范围覆盖全部（含授权后才生效者）；限定范围只覆盖明确选中
			// 的记录，同类其他记录与后来新增记录不被覆盖。多条授权、整类与
			// 限定重叠时同一条记录只出现一次。
			if !coverage.coversRecord(r) {
				continue
			}
			if eff, ok := currentEffective(snap, r); ok {
				recs = append(recs, eff)
			}
			// 仅有草稿、尚无生效版本的记录天然不出现；
			// 限定授权选中的记录在授权后也不会退回草稿状态。
		}
		sort.Slice(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID })
		out.Records = recs
		return nil
	})
	return out, err
}

// cloneAuthorizationValue 返回授权的深拷贝：整类范围、限定范围与撤回时间
// 都与库内保存的对象各自独立。返回给调用方的授权只是本次操作得到的信息；
// 调用方在本地修改它（改范围、换记录标识、改撤回时间）不能改变库内正式
// 保存的授权，从同一授权取得的多份结果也互不影响。
func cloneAuthorizationValue(a *Authorization) Authorization {
	out := *a
	out.Scopes = append([]Scope(nil), a.Scopes...)
	out.Selections = append([]RecordSelection(nil), a.Selections...)
	if a.RevokedAt != nil {
		t := *a.RevokedAt
		out.RevokedAt = &t
	}
	return out
}
