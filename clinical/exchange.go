package clinical

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CreateExchange 由内部使用者把一批已生效记录打包给指定接收方。
//
// 创建条件（任一不满足都拒绝，且不留下任何交换或审计）：
//   - 患者存在且未停用；
//   - 指定的授权存在、属于该患者与该接收方，且在当前时刻有效
//     （已开始、未到期、未撤回）；
//   - 记录集合非空；每条记录都存在、属于该患者、已经生效（夹带草稿一律拒绝），
//     且其就诊与类别均被这条绑定授权覆盖。
//
// 包内容在创建时固化为各记录的当前生效版本：此后记录更正或新增都不会改写包，
// 现有 Read 仍返回当前生效版本。包只含患者与接收方标识及各记录的就诊、类别、
// 记录与版本标识、版本号、生效时间、完整内容，不含姓名、草稿、其他历史版本
// 或更正原因。重复记录标识合并。
//
// 请求号按内部使用者区分：同一使用者以相同请求号、患者、接收方、授权和记录集合
// （顺序无关）重试，返回原交换及其现有状态，不重新取内容也不新增审计；
// 任一其他参数变化返回 ErrConflict。首次失败不占用请求号。
// 交换创建与审计事件在同一次原子落盘中保留。
func (s *Store) CreateExchange(actor Actor, patientID, receiverID, authorizationID ID, recordIDs []ID, requestKey string) (Exchange, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Exchange{}, ErrAccessDenied
	}
	if patientID == "" || receiverID == "" || authorizationID == "" {
		return Exchange{}, fmt.Errorf("%w: patient, receiver and authorization ids are required", ErrInvalidArgument)
	}
	if strings.TrimSpace(requestKey) == "" {
		return Exchange{}, fmt.Errorf("%w: request number is required", ErrInvalidArgument)
	}
	if len(recordIDs) == 0 {
		return Exchange{}, fmt.Errorf("%w: record set must not be empty", ErrInvalidArgument)
	}

	var result Exchange
	err := s.mutate(func(snap *snapshot) error {
		// 请求号幂等：同一内部使用者的同一请求号只产生一份交换。
		// 这是重试路径：参数吻合即返回原交换（不重新校验患者当前状态、
		// 不取内容、不新增审计）；任一参数变化返回 ErrConflict。
		if existing := findExchangeByRequest(snap, actor.ID, requestKey); existing != nil {
			if existing.PatientID != patientID || existing.ReceiverID != receiverID ||
				existing.AuthorizationID != authorizationID || !sameRecordSet(existing.Package, recordIDs) {
				return ErrConflict
			}
			result = *existing
			return nil
		}

		if _, err := requireActivePatient(snap, patientID); err != nil {
			return err
		}

		auth := snap.Authorizations[authorizationID]
		if auth == nil {
			return fmt.Errorf("%w: authorization %q", ErrNotFound, authorizationID)
		}
		if auth.PatientID != patientID {
			return fmt.Errorf("%w: authorization %q belongs to patient %q, not %q",
				ErrMismatchedPatient, authorizationID, auth.PatientID, patientID)
		}
		if auth.ReceiverID != receiverID {
			return fmt.Errorf("%w: authorization %q is granted to receiver %q, not %q",
				ErrInvalidArgument, authorizationID, auth.ReceiverID, receiverID)
		}
		if !auth.ActiveAt(s.now()) {
			return fmt.Errorf("%w: authorization %q is not currently effective", ErrAccessDenied, authorizationID)
		}

		pkg := ExchangePackage{PatientID: patientID, ReceiverID: receiverID}
		seen := make(map[ID]bool, len(recordIDs))
		for _, rid := range recordIDs {
			if seen[rid] {
				continue // 重复记录标识合并
			}
			seen[rid] = true

			r := snap.Records[rid]
			if r == nil {
				return fmt.Errorf("%w: record %q", ErrNotFound, rid)
			}
			if r.PatientID != patientID {
				return fmt.Errorf("%w: record %q belongs to patient %q, not %q",
					ErrMismatchedPatient, rid, r.PatientID, patientID)
			}
			if r.CurrentVersionID == "" {
				// 草稿或从未生效的记录一律拒绝。
				return fmt.Errorf("%w: record %q is not yet effective", ErrInvalidArgument, rid)
			}
			if !authorizationCovers(auth, r.EncounterID, r.Category) {
				return fmt.Errorf("%w: record %q (encounter %q, category %q) is not covered by authorization %q",
					ErrAccessDenied, rid, r.EncounterID, r.Category, authorizationID)
			}
			v := snap.Versions[r.CurrentVersionID]
			if v == nil {
				return fmt.Errorf("%w: current version %q", ErrNotFound, r.CurrentVersionID)
			}
			pkg.Records = append(pkg.Records, ExchangeRecord{
				EncounterID: r.EncounterID,
				Category:    r.Category,
				RecordID:    r.ID,
				VersionID:   v.ID,
				Version:     v.Number,
				EffectiveAt: v.CreatedAt,
				Content:     v.Content,
			})
		}
		sort.Slice(pkg.Records, func(i, j int) bool { return pkg.Records[i].RecordID < pkg.Records[j].RecordID })

		now := s.now()
		ex := &Exchange{
			ID:              newID("exc"),
			PatientID:       patientID,
			ReceiverID:      receiverID,
			AuthorizationID: authorizationID,
			RequesterID:     actor.ID,
			RequestKey:      requestKey,
			CreatedAt:       now,
			Status:          ExchangePending,
			Package:         pkg,
			Summary:         packageSummary(pkg),
		}
		snap.Exchanges[ex.ID] = ex
		s.addAudit(snap, patientID, actor, ActionExchangeCreated, "exchange", ex.ID, now)
		result = *ex
		return nil
	})
	return result, err
}

// FetchExchangePackage 供指定接收方取包。取包时重新检查绑定授权与患者状态：
// 授权未开始、到期、撤回或患者停用后返回 ErrAccessDenied，且不返回包内容或摘要；
// 其他有效授权不能替代绑定授权。其他接收方或不存在的交换统一拒绝，不泄露存在性。
func (s *Store) FetchExchangePackage(actor Actor, exchangeID ID) (ExchangePackage, string, error) {
	if !actor.valid() || actor.Kind != "receiver" {
		return ExchangePackage{}, "", ErrAccessDenied
	}
	if exchangeID == "" {
		return ExchangePackage{}, "", fmt.Errorf("%w: exchange id is required", ErrInvalidArgument)
	}

	var pkg ExchangePackage
	var summary string
	err := s.view(func(snap *snapshot) error {
		ex := snap.Exchanges[exchangeID]
		if ex == nil || ex.ReceiverID != actor.ID {
			// 不存在与非指定接收方统一拒绝。
			return ErrAccessDenied
		}
		p := snap.Patients[ex.PatientID]
		if p == nil || p.Deactivated {
			return ErrAccessDenied
		}
		auth := snap.Authorizations[ex.AuthorizationID]
		if auth == nil || auth.ReceiverID != actor.ID || !auth.ActiveAt(s.now()) {
			// 绑定授权失效：其他有效授权不能替代。
			return ErrAccessDenied
		}
		pkg = ex.Package
		summary = ex.Summary
		return nil
	})
	return pkg, summary, err
}

// SubmitReceipt 供指定接收方提交回执：提供交换标识、包摘要、接受/拒绝结果及原因。
//
// 只有指定接收方且摘要吻合，才能把交换从待回执变为已接受或已拒绝；摘要不符返回
// ErrConflict。相同回执重交返回原结果（不新增审计）；结果或原因改变返回 ErrConflict。
// 授权失效或患者停用后仍允许登记此前包的回执，但响应只返回确认状态，不提供受保护内容。
func (s *Store) SubmitReceipt(actor Actor, exchangeID ID, summary, result, reason string) (ReceiptConfirmation, error) {
	if !actor.valid() || actor.Kind != "receiver" {
		return ReceiptConfirmation{}, ErrAccessDenied
	}
	if exchangeID == "" {
		return ReceiptConfirmation{}, fmt.Errorf("%w: exchange id is required", ErrInvalidArgument)
	}
	if result != ExchangeAccepted && result != ExchangeRejected {
		return ReceiptConfirmation{}, fmt.Errorf("%w: result must be %q or %q",
			ErrInvalidArgument, ExchangeAccepted, ExchangeRejected)
	}
	if result == ExchangeRejected && strings.TrimSpace(reason) == "" {
		return ReceiptConfirmation{}, fmt.Errorf("%w: rejection reason is required", ErrInvalidArgument)
	}

	var confirm ReceiptConfirmation
	err := s.mutate(func(snap *snapshot) error {
		ex := snap.Exchanges[exchangeID]
		if ex == nil || ex.ReceiverID != actor.ID {
			return ErrAccessDenied
		}
		if summary != ex.Summary {
			return fmt.Errorf("%w: package summary does not match exchange %q", ErrConflict, exchangeID)
		}

		if ex.Status == ExchangePending {
			now := s.now()
			ex.Status = result
			ex.ReceiptResult = result
			ex.ReceiptReason = reason
			ex.ReceiptActorID = actor.ID
			ex.ReceiptAt = &now
			s.addAudit(snap, ex.PatientID, actor, ActionExchangeReceipt, "exchange", ex.ID, now)
		} else if ex.ReceiptResult != result || ex.ReceiptReason != reason {
			// 已回执：结果或原因改变构成冲突；完全相同则幂等返回原结果。
			return ErrConflict
		}

		confirm = ReceiptConfirmation{
			ExchangeID: ex.ID,
			Status:     ex.Status,
			Result:     ex.ReceiptResult,
			Reason:     ex.ReceiptReason,
			ActorID:    ex.ReceiptActorID,
			At:         *ex.ReceiptAt,
		}
		return nil
	})
	return confirm, err
}

// ListExchanges 供内部使用者按患者查看原包及回执，患者停用后仍可查看。
// 交换按创建时间先后排列。
func (s *Store) ListExchanges(actor Actor, patientID ID) ([]Exchange, error) {
	if !actor.valid() || !actor.IsInternal() {
		return nil, ErrAccessDenied
	}
	if patientID == "" {
		return nil, fmt.Errorf("%w: patient id is required", ErrInvalidArgument)
	}

	var result []Exchange
	err := s.view(func(snap *snapshot) error {
		if _, err := requirePatient(snap, patientID); err != nil {
			return err
		}
		for _, ex := range snap.Exchanges {
			if ex.PatientID == patientID {
				result = append(result, *ex)
			}
		}
		sort.Slice(result, func(i, j int) bool {
			if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
				return result[i].CreatedAt.Before(result[j].CreatedAt)
			}
			return result[i].ID < result[j].ID
		})
		return nil
	})
	return result, err
}

func findExchangeByRequest(snap *snapshot, requesterID, requestKey string) *Exchange {
	for _, ex := range snap.Exchanges {
		if ex.RequesterID == requesterID && ex.RequestKey == requestKey {
			return ex
		}
	}
	return nil
}

// sameRecordSet 判断包内记录集合与给定记录标识集合是否相同（顺序无关、重复合并）。
func sameRecordSet(pkg ExchangePackage, recordIDs []ID) bool {
	want := make(map[ID]bool, len(recordIDs))
	for _, id := range recordIDs {
		want[id] = true
	}
	if len(want) != len(pkg.Records) {
		return false
	}
	for _, rec := range pkg.Records {
		if !want[rec.RecordID] {
			return false
		}
	}
	return true
}

// packageSummary 计算包内容的稳定摘要：对包的规范化 JSON 取 SHA-256。
// 包内记录已按记录标识排序，因此摘要只取决于内容本身。
func packageSummary(pkg ExchangePackage) string {
	raw, err := json.Marshal(pkg)
	if err != nil {
		// 包内容均为可 JSON 化的基础类型；序列化失败在正常运行环境中不应发生。
		panic(fmt.Errorf("clinical: cannot summarize package: %w", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func cloneExchangePackage(in ExchangePackage) ExchangePackage {
	out := ExchangePackage{PatientID: in.PatientID, ReceiverID: in.ReceiverID}
	out.Records = append([]ExchangeRecord(nil), in.Records...)
	return out
}
