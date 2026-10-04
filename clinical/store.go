package clinical

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dataName 是存放全部业务状态的快照文件名。
const dataName = "clinical-data.json"

// dataVersion 是快照格式的版本标记，便于将来迁移。
const dataVersion = 1

// snapshot 是写入磁盘的完整状态。所有业务对象集中在一个文件中，
// 采用“临时文件 + 原子改名”整体落盘：成功操作与其审计事件一同保留，
// 失败的操作在 mutate 中就已丢弃，不会写出半截状态。
type snapshot struct {
	FormatVersion  int                       `json:"format_version"`
	Patients       map[string]*Patient       `json:"patients"`
	Encounters     map[string]*Encounter     `json:"encounters"`
	Records        map[string]*Record        `json:"records"`
	Versions       map[string]*Version       `json:"versions"`
	Authorizations map[string]*Authorization `json:"authorizations"`
	Exchanges      map[string]*Exchange      `json:"exchanges"`
	AuditEvents    []*AuditEvent             `json:"audit_events"`
}

// Clock 决定“发生时间”的来源。生产使用实时时钟；测试可注入固定/可控时钟
// 以验证开始、到期、撤回等时间边界。
type Clock func() time.Time

// Store 是本地临床档案的句柄。数据存放位置由调用方在 Open 时指定。
// 同一目录同一时刻只允许一个 Store 打开（通过锁文件互斥）。
type Store struct {
	dir   string
	clock Clock
	mu    sync.Mutex
	lock  *os.File
	data  *snapshot
}

// Option 配置 Open 的行为。
type Option func(*Store)

// WithClock 注入自定义时钟。
func WithClock(c Clock) Option {
	return func(s *Store) { s.clock = c }
}

// Open 在指定目录打开（必要时创建）本地存储。
// 调用方指定数据存放位置；关闭后从同一位置重新打开可看到全部历史，
// 到期与否始终按“本次读取时间”重新计算。
func Open(dir string, opts ...Option) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: empty data directory", ErrInvalidArgument)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	lockPath := filepath.Join(dir, ".clinical.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(lockFile); err != nil {
		lockFile.Close()
		return nil, err
	}

	s := &Store{dir: dir, clock: time.Now, lock: lockFile}
	for _, opt := range opts {
		opt(s)
	}
	if s.clock == nil {
		s.clock = time.Now
	}

	if err := s.loadLocked(); err != nil {
		unlockFile(lockFile)
		lockFile.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) loadLocked() error {
	path := filepath.Join(s.dir, dataName)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s.data = newSnapshot()
		return nil
	}
	if err != nil {
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return err
	}
	if snap.FormatVersion != dataVersion {
		return fmt.Errorf("clinical: unsupported data format version %d", snap.FormatVersion)
	}
	ensureMaps(&snap)
	s.data = &snap
	return nil
}

func newSnapshot() *snapshot {
	snap := &snapshot{FormatVersion: dataVersion}
	ensureMaps(snap)
	return snap
}

func ensureMaps(snap *snapshot) {
	if snap.Patients == nil {
		snap.Patients = map[string]*Patient{}
	}
	if snap.Encounters == nil {
		snap.Encounters = map[string]*Encounter{}
	}
	if snap.Records == nil {
		snap.Records = map[string]*Record{}
	}
	if snap.Versions == nil {
		snap.Versions = map[string]*Version{}
	}
	if snap.Authorizations == nil {
		snap.Authorizations = map[string]*Authorization{}
	}
	if snap.Exchanges == nil {
		snap.Exchanges = map[string]*Exchange{}
	}
}

// Close 刷新并释放数据目录。Close 后任何业务方法返回 ErrClosed。
//
// 锁文件标记保留在目录里（不删除）：占用状态完全由锁本身决定，残留的
// 标记文件不代表仍有人占用，也不会阻止下一次打开。删除标记反而会在
// “关闭与接手交错”时让不同调用方锁住不同 inode，破坏独占。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrClosed
	}
	unlockFile(s.lock)
	err := s.lock.Close()
	s.lock = nil
	s.data = nil
	return err
}

func (s *Store) now() time.Time { return s.clock().UTC() }

// mutate 在状态的深拷贝上执行业务变更。fn 返回错误时，拷贝连同其中的
// 半成品一并丢弃，磁盘与既有业务状态保持原样；fn 返回 nil 时整体原子落盘。
// 这样“引用不存在的对象/跨患者混用”等失败都不会留下半条记录。
func (s *Store) mutate(fn func(*snapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return ErrClosed
	}

	clone := cloneSnapshot(s.data)
	if err := fn(clone); err != nil {
		return err
	}
	if err := s.persist(clone); err != nil {
		return err
	}
	s.data = clone
	return nil
}

// view 在只读快照上执行查询。
func (s *Store) view(fn func(*snapshot) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return ErrClosed
	}
	return fn(s.data)
}

func (s *Store) persist(snap *snapshot) error {
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(s.dir, dataName)
	tmp := filepath.Join(s.dir, dataName+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func cloneSnapshot(in *snapshot) *snapshot {
	out := &snapshot{
		FormatVersion:  in.FormatVersion,
		Patients:       make(map[string]*Patient, len(in.Patients)),
		Encounters:     make(map[string]*Encounter, len(in.Encounters)),
		Records:        make(map[string]*Record, len(in.Records)),
		Versions:       make(map[string]*Version, len(in.Versions)),
		Authorizations: make(map[string]*Authorization, len(in.Authorizations)),
		Exchanges:      make(map[string]*Exchange, len(in.Exchanges)),
		AuditEvents:    make([]*AuditEvent, 0, len(in.AuditEvents)),
	}
	for k, v := range in.Patients {
		p := *v
		out.Patients[k] = &p
	}
	for k, v := range in.Encounters {
		e := *v
		out.Encounters[k] = &e
	}
	for k, v := range in.Records {
		r := *v
		r.Versions = append([]ID(nil), v.Versions...)
		out.Records[k] = &r
	}
	for k, v := range in.Versions {
		vv := *v
		out.Versions[k] = &vv
	}
	for k, v := range in.Authorizations {
		a := *v
		a.Scopes = append([]Scope(nil), v.Scopes...)
		a.Selections = append([]RecordSelection(nil), v.Selections...)
		if v.RevokedAt != nil {
			t := *v.RevokedAt
			a.RevokedAt = &t
		}
		out.Authorizations[k] = &a
	}
	for k, x := range in.Exchanges {
		y := *x
		y.Package.Records = append([]PackagedRecord(nil), x.Package.Records...)
		if x.Receipt != nil {
			r := *x.Receipt
			y.Receipt = &r
		}
		out.Exchanges[k] = &y
	}
	for _, ev := range in.AuditEvents {
		e := *ev
		out.AuditEvents = append(out.AuditEvents, &e)
	}
	return out
}

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败在正常运行环境中不应发生；无法安全生成标识时直接中断。
		panic(fmt.Errorf("clinical: cannot generate id: %w", err))
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

func (s *Store) addAudit(snap *snapshot, patientID ID, actor Actor, action, objectType, objectID ID, at time.Time) {
	snap.AuditEvents = append(snap.AuditEvents, &AuditEvent{
		ID:         newID("aud"),
		PatientID:  patientID,
		ActorID:    actor.ID,
		Action:     action,
		ObjectType: objectType,
		ObjectID:   objectID,
		OccurredAt: at,
	})
}
