package clinical

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// validContent 校验诊断/医嘱正文：非空白且必须是完整合法的 UTF-8。
// 本地快照以 JSON 落盘，encoding/json 会把无效 UTF-8 字节静默替换为
// U+FFFD——若放行，当次查询看到的是原字节而重开后正文已被改写。因此
// 夹带不完整多字节字符、孤立续字节等任何无效序列的正文一律拒绝，
// 绝不靠替换、截断或跳过坏字节继续保存。用户明确输入的合法 U+FFFD
// 字符本身是合法 UTF-8，不受此限制。
func validContent(content string) bool {
	return utf8.ValidString(content)
}

// CreateDraft 在某次就诊下新建一条诊断或医嘱草稿。
// 患者必须存在且未停用；就诊必须属于该患者（否则 ErrMismatchedPatient）；
// 类别必须是 Diagnosis 或 Order，内容不能为空。
func (s *Store) CreateDraft(actor Actor, patientID, encounterID ID, category, content string) (Record, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Record{}, ErrAccessDenied
	}
	if patientID == "" || encounterID == "" {
		return Record{}, fmt.Errorf("%w: patient and encounter ids are required", ErrInvalidArgument)
	}
	if !validCategory(category) {
		return Record{}, fmt.Errorf("%w: category must be %q or %q", ErrInvalidArgument, Diagnosis, Order)
	}
	if strings.TrimSpace(content) == "" {
		return Record{}, fmt.Errorf("%w: draft content is required", ErrInvalidArgument)
	}
	if !validContent(content) {
		return Record{}, fmt.Errorf("%w: draft content must be valid UTF-8", ErrInvalidArgument)
	}

	var result Record
	err := s.mutate(func(snap *snapshot) error {
		if _, err := requireActivePatient(snap, patientID); err != nil {
			return err
		}
		if _, err := requireEncounter(snap, patientID, encounterID); err != nil {
			return err
		}
		r := &Record{
			ID:           newID("rec"),
			PatientID:    patientID,
			EncounterID:  encounterID,
			Category:     category,
			DraftContent: content,
			HasDraft:     true,
		}
		snap.Records[r.ID] = r
		result = *r
		return nil
	})
	return result, err
}

// UpdateDraft 修改尚未生效的草稿内容。已生效的记录不能再走草稿路径
// （返回 ErrActive），必须使用 CorrectRecord 更正。
func (s *Store) UpdateDraft(actor Actor, recordID ID, content string) (Record, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Record{}, ErrAccessDenied
	}
	if strings.TrimSpace(content) == "" {
		return Record{}, fmt.Errorf("%w: draft content is required", ErrInvalidArgument)
	}
	if !validContent(content) {
		return Record{}, fmt.Errorf("%w: draft content must be valid UTF-8", ErrInvalidArgument)
	}

	var result Record
	err := s.mutate(func(snap *snapshot) error {
		r, err := requireDraftRecord(snap, recordID)
		if err != nil {
			return err
		}
		if _, err := requireActivePatient(snap, r.PatientID); err != nil {
			return err
		}
		r.DraftContent = content
		result = *r
		return nil
	})
	return result, err
}

// DeleteDraft 删除一条尚未生效的草稿及其记录。草稿从未生效，
// 因此删除不影响任何生效内容，也不产生审计事件。
func (s *Store) DeleteDraft(actor Actor, recordID ID) error {
	if !actor.valid() || !actor.IsInternal() {
		return ErrAccessDenied
	}
	return s.mutate(func(snap *snapshot) error {
		r, err := requireDraftRecord(snap, recordID)
		if err != nil {
			return err
		}
		if _, err := requireActivePatient(snap, r.PatientID); err != nil {
			return err
		}
		delete(snap.Records, r.ID)
		return nil
	})
}

// ActivateRecord 使草稿生效：固化当时的完整内容与时间为第 1 版。
// 生效后草稿被清除，记录不能再被直接覆盖或删除，只能更正。
// 生效与审计事件在同一次原子落盘中保留。
func (s *Store) ActivateRecord(actor Actor, recordID ID) (Version, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Version{}, ErrAccessDenied
	}
	var result Version
	err := s.mutate(func(snap *snapshot) error {
		r, err := requireDraftRecord(snap, recordID)
		if err != nil {
			return err
		}
		if _, err := requireActivePatient(snap, r.PatientID); err != nil {
			return err
		}
		now := s.now()
		v := &Version{
			ID:        newID("ver"),
			RecordID:  r.ID,
			Number:    1,
			Content:   r.DraftContent,
			Category:  r.Category,
			CreatedAt: now,
		}
		snap.Versions[v.ID] = v
		r.CurrentVersionID = v.ID
		r.Versions = append(r.Versions, v.ID)
		r.HasDraft = false
		r.DraftContent = ""
		s.addAudit(snap, r.PatientID, actor, ActionActivated, "record", r.ID, now)
		result = *v
		return nil
	})
	return result, err
}

// CorrectRecord 对当前生效版本进行更正，生成同一条记录的新版本。
//
// reason 必须非空（不能仅含空白）且必须是完整合法的 UTF-8，并会原样保存；
// expectedCurrentVersion 必须等于记录当前版本号，
// 否则返回 ErrConflict 且状态保持不变。旧版本内容、版本关系（PrevID）
// 与更正原因都会保留。新版本与审计事件一同原子落盘。
func (s *Store) CorrectRecord(actor Actor, recordID ID, expectedCurrentVersion int, content, reason string) (Version, error) {
	if !actor.valid() || !actor.IsInternal() {
		return Version{}, ErrAccessDenied
	}
	if strings.TrimSpace(content) == "" {
		return Version{}, fmt.Errorf("%w: corrected content is required", ErrInvalidArgument)
	}
	if !validContent(content) {
		return Version{}, fmt.Errorf("%w: corrected content must be valid UTF-8", ErrInvalidArgument)
	}
	if strings.TrimSpace(reason) == "" {
		return Version{}, fmt.Errorf("%w: correction reason is required", ErrInvalidArgument)
	}
	// 更正原因随版本原样落盘，是解释病历变化的正式历史，必须是完整合法的
	// UTF-8：坏字节位于开头、中间、结尾，或最后一个多字节字符没有写完整，
	// 都在此拒绝，不能让 JSON 落盘把原因替换成 U+FFFD 导致重开后同一版本
	// 的解释与提交时不同，也不能只保存其中合法的部分。即使用户同时写了可读
	// 的中文说明也一样拒绝。用户明确输入的合法 U+FFFD 字符本身是合法
	// UTF-8，不受此限制。
	if !validContent(reason) {
		return Version{}, fmt.Errorf("%w: correction reason must be valid UTF-8", ErrInvalidArgument)
	}
	if expectedCurrentVersion <= 0 {
		return Version{}, fmt.Errorf("%w: expected current version must be positive", ErrInvalidArgument)
	}

	var result Version
	err := s.mutate(func(snap *snapshot) error {
		r := snap.Records[recordID]
		if r == nil {
			return fmt.Errorf("%w: record %q", ErrNotFound, recordID)
		}
		if _, err := requireActivePatient(snap, r.PatientID); err != nil {
			return err
		}
		if r.CurrentVersionID == "" {
			return fmt.Errorf("%w: record %q has no effective version; activate its draft first", ErrInvalidArgument, recordID)
		}
		current := snap.Versions[r.CurrentVersionID]
		if current == nil {
			// 内部状态不应出现；按不存在处理，拒绝写入。
			return fmt.Errorf("%w: current version %q", ErrNotFound, r.CurrentVersionID)
		}
		if current.Number != expectedCurrentVersion {
			return fmt.Errorf("%w: record %q is at version %d, not %d",
				ErrConflict, recordID, current.Number, expectedCurrentVersion)
		}

		now := s.now()
		v := &Version{
			ID:        newID("ver"),
			RecordID:  r.ID,
			Number:    current.Number + 1,
			Content:   content,
			Category:  r.Category,
			CreatedAt: now,
			PrevID:    current.ID,
			Reason:    reason,
		}
		snap.Versions[v.ID] = v
		r.CurrentVersionID = v.ID
		r.Versions = append(r.Versions, v.ID)
		s.addAudit(snap, r.PatientID, actor, ActionCorrected, "record", r.ID, now)
		result = *v
		return nil
	})
	return result, err
}

// requireDraftRecord 取出仍处于草稿阶段（有草稿且尚无生效版本）的记录。
func requireDraftRecord(snap *snapshot, recordID ID) (*Record, error) {
	r := snap.Records[recordID]
	if r == nil {
		return nil, fmt.Errorf("%w: record %q", ErrNotFound, recordID)
	}
	if r.CurrentVersionID != "" || !r.HasDraft {
		return nil, fmt.Errorf("%w: record %q is already effective", ErrActive, recordID)
	}
	return r, nil
}
