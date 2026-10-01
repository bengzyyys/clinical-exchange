package clinical

import (
	"errors"
	"time"
)

// 记录类别：诊断与医嘱。
const (
	Diagnosis = "diagnosis"
	Order     = "order"
)

// 审计动作。
const (
	ActionActivated   = "activated"
	ActionCorrected   = "corrected"
	ActionGranted     = "authorization_granted"
	ActionRevoked     = "authorization_revoked"
	ActionDeactivated = "patient_deactivated"
	ActionExchanged   = "exchange_created"
	ActionReceipted   = "receipt_registered"
)

// 合成数据标记。本包只处理合成患者资料，登记时统一写入。
const SyntheticSource = "synthetic"

// 调用方身份。Internal 为内部使用者，Receiver 为接收方。
//
// 内部使用者可查看全部草稿与完整历史、建立与撤回授权、维护档案；
// 接收方只能凭有效授权读取当前生效版本。
type Actor struct {
	ID   string
	Kind string // "internal" 或 "receiver"
}

// InternalActor 构造一个内部使用者身份。
func InternalActor(id string) Actor { return Actor{ID: id, Kind: "internal"} }

// ReceiverActor 构造一个接收方身份。
func ReceiverActor(id string) Actor { return Actor{ID: id, Kind: "receiver"} }

// IsInternal 报告该身份是否为内部使用者。
func (a Actor) IsInternal() bool { return a.Kind == "internal" }

func (a Actor) valid() bool {
	if a.ID == "" {
		return false
	}
	return a.Kind == "internal" || a.Kind == "receiver"
}

// ID 是患者、就诊、记录、版本、授权与审计事件的稳定标识。
// ID 在各自类型的集合内唯一，由登记操作生成并原样返回，
// 关闭后从同一位置重新打开仍然有效。
type ID = string

// Patient 是一名合成患者的档案。
type Patient struct {
	ID          ID
	Name        string
	Source      string // 始终为 SyntheticSource
	Deactivated bool
	CreatedAt   time.Time
}

// Encounter 是患者的一次就诊。
type Encounter struct {
	ID         ID
	PatientID  ID
	OccurredAt time.Time
	CreatedAt  time.Time
}

// Version 是一条记录在某一时刻被固化的完整内容。
// 生效产生第 1 版；每次更正产生新版本并通过 PrevID 链接到上一版本。
type Version struct {
	ID        ID
	RecordID  ID
	Number    int
	Content   string
	Category  string
	CreatedAt time.Time // 生效或更正发生的时间
	PrevID    ID        // 上一版本；第 1 版为空
	Reason    string    // 更正原因；第 1 版为空
}

// Record 是一条诊断或医嘱。它在草稿与版本之间推进：
// 草稿可反复修改或删除；生效后产生不可变的当前版本，
// 此后只能通过更正产生新版本，不能被直接覆盖或删除。
type Record struct {
	ID          ID
	PatientID   ID
	EncounterID ID
	Category    string

	DraftContent string
	HasDraft     bool

	CurrentVersionID ID
	Versions         []ID // 按版本号顺序，旧到新
}

// Authorization 是内部使用者为一名接收方建立的、针对某患者的读取授权。
// 范围由明确的“就诊 + 类别”组合构成，仅在 [StartsAt, ExpiresAt) 内有效。
type Authorization struct {
	ID         ID
	PatientID  ID
	ReceiverID string
	Scopes     []Scope
	StartsAt   time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time // 撤回时间；nil 表示未撤回
	CreatedAt  time.Time
}

// ActiveAt 报告授权在时刻 t 是否有效：已开始、未到期、未撤回。
// 开始时刻即生效；到截止时刻立即失效（半开区间 [StartsAt, ExpiresAt)）。
func (a *Authorization) ActiveAt(t time.Time) bool {
	if a.RevokedAt != nil {
		return false
	}
	if t.Before(a.StartsAt) {
		return false
	}
	if !t.Before(a.ExpiresAt) {
		return false
	}
	return true
}

// Scope 是授权范围中的一项：某次就诊下的某个记录类别。
type Scope struct {
	EncounterID ID
	Category    string
}

// AuditEvent 是一条仅供内部使用者按患者查看的审计事件。
type AuditEvent struct {
	ID         ID
	PatientID  ID
	ActorID    string
	Action     string
	ObjectType string
	ObjectID   ID
	OccurredAt time.Time
}

// ReadResult 是接收方一次受限读取得到的内容。
type ReadResult struct {
	EncounterID ID
	Category    string
	// Records 为该就诊该类别下、当前生效且被有效授权覆盖的记录，
	// 每项只含当前生效版本的完整内容，不含草稿、旧版本或更正原因。
	Records []EffectiveRecord
}

// EffectiveRecord 是对外暴露的一条当前生效记录。
type EffectiveRecord struct {
	RecordID    ID
	VersionID   ID
	Version     int
	Content     string
	EffectiveAt time.Time
}

// 交换状态：创建后待回执，接收方登记回执后转为已接受或已拒绝。
const (
	ExchangePending  = "pending_receipt"
	ExchangeAccepted = "accepted"
	ExchangeRejected = "rejected"
)

// 回执结果：接受或拒绝打包内容。
const (
	ReceiptAccepted = "accepted"
	ReceiptRejected = "rejected"
)

// PackagedRecord 是包内固化的一条记录版本。包在创建时取各记录的当前生效
// 版本快照：此后记录被更正或新增版本都不会改写包内容。
//
// 只含核对所需的标识、版本号、生效时间与完整内容，不含患者姓名、草稿、
// 历史版本或更正原因（Version.Reason）。
type PackagedRecord struct {
	EncounterID ID
	Category    string
	RecordID    ID
	VersionID   ID
	Version     int
	EffectiveAt time.Time
	Content     string
}

// Package 是一份已固化的交换内容：只含患者与接收方标识及各记录的版本快照。
type Package struct {
	PatientID  ID
	ReceiverID string
	Records    []PackagedRecord
}

// Receipt 是接收方对一份包的回执。接受时 Reason 为空；拒绝时 Reason 必填。
type Receipt struct {
	Outcome      string
	Reason       string
	RegisteredAt time.Time
}

// Exchange 是把已授权记录打包给指定接收方的一次交换。
//
// 创建即把当时的当前版本固化进 Package 并计算摘要；状态初始为待回执。
// 请求号（RequestID）按发起的内部使用者区分，用于安全重试：同一使用者以
// 相同请求号、患者、接收方、绑定授权与记录集合重试时返回原交换。
type Exchange struct {
	ID              ID
	PatientID       ID
	ReceiverID      string
	AuthorizationID ID
	RequestID       string
	CreatorID       string // 创建交换的内部使用者标识
	Status          string // ExchangePending/Accepted/Rejected
	Digest          string // 包内容摘要，接收方回执时须原样回传
	Package         Package
	Receipt         *Receipt
	CreatedAt       time.Time
}

// 失败一律返回明确错误，不留下半条记录。
var (
	// ErrNotFound 表示引用的患者、就诊、记录、版本或授权不存在。
	ErrNotFound = errors.New("clinical: referenced object not found")
	// ErrMismatchedPatient 表示混用了属于不同患者的对象。
	ErrMismatchedPatient = errors.New("clinical: objects belong to different patients")
	// ErrInvalidArgument 表示参数本身不合法（空内容、空范围、时间窗倒置等）。
	ErrInvalidArgument = errors.New("clinical: invalid argument")
	// ErrConflict 表示乐观并发冲突：指定的版本已不再是当前版本。
	ErrConflict = errors.New("clinical: version conflict")
	// ErrDeactivated 表示患者档案已停用，该写操作不再允许。
	ErrDeactivated = errors.New("clinical: patient record is deactivated")
	// ErrActive 表示记录已有生效版本，不能按草稿方式直接覆盖或删除。
	ErrActive = errors.New("clinical: record is already effective")
	// ErrAccessDenied 表示接收方没有覆盖所请求范围的有效授权，
	// 或内部规则不允许该身份执行此操作。返回时不携带任何受保护内容。
	ErrAccessDenied = errors.New("clinical: access denied")
	// ErrClosed 表示 Store 已关闭。
	ErrClosed = errors.New("clinical: store closed")
)
