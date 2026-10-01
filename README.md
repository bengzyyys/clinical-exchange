# 本地临床记录与交换

这是一个在本机运行的 本地临床记录与交换 Go 包，只处理合成患者资料。
调用方指定数据存放位置后，可以登记患者及其就诊，在就诊下录入诊断和医嘱；
诊断和医嘱先保存为草稿，内部使用者可修改、删除草稿，再生效并保留完整版本历史；
内部使用者还可为接收方建立授权，接收方按身份读取授权范围内的当前生效内容。

## 使用

```bash
go test ./...
```

测试通过表示基线包可以加载。后续能力在这个模块上继续增加。

## 快速上手

```go
import "github.com/bengzyyys/clinical-exchange/clinical"

// 在调用方指定的位置打开（或首次创建）存储。
s, err := clinical.Open("/path/to/data")
defer s.Close()

// 登记患者与就诊。
p, _ := s.RegisterPatient("dr.lee", "张三")
v, _ := s.AddVisit("dr.lee", p.ID, "初诊")

// 诊断/医嘱先存为草稿，可修改、删除，再生效。
rec, _ := s.SaveDraft("dr.lee", p.ID, v.ID, clinical.KindDiagnosis, "感冒")
ver, _ := s.Activate("dr.lee", p.ID, rec.ID) // 第 1 版，保存当时完整内容与时间

// 更正必须提供非空原因并指明当前版本。
_, _ = s.Correct("dr.lee", p.ID, rec.ID, ver.Version, "病情变化", "流感")

// 为接收方建立授权：明确的就诊 + 明确的类别 + 开始/截止时间。
base := time.Now()
auth, _ := s.CreateAuthorization("dr.lee", p.ID, "pharmacy",
    []string{v.ID}, []clinical.RecordKind{clinical.KindDiagnosis},
    base, base.Add(24*time.Hour))

// 接收方按自己的身份读取，只返回有效授权覆盖的当前生效版本；
// 未开始、已到期、已撤回或无授权时返回 clinical.ErrNotAuthorized。
records, err := s.ReadForRecipient("pharmacy", p.ID,
    []string{v.ID}, []clinical.RecordKind{clinical.KindDiagnosis})

// 撤回授权、停用患者（均幂等）。
_, _ = s.RevokeAuthorization("dr.lee", p.ID, auth.ID)
_, _ = s.DeactivatePatient("dr.lee", p.ID)

// 内部使用者可查看草稿、完整历史与按患者隔离的审计事件。
_, _ = s.ListRecords(p.ID)
_, _ = s.ListAuditEvents(p.ID)
```

## 关键约定

- **稳定标识**：患者、就诊、记录、授权、审计事件各有稳定唯一标识。
- **引用完整性**：引用不存在的对象或混用不同患者的数据时明确失败，不留下半条记录。
- **草稿与版本**：草稿可增改删；生效保存当时完整内容与时间。生效记录不得直接覆盖或删除；
  更正必须提供非空原因并指明当前版本，成功后生成同一记录的新版本，保留旧内容、版本关系和原因；
  版本已变更时拒绝更正并保持现状。
- **可见性**：内部使用者可查看全部草稿与完整历史；接收方只能读取当前生效版本，
  看不到草稿、旧版本或更正原因。
- **授权**：范围由明确的就诊与诊断/医嘱类别组成，空范围、跨患者就诊、开始不早于截止均被拒绝；
  授权在开始时刻生效、到截止时刻失效，可提前撤回；多个授权分别判断，撤回一个不影响其他；
  授权允许读取范围内后来生效或更正后的记录，不自动扩大到新增就诊。
- **停用**：停用后不能新增就诊、改动草稿、生效、更正或新建授权，接收方不能继续读取，
  内部仍可查看历史；重复撤回或停用返回已有结果，不产生额外变化。
- **审计**：生效、更正、授权创建与撤回、停用均留下仅供内部按患者查看的审计事件；
  失败操作不改变业务状态，也不产生事件。
- **持久化**：数据以 JSON 文件保存在指定目录，写入采用临时文件加同目录改名的原子方式；
  关闭后从同一位置重新打开，档案、版本、授权状态与审计历史保留，到期判断按本次读取时间计算。
