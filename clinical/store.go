package clinical

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 业务错误。调用方通过 errors.Is 判定类别。
var (
	// ErrNotFound 引用的对象不存在。
	ErrNotFound = errors.New("clinical: 对象不存在")
	// ErrInvalid 输入无效或不满足业务规则。
	ErrInvalid = errors.New("clinical: 输入无效")
	// ErrPatientInactive 患者档案已停用。
	ErrPatientInactive = errors.New("clinical: 患者档案已停用")
	// ErrConflict 更正时指定的版本已不是当前版本。
	ErrConflict = errors.New("clinical: 版本冲突")
	// ErrNotAuthorized 接收方在指定范围上没有有效授权。
	ErrNotAuthorized = errors.New("clinical: 无有效授权")
)

const (
	filePatients       = "patients.json"
	fileVisits         = "visits.json"
	fileRecords        = "records.json"
	fileAuthorizations = "authorizations.json"
	fileAudit          = "audit.json"
)

// Store 是本地临床档案存储。所有变更在互斥锁内完成并原子落盘，
// 失败的操作不会改变已有业务状态。
type Store struct {
	dir string
	mu  sync.Mutex
	// now 默认取当前时间，测试可替换以判定授权窗口。
	now func() time.Time

	patients map[string]*Patient
	visits   map[string]*Visit
	records  map[string]*Record
	auths    map[string]*Authorization
	audits   []*AuditEvent
}

// Open 在 dir 指定的位置打开存储。目录不存在时创建；
// 已存在数据时加载档案、版本、授权状态与审计历史。
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: 数据目录不能为空", ErrInvalid)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: 创建数据目录失败: %v", ErrInvalid, err)
	}
	s := &Store{
		dir:      dir,
		now:      time.Now,
		patients: map[string]*Patient{},
		visits:   map[string]*Visit{},
		records:  map[string]*Record{},
		auths:    map[string]*Authorization{},
	}
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close 将内存中的全部状态落盘后关闭存储。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}

// ---- 持久化 ----

func (s *Store) loadLocked() error {
	read := func(name string, dst any) error {
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(b) == 0 {
			return nil
		}
		return json.Unmarshal(b, dst)
	}

	var patients []*Patient
	if err := read(filePatients, &patients); err != nil {
		return fmt.Errorf("clinical: 加载患者档案失败: %w", err)
	}
	var visits []*Visit
	if err := read(fileVisits, &visits); err != nil {
		return fmt.Errorf("clinical: 加载就诊记录失败: %w", err)
	}
	var records []*Record
	if err := read(fileRecords, &records); err != nil {
		return fmt.Errorf("clinical: 加载记录失败: %w", err)
	}
	var auths []*Authorization
	if err := read(fileAuthorizations, &auths); err != nil {
		return fmt.Errorf("clinical: 加载授权失败: %w", err)
	}
	var audits []*AuditEvent
	if err := read(fileAudit, &audits); err != nil {
		return fmt.Errorf("clinical: 加载审计历史失败: %w", err)
	}

	s.patients = map[string]*Patient{}
	for _, p := range patients {
		s.patients[p.ID] = p
	}
	s.visits = map[string]*Visit{}
	for _, v := range visits {
		s.visits[v.ID] = v
	}
	s.records = map[string]*Record{}
	for _, r := range records {
		s.records[r.ID] = r
	}
	s.auths = map[string]*Authorization{}
	for _, a := range auths {
		s.auths[a.ID] = a
	}
	s.audits = audits
	return nil
}

// persistLocked 原子写入全部数据文件（临时文件 + 同目录改名）。
func (s *Store) persistLocked() error {
	write := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return writeFileAtomic(filepath.Join(s.dir, name), b)
	}
	if err := write(filePatients, s.sortedPatients()); err != nil {
		return err
	}
	if err := write(fileVisits, s.sortedVisits()); err != nil {
		return err
	}
	if err := write(fileRecords, s.sortedRecords()); err != nil {
		return err
	}
	if err := write(fileAuthorizations, s.sortedAuthorizations()); err != nil {
		return err
	}
	if err := write(fileAudit, s.audits); err != nil {
		return err
	}
	return nil
}

// commitLocked 在内存状态变更后落盘；落盘失败时重新加载磁盘状态回滚，
// 保证失败操作不改变已有业务状态。
func (s *Store) commitLocked() error {
	if err := s.persistLocked(); err != nil {
		_ = s.loadLocked()
		return fmt.Errorf("%w: 数据保存失败: %v", ErrInvalid, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// 同步目录项，尽量保证改名落盘。
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ---- 内部辅助 ----

func newID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

func validKind(k RecordKind) bool {
	return k == KindDiagnosis || k == KindOrder
}

func (s *Store) patientLocked(patientID string) (*Patient, error) {
	p, ok := s.patients[patientID]
	if !ok {
		return nil, fmt.Errorf("%w: 患者 %s", ErrNotFound, patientID)
	}
	return p, nil
}

func (s *Store) recordForPatientLocked(patientID, recordID string) (*Record, error) {
	r, ok := s.records[recordID]
	if !ok {
		return nil, fmt.Errorf("%w: 记录 %s", ErrNotFound, recordID)
	}
	if r.PatientID != patientID {
		return nil, fmt.Errorf("%w: 记录 %s 不属于患者 %s", ErrInvalid, recordID, patientID)
	}
	return r, nil
}

func (s *Store) addAuditLocked(patientID, operator, action, object, detail string) {
	s.audits = append(s.audits, &AuditEvent{
		ID:        newID("audit_"),
		PatientID: patientID,
		Operator:  operator,
		Time:      s.now(),
		Object:    object,
		Action:    action,
		Detail:    detail,
	})
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func dedupKinds(in []RecordKind) []RecordKind {
	seen := map[RecordKind]bool{}
	out := make([]RecordKind, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// sortByID 按 getID 返回的稳定标识升序排序。
func sortByID[T any](in []T, getID func(T) string) {
	sort.Slice(in, func(i, j int) bool { return getID(in[i]) < getID(in[j]) })
}

// ---- 排序输出（保证落盘与列表顺序确定） ----

func (s *Store) sortedPatients() []*Patient {
	out := make([]*Patient, 0, len(s.patients))
	for _, p := range s.patients {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) sortedVisits() []*Visit {
	out := make([]*Visit, 0, len(s.visits))
	for _, v := range s.visits {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) sortedRecords() []*Record {
	out := make([]*Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) sortedAuthorizations() []*Authorization {
	out := make([]*Authorization, 0, len(s.auths))
	for _, a := range s.auths {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
