package clinical

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PackageDelivery 是指定接收方取包时得到的视图：交换标识、当前状态、
// 创建时间、包内容与摘要，不含内部使用者标识等包外信息。
type PackageDelivery struct {
	ExchangeID ID
	Status     string
	Digest     string
	CreatedAt  time.Time
	Package    Package
}

// ReceiptConfirmation 是接收方登记回执后得到的确认。
// 只含交换标识、当前状态、回执结果与登记时间，不含任何受保护内容；
// 授权失效或患者停用后登记回执也只返回该确认。
type ReceiptConfirmation struct {
	ExchangeID   ID
	Status       string
	Outcome      string
	RegisteredAt time.Time
}

// CreateExchange 由内部使用者把一条当前有效授权覆盖的已生效记录集合打包给
// 指定接收方。
//
// 参数：患者、接收方、绑定授权、记录标识集合与非空请求号。记录必须属于该
// 患者且已经生效；每条记录都必须被绑定的那一条授权覆盖——整类范围按记录的
// 就诊+类别覆盖，限定范围只覆盖明确选中的记录，不能借用同一接收方的其他
// 授权补足。授权必须属于该患者与接收方且在当前时间有效。空集合、空白请求号、
// 不存在或跨患者的引用、夹带草稿、记录不被绑定授权覆盖、授权不符一律拒绝，
// 不留下交换或审计，也不占用请求号。患者停用后不能新建交换。
//
// 成功后包内容固化为创建时各记录的当前生效版本（重复记录标识合并），并计算
// 稳定摘要，状态为待回执；交换与审计事件在同一次原子落盘中保留。此后记录被
// 更正或有新版本、新增记录都不会改写已创建的包。
//
// 请求号按发起的内部使用者区分：同一使用者以相同请求号携带相同患者、接收方、
// 授权与记录集合（顺序不限）重试时，返回原交换及其现有状态，不重新取内容、
// 不新增审计；其他参数变化返回 ErrConflict。首次失败不占用请求号。
func (s *Store) CreateExchange(actor Actor, patientID ID, receiverID, authorizationID ID, recordIDs []ID, requestID string) (Exchange, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Exchange{}, ErrAccessDenied
	}
	if patientID == "" {
		return Exchange{}, fmt.Errorf("%w: patient id is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(receiverID) == "" {
		return Exchange{}, fmt.Errorf("%w: receiver id is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(authorizationID) == "" {
		return Exchange{}, fmt.Errorf("%w: authorization id is required", ErrInvalidArgument)
	}
	if strings.TrimSpace(requestID) == "" {
		return Exchange{}, fmt.Errorf("%w: request id is required", ErrInvalidArgument)
	}
	if len(recordIDs) == 0 {
		return Exchange{}, fmt.Errorf("%w: record id set must not be empty", ErrInvalidArgument)
	}
	for _, rid := range recordIDs {
		if strings.TrimSpace(rid) == "" {
			return Exchange{}, fmt.Errorf("%w: record id must not be blank", ErrInvalidArgument)
		}
	}
	wanted := uniqueSortedIDs(recordIDs)

	var result Exchange
	err := s.mutate(func(snap *snapshot) error {
		// 幂等检索优先：同一使用者的同一请求号无论当前患者/授权状态如何，
		// 都只返回原结果（重试不是新建）。
		if existing := findExchangeByRequest(snap, actor.ID, requestID); existing != nil {
			if !sameExchangeRequest(existing, patientID, receiverID, authorizationID, wanted) {
				return fmt.Errorf("%w: request id %q already used with different parameters", ErrConflict, requestID)
			}
			result = *existing
			if existing.Receipt != nil {
				r := *existing.Receipt
				result.Receipt = &r
			}
			result.Package.Records = append([]PackagedRecord(nil), existing.Package.Records...)
			return nil
		}

		// 首次创建：患者必须存在且未停用。
		if _, err := requireActivePatient(snap, patientID); err != nil {
			return err
		}

		// 绑定授权必须存在、属于该患者与接收方、当前有效。
		a := snap.Authorizations[authorizationID]
		if a == nil {
			return fmt.Errorf("%w: authorization %q", ErrNotFound, authorizationID)
		}
		if a.PatientID != patientID {
			return fmt.Errorf("%w: authorization %q belongs to patient %q, not %q",
				ErrMismatchedPatient, authorizationID, a.PatientID, patientID)
		}
		if a.ReceiverID != receiverID {
			return fmt.Errorf("%w: authorization %q is bound to receiver %q, not %q",
				ErrInvalidArgument, authorizationID, a.ReceiverID, receiverID)
		}
		now := s.now()
		if !a.ActiveAt(now) {
			// 未开始、已到期或已撤回：没有覆盖此次打包的有效授权。
			return fmt.Errorf("%w: authorization %q is not active", ErrAccessDenied, authorizationID)
		}

		// 逐记录固化当前生效版本；重复标识已合并，按记录标识排序保证确定性。
		pkg := Package{PatientID: patientID, ReceiverID: receiverID}
		for _, rid := range wanted {
			r := snap.Records[rid]
			if r == nil {
				return fmt.Errorf("%w: record %q", ErrNotFound, rid)
			}
			if r.PatientID != patientID {
				return fmt.Errorf("%w: record %q belongs to patient %q, not %q",
					ErrMismatchedPatient, rid, r.PatientID, patientID)
			}
			if r.CurrentVersionID == "" {
				return fmt.Errorf("%w: record %q has no effective version; activate its draft first",
					ErrInvalidArgument, rid)
			}
			v := snap.Versions[r.CurrentVersionID]
			if v == nil {
				return fmt.Errorf("%w: current version %q of record %q", ErrNotFound, r.CurrentVersionID, rid)
			}
			// 每条记录都必须被绑定授权自身覆盖：整类范围或限定范围。
			// 同一接收方的其他授权不能补足；夹带一条未覆盖记录即整包拒绝。
			if !authorizationCoversRecord(a, r) {
				return fmt.Errorf("%w: record %q (encounter %q, category %q) is not covered by authorization %q",
					ErrAccessDenied, rid, r.EncounterID, r.Category, authorizationID)
			}
			pkg.Records = append(pkg.Records, PackagedRecord{
				EncounterID: r.EncounterID,
				Category:    r.Category,
				RecordID:    r.ID,
				VersionID:   v.ID,
				Version:     v.Number,
				EffectiveAt: v.CreatedAt,
				Content:     v.Content,
			})
		}

		x := &Exchange{
			ID:              newID("exch"),
			PatientID:       patientID,
			ReceiverID:      receiverID,
			AuthorizationID: authorizationID,
			RequestID:       requestID,
			CreatorID:       actor.ID,
			Status:          ExchangePending,
			Digest:          packageDigest(pkg),
			Package:         pkg,
			CreatedAt:       now,
		}
		snap.Exchanges[x.ID] = x
		s.addAudit(snap, patientID, actor, ActionExchanged, "exchange", x.ID, now)
		result = *x
		result.Package.Records = append([]PackagedRecord(nil), x.Package.Records...)
		return nil
	})
	return result, err
}

// FetchPackage 供指定接收方凭交换标识取包。
//
// 每次取包都重新检查绑定授权与患者状态：授权必须仍是创建时绑定的那一条且
// 已开始、未到期、未撤回，患者档案必须未停用；其他有效授权不能替代绑定授权。
// 是否允许取包按“本次核对时间”判断：请求发出后若因等待同一存储上的其他操作
// 而排队，以等待结束、真正开始核对本次取包权限的时刻为准（进入临界区后才
// 取样，与接收方 Read 的时间含义一致），提前发出的请求不会延长授权有效期；
// 时间窗仍是半开区间 [开始, 截止)，恰好到达截止时刻即失效。授权未开始、
// 到期、撤回或患者停用后返回 ErrAccessDenied，交付结果为空，不返回包内容、
// 摘要、记录标识或交换状态。
// 其他接收方、内部使用者以及不存在的交换统一拒绝，不泄露交换是否存在。
//
// 成功时返回交换创建时固化的包及其摘要与当前状态；此后记录被更正也不会
// 重新打包，包内版本与内容仍以创建时保存的为准。
func (s *Store) FetchPackage(actor Actor, exchangeID ID) (PackageDelivery, error) {
	if !actor.valid() || actor.Kind != "receiver" {
		return PackageDelivery{}, ErrAccessDenied
	}
	if strings.TrimSpace(exchangeID) == "" {
		return PackageDelivery{}, fmt.Errorf("%w: exchange id is required", ErrInvalidArgument)
	}

	var out PackageDelivery
	err := s.view(func(snap *snapshot) error {
		// 核对时刻在进入临界区、真正开始核对本次取包权限时才取样：请求发出后
		// 若因等待其他操作而排队，等待时间不能延长绑定授权的有效判断——
		// 是否可取一律按这同一时刻判断，与 Read 的时间含义一致。
		now := s.now()
		x := snap.Exchanges[exchangeID]
		if x == nil || x.ReceiverID != actor.ID {
			// 不存在或属于其他接收方：统一拒绝，不泄露存在性。
			return ErrAccessDenied
		}
		p := snap.Patients[x.PatientID]
		if p == nil || p.Deactivated {
			return ErrAccessDenied
		}
		// 必须仍是绑定的那条授权（同一患者与接收方）；其他有效授权不能替代。
		a := snap.Authorizations[x.AuthorizationID]
		if a == nil || a.PatientID != x.PatientID || a.ReceiverID != actor.ID || !a.ActiveAt(now) {
			return ErrAccessDenied
		}
		out = PackageDelivery{
			ExchangeID: x.ID,
			Status:     x.Status,
			Digest:     x.Digest,
			CreatedAt:  x.CreatedAt,
			Package:    clonePackage(x.Package),
		}
		return nil
	})
	return out, err
}

// SubmitReceipt 供指定接收方对一份包登记回执：提供交换标识、取包时得到的
// 包摘要、接受或拒绝的结果及原因。
//
// 非法结果、拒绝时原因为空白返回 ErrInvalidArgument。只有指定接收方且摘要
// 吻合，才能把交换从待回执变为已接受或已拒绝；摘要不符返回 ErrConflict，
// 状态不变。相同回执（结果与原因均相同）重交返回原确认，不新增审计；结果
// 或原因改变返回 ErrConflict。其他身份不能代交，不存在的交换统一拒绝。
//
// 授权失效或患者停用后仍允许登记此前包的回执，但响应只返回确认状态，
// 不提供任何受保护内容。
func (s *Store) SubmitReceipt(actor Actor, exchangeID ID, digest, outcome, reason string) (ReceiptConfirmation, error) {
	if !actor.valid() || actor.Kind != "receiver" {
		return ReceiptConfirmation{}, ErrAccessDenied
	}
	if strings.TrimSpace(exchangeID) == "" {
		return ReceiptConfirmation{}, fmt.Errorf("%w: exchange id is required", ErrInvalidArgument)
	}
	if outcome != ReceiptAccepted && outcome != ReceiptRejected {
		return ReceiptConfirmation{}, fmt.Errorf("%w: outcome must be %q or %q", ErrInvalidArgument, ReceiptAccepted, ReceiptRejected)
	}
	if outcome == ReceiptRejected && strings.TrimSpace(reason) == "" {
		return ReceiptConfirmation{}, fmt.Errorf("%w: rejection reason is required", ErrInvalidArgument)
	}
	if outcome == ReceiptAccepted {
		// 接受不携带原因；统一归一化，保证重复提交判定稳定。
		reason = ""
	}

	var conf ReceiptConfirmation
	err := s.mutate(func(snap *snapshot) error {
		x := snap.Exchanges[exchangeID]
		if x == nil || x.ReceiverID != actor.ID {
			// 不存在或属于其他接收方/其他身份：统一拒绝。
			return ErrAccessDenied
		}
		if digest != x.Digest {
			return fmt.Errorf("%w: package digest does not match exchange %q", ErrConflict, exchangeID)
		}
		if x.Receipt != nil {
			if x.Receipt.Outcome != outcome || x.Receipt.Reason != reason {
				return fmt.Errorf("%w: exchange %q already has a different receipt", ErrConflict, exchangeID)
			}
			// 相同回执重交：返回原确认，不新增审计、不改状态。
			conf = ReceiptConfirmation{
				ExchangeID:   x.ID,
				Status:       x.Status,
				Outcome:      x.Receipt.Outcome,
				RegisteredAt: x.Receipt.RegisteredAt,
			}
			return nil
		}

		now := s.now()
		x.Receipt = &Receipt{Outcome: outcome, Reason: reason, RegisteredAt: now}
		if outcome == ReceiptAccepted {
			x.Status = ExchangeAccepted
		} else {
			x.Status = ExchangeRejected
		}
		s.addAudit(snap, x.PatientID, actor, ActionReceipted, "exchange", x.ID, now)
		conf = ReceiptConfirmation{
			ExchangeID:   x.ID,
			Status:       x.Status,
			Outcome:      outcome,
			RegisteredAt: now,
		}
		return nil
	})
	return conf, err
}

// GetExchange 供内部使用者按患者查看原包（含固化内容与摘要）及回执。
// 患者停用后仍可查看。交换不存在返回 ErrNotFound，属于其他患者返回
// ErrMismatchedPatient。
func (s *Store) GetExchange(actor Actor, patientID, exchangeID ID) (Exchange, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Exchange{}, ErrAccessDenied
	}
	var result Exchange
	err := s.view(func(snap *snapshot) error {
		x, err := requireExchange(snap, patientID, exchangeID)
		if err != nil {
			return err
		}
		result = cloneExchangeValue(x)
		return nil
	})
	return result, err
}

// ListExchanges 供内部使用者按患者查看其全部交换（原包与回执），
// 按创建时间旧到新排序；时间戳相同时按交换标识排序。患者停用后仍可查看。
func (s *Store) ListExchanges(actor Actor, patientID ID) ([]Exchange, error) {
	if !actor.valid() || !actor.IsInternal() {
		return nil, ErrAccessDenied
	}
	var result []Exchange
	err := s.view(func(snap *snapshot) error {
		if _, err := requirePatient(snap, patientID); err != nil {
			return err
		}
		for _, x := range snap.Exchanges {
			if x.PatientID == patientID {
				result = append(result, cloneExchangeValue(x))
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

func requireExchange(snap *snapshot, patientID, exchangeID ID) (*Exchange, error) {
	x := snap.Exchanges[exchangeID]
	if x == nil {
		return nil, fmt.Errorf("%w: exchange %q", ErrNotFound, exchangeID)
	}
	if x.PatientID != patientID {
		return nil, fmt.Errorf("%w: exchange %q belongs to patient %q, not %q",
			ErrMismatchedPatient, exchangeID, x.PatientID, patientID)
	}
	return x, nil
}

func findExchangeByRequest(snap *snapshot, creatorID, requestID string) *Exchange {
	for _, x := range snap.Exchanges {
		if x.CreatorID == creatorID && x.RequestID == requestID {
			return x
		}
	}
	return nil
}

// sameExchangeRequest 判断重试参数是否与既有交换完全一致（记录集合顺序无关）。
func sameExchangeRequest(x *Exchange, patientID ID, receiverID, authorizationID ID, wantedRecordIDs []ID) bool {
	if x.PatientID != patientID || x.ReceiverID != receiverID || x.AuthorizationID != authorizationID {
		return false
	}
	if len(x.Package.Records) != len(wantedRecordIDs) {
		return false
	}
	// 包内记录按记录标识排序保存，wantedRecordIDs 同样已去重排序。
	for i, pr := range x.Package.Records {
		if pr.RecordID != wantedRecordIDs[i] {
			return false
		}
	}
	return true
}

func uniqueSortedIDs(ids []ID) []ID {
	seen := make(map[ID]struct{}, len(ids))
	out := make([]ID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func clonePackage(p Package) Package {
	p.Records = append([]PackagedRecord(nil), p.Records...)
	return p
}

func cloneExchangeValue(x *Exchange) Exchange {
	y := *x
	y.Package = clonePackage(x.Package)
	if x.Receipt != nil {
		r := *x.Receipt
		y.Receipt = &r
	}
	return y
}

// canonicalRecord 固定摘要中每个字段的序列化顺序。
type canonicalRecord struct {
	EncounterID ID     `json:"encounter_id"`
	Category    string `json:"category"`
	RecordID    ID     `json:"record_id"`
	VersionID   ID     `json:"version_id"`
	Version     int    `json:"version"`
	EffectiveAt string `json:"effective_at"`
	Content     string `json:"content"`
}

type canonicalPackage struct {
	PatientID  ID                `json:"patient_id"`
	ReceiverID string            `json:"receiver_id"`
	Records    []canonicalRecord `json:"records"`
}

// packageDigest 计算包内容的稳定摘要：记录先按标识排序（创建时已排序），
// 时间统一为 UTC RFC3339Nano，字段顺序固定，因此与入参顺序无关、可重算核对。
func packageDigest(p Package) string {
	cp := canonicalPackage{
		PatientID:  p.PatientID,
		ReceiverID: p.ReceiverID,
		Records:    make([]canonicalRecord, 0, len(p.Records)),
	}
	recs := append([]PackagedRecord(nil), p.Records...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID })
	for _, r := range recs {
		cp.Records = append(cp.Records, canonicalRecord{
			EncounterID: r.EncounterID,
			Category:    r.Category,
			RecordID:    r.RecordID,
			VersionID:   r.VersionID,
			Version:     r.Version,
			EffectiveAt: r.EffectiveAt.UTC().Format(time.RFC3339Nano),
			Content:     r.Content,
		})
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		// 参与序列化的都是字符串/整数等基础类型，不应失败。
		panic(fmt.Errorf("clinical: cannot marshal package digest: %w", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
