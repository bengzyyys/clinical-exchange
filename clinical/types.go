package clinical

import "time"

// RecordKind 是就诊下记录的类别：诊断或医嘱。
type RecordKind string

const (
	// KindDiagnosis 诊断记录。
	KindDiagnosis RecordKind = "diagnosis"
	// KindOrder 医嘱记录。
	KindOrder RecordKind = "order"
)

// Patient 是建档的合成患者资料。
type Patient struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Active        bool       `json:"active"`
	CreatedAt     time.Time  `json:"created_at"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
}

// Visit 是患者名下的一次就诊。
type Visit struct {
	ID        string    `json:"id"`
	PatientID string    `json:"patient_id"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// RecordVersion 是记录的一个生效版本，保存生效当时的完整内容与时间。
type RecordVersion struct {
	Version   int       `json:"version"`
	Content   string    `json:"content"`
	Reason    string    `json:"reason,omitempty"`
	Operator  string    `json:"operator"`
	CreatedAt time.Time `json:"created_at"`
}

// RecordDraft 是尚未生效的草稿，内部使用者可以修改或删除。
type RecordDraft struct {
	Content   string    `json:"content"`
	Operator  string    `json:"operator"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Record 是就诊下的一条诊断或医嘱记录。
// 生效前只有 Draft；生效后保留完整版本历史，Draft 清空。
type Record struct {
	ID             string          `json:"id"`
	PatientID      string          `json:"patient_id"`
	VisitID        string          `json:"visit_id"`
	Kind           RecordKind      `json:"kind"`
	CurrentVersion int             `json:"current_version"`
	Versions       []RecordVersion `json:"versions"`
	Draft          *RecordDraft    `json:"draft,omitempty"`
}

// Authorization 是内部使用者为接收方建立的读取授权。
// 范围由明确的就诊集合与记录类别集合组成，含开始与截止时间。
type Authorization struct {
	ID         string       `json:"id"`
	PatientID  string       `json:"patient_id"`
	Recipient  string       `json:"recipient"`
	Visits     []string     `json:"visits"`
	Categories []RecordKind `json:"categories"`
	StartAt    time.Time    `json:"start_at"`
	EndAt      time.Time    `json:"end_at"`
	Revoked    bool         `json:"revoked"`
	CreatedAt  time.Time    `json:"created_at"`
	CreatedBy  string       `json:"created_by"`
	RevokedAt  *time.Time   `json:"revoked_at,omitempty"`
	RevokedBy  *string      `json:"revoked_by,omitempty"`
}

// AuditEvent 是仅供内部使用者按患者查看的审计事件。
type AuditEvent struct {
	ID        string    `json:"id"`
	PatientID string    `json:"patient_id"`
	Operator  string    `json:"operator"`
	Time      time.Time `json:"time"`
	Object    string    `json:"object"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail,omitempty"`
}

// RecipientRecord 是接收方按授权读取到的当前生效记录内容。
type RecipientRecord struct {
	RecordID string     `json:"record_id"`
	VisitID  string     `json:"visit_id"`
	Kind     RecordKind `json:"kind"`
	Version  int        `json:"version"`
	Content  string     `json:"content"`
}
