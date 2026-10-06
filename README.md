# 本地临床记录与交换

在本机运行的临床档案与授权查阅库（Go 包），只处理**合成患者资料**。数据以
JSON 快照原子写入调用方指定的目录，关闭后从同一位置重新打开可完整恢复。

## 使用

```bash
go test ./...
```

## 能力概览

- `clinical.Open(dir)` 打开/创建本地数据目录（带文件锁，同一目录不允许两个进程同时打开）；`Store.Close()` 关闭。
- **患者与就诊**：内部使用者登记合成患者与就诊，患者、就诊、记录、版本、授权均有稳定标识；引用不存在的对象或跨患者混用数据会明确失败，不留下半条记录。
- **草稿 → 生效 → 更正**：诊断/医嘱先存为草稿，可改可删；`ActivateRecord` 固化当时的完整内容与时间。生效记录不可直接覆盖或删除；`CorrectRecord` 必须带非空原因和当前版本号，成功后生成新版本（保留旧内容、`PrevID` 版本链与原因），版本过期返回 `ErrConflict`。
- **授权与接收方读取**：`Grant` 建立整类授权，范围由明确的“就诊 + 诊断/医嘱类别”组成；`GrantSelective` 还可在该就诊的诊断或医嘱中明确选出若干条**已生效记录**（`RecordSelection`）。同一条授权可包含多个就诊、类别，整类范围与选定记录范围可以并存；重复选择同一记录只算一次。被选记录更正后授权覆盖它的当前版本，但同一就诊同类的其他记录以及后来新增、生效的记录都不会自动进入限定范围。授权带 `[开始, 截止)` 时间窗；空范围、空选择、空白标识、跨患者、选择草稿或记录与声明的就诊/类别不符均拒绝（空选择不会被当成整类授权），任一选择不合法就拒绝整条授权。接收方 `Read` 只能看到所有当前有效授权允许记录的合集（每条只出现一次、按稳定顺序），看不到草稿、旧版本或更正原因；未开始、已到期、已撤回或无授权一律返回 `ErrAccessDenied`，不泄露未授权记录的标识、数量或内容。整类与限定重叠时可见整类内容；撤回整类授权后只剩其他有效授权明确允许的记录。多授权独立判断，撤回互不影响；可 `Revoke` 提前撤回。
- **打包交换与回执**：内部使用者 `CreateExchange` 指定患者、接收方、一条当前有效授权、已生效记录集合与非空请求号，把记录的**创建时当前版本**固化成包并计算摘要，状态为待回执。所选每条记录都必须被**绑定的那一条授权**自身覆盖（整类或选定记录），不能借用同一接收方的其他授权补足；记录跨患者、夹带草稿、夹带一条不被绑定授权覆盖的记录、授权不符、患者停用一律拒绝，不留交换或审计，也不占用请求号。请求号按内部使用者区分：相同请求号与相同参数（集合顺序无关）重试返回原交换及现有状态，不重新取内容或新增审计；其他参数变化返回 `ErrConflict`。接收方 `FetchPackage` 按等待结束、真正开始核对本次取包权限的时刻重新检查绑定授权与患者状态（提前发出的请求不会延长授权有效期；其他有效授权不能替代），`SubmitReceipt` 凭摘要登记接受/拒绝回执；授权失效或患者停用后取包被拒，但此前包的回执仍可登记，且只返回确认状态。内部使用者可用 `GetExchange`/`ListExchanges` 按患者查看原包与回执，停用后亦可。
- **停用**：停用后不能新增就诊、改草稿、生效、更正、新建授权，接收方也不能继续读取或取包；但接收方对停用前已取得的包仍可登记回执，内部使用者仍能查看完整历史。重复停用/撤回幂等，不产生额外变化。
- **审计**：生效、更正、授权创建与撤回、档案停用、交换创建与首次回执登记均记录操作身份、时间、对象与动作，仅供内部使用者按患者查看。
- 成功的业务变更与其审计事件在同一次原子写盘中保留；失败操作不改变任何已有状态。

## 最小示例

```go
package main

import (
    "fmt"
    "time"

    "github.com/bengzyyys/clinical-exchange/clinical"
)

func main() {
    doc := clinical.InternalActor("doctor-1")
    ins := clinical.ReceiverActor("insurer-1")

    s, _ := clinical.Open("./data")
    defer s.Close()

    p, _ := s.RegisterPatient(doc, "合成患者甲")
    e, _ := s.AddEncounter(doc, p.ID, time.Now())

    r, _ := s.CreateDraft(doc, p.ID, e.ID, clinical.Diagnosis, "高血压 I10")
    v1, _ := s.ActivateRecord(doc, r.ID)

    // 为接收方授权该就诊下的诊断，时间窗 [now, now+24h)
    now := time.Now()
    a, _ := s.Grant(doc, p.ID, ins.ID,
        []clinical.Scope{{EncounterID: e.ID, Category: clinical.Diagnosis}},
        now, now.Add(24*time.Hour))

    // 接收方按自己的身份读取，只能拿到当前生效版本
    res, err := s.Read(ins, p.ID, e.ID, clinical.Diagnosis)
    fmt.Println(res, err)

    // 更正：必须指明当前版本号与非空原因
    _, _ = s.CorrectRecord(doc, r.ID, v1.Number, "高血压 I10（复核确认）", "补录依据")
    _ = a
}
```

## 取包被拒后仍可登记回执

`FetchPackage`（还能取包）与 `SubmitReceipt`（还能确认已经收到的包）是两项
独立判断，权限撤回后要区分看待：

- **继续取包看当前授权**：绑定授权到期、被撤回，或患者档案停用后，再次
  `FetchPackage` 一律返回 `ErrAccessDenied`，交付结果为空——不含包内容、摘要、
  记录标识或交换状态。其他有效授权不能替代创建时绑定的那一条。
- **回执确认的是此前已经收到的包**：接收方在权限尚在时已成功取包，并保留了
  那次交付返回的交换标识与摘要；授权到期、撤回或档案停用不会撤销其凭这份
  摘要确认这次交换的资格，`SubmitReceipt` 仍可登记。
- **回执不恢复任何授权**：回执成功只改变这次交换的回执状态，不会让绑定授权
  复活，也不会让下一次取包重新获准。
- **确认中不含临床内容**：`ReceiptConfirmation` 只有交换标识、状态、回执结果
  与登记时间。回执登记后，原包内容与摘要保持创建时的原值；内部使用者可用
  `GetExchange`/`ListExchanges` 查看正式回执与原包，该查看仍遵循现有身份限制
  （仅内部使用者，接收方不可用）。
- **两个失败条件**：摘要与这次交换不符返回 `ErrConflict`；换成其他接收方
  （或其他身份、不存在的交换）登记返回 `ErrAccessDenied`。两种失败都不改变
  原有交换状态，也不增加回执审计。

下面的完整示例位于
[`examples/receipt_after_revoke`](examples/receipt_after_revoke/main.go)
（`go run ./examples/receipt_after_revoke`）。它准备合成患者资料与一条已生效
诊断、建立覆盖它的有效授权，创建交换后由指定接收方成功取包；随后撤回授权，
演示再次取包被拒而回执登记成功，并核对内部查看到的正式回执与保持原值的包。
示例对每一步都检查错误，创建或取包失败后不继续使用其返回值：

```go
// 命令 receipt_after_revoke 演示交换回执的允许范围：接收方已成功取到包之后，
// 绑定授权被撤回，继续取包会被拒绝（ErrAccessDenied，且没有交付内容），
// 但接收方凭此次取包得到的交换标识与摘要仍能登记接受回执。
//
// 全程只使用合成患者资料。运行：
//
//  go run ./examples/receipt_after_revoke
package main

import (
    "errors"
    "fmt"
    "os"
    "time"

    "github.com/bengzyyys/clinical-exchange/clinical"
)

func main() {
    dir, err := os.MkdirTemp("", "clinical-receipt-")
    must("创建临时数据目录", err)
    defer os.RemoveAll(dir)

    store, err := clinical.Open(dir)
    must("打开本地存储", err)
    defer store.Close()

    doctor := clinical.InternalActor("doctor-1")
    receiver := clinical.ReceiverActor("insurer-1")
    otherReceiver := clinical.ReceiverActor("insurer-2")

    // 由内部使用者准备一条已生效记录：合成患者、就诊，诊断草稿生效为第 1 版。
    patient, err := store.RegisterPatient(doctor, "合成患者乙")
    must("登记合成患者", err)
    encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
    must("登记就诊", err)
    record, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
        clinical.Diagnosis, "合成诊断：2 型糖尿病 E11")
    must("创建诊断草稿", err)
    _, err = store.ActivateRecord(doctor, record.ID)
    must("生效诊断", err)

    // 建立覆盖它的有效授权：该就诊下的全部诊断，时间窗 [now, now+24h)。
    now := time.Now()
    grant, err := store.Grant(doctor, patient.ID, receiver.ID,
        []clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
        now, now.Add(24*time.Hour))
    must("建立授权", err)

    // 内部使用者创建交换：指定患者、接收方、绑定授权与已生效记录集合。
    exchange, err := store.CreateExchange(doctor, patient.ID, receiver.ID,
        grant.ID, []clinical.ID{record.ID}, "request-receipt-after-revoke")
    must("创建交换", err)

    // 指定接收方第一次取包成功：交付含交换标识、状态、摘要、创建时间与包内容。
    delivery, err := store.FetchPackage(receiver, exchange.ID)
    must("撤回前取包", err)
    fmt.Printf("撤回前取包: 成功，包内记录 %d 条，交换状态=%s\n",
        len(delivery.Package.Records), delivery.Status)

    // 回执必须使用这次成功取包得到的交换标识与摘要，不能另算或猜测。
    exchangeID := delivery.ExchangeID
    digest := delivery.Digest

    // 取包之后，内部使用者撤回绑定授权。
    must("撤回授权", store.Revoke(doctor, patient.ID, grant.ID))

    // 再次取包：ErrAccessDenied，交付为空。被拒绝后不再使用这次的返回值。
    denied, err := store.FetchPackage(receiver, exchangeID)
    switch {
    case err == nil:
        die("授权撤回后取包应当被拒绝")
    case errors.Is(err, clinical.ErrAccessDenied):
        empty := denied.ExchangeID == "" && denied.Status == "" && denied.Digest == "" &&
            len(denied.Package.Records) == 0
        fmt.Printf("撤回后取包: 被拒绝（%v），没有交付内容=%v\n", clinical.ErrAccessDenied, empty)
    default:
        die("撤回后取包应返回 ErrAccessDenied，实际得到 %v", err)
    }

    // “不能继续取包”不等于“不能确认已收到的包”：凭取包时拿到的摘要登记接受。
    conf, err := store.SubmitReceipt(receiver, exchangeID, digest,
        clinical.ReceiptAccepted, "")
    must("登记接受回执", err)
    // 确认只有交换标识、状态、回执结果与登记时间，不含任何临床内容。
    fmt.Printf("登记接受回执: 成功，交换状态=%s，回执结果=%s，已返回登记时间=%v\n",
        conf.Status, conf.Outcome, !conf.RegisteredAt.IsZero())

    // 回执成功不恢复授权：再取一次仍被拒绝，下一次取包不会因此重新获准。
    if _, err := store.FetchPackage(receiver, exchangeID); !errors.Is(err, clinical.ErrAccessDenied) {
        die("回执登记后取包应仍被拒绝，实际得到 %v", err)
    }
    fmt.Printf("回执后再次取包: 仍被拒绝（%v），回执不恢复授权\n", clinical.ErrAccessDenied)

    // 两个与回执直接相关的失败条件：
    //  1. 摘要与这次交换不符 -> ErrConflict；
    //  2. 换成其他接收方登记 -> ErrAccessDenied（不泄露交换是否存在）。
    // 先记下审计条数，随后验证两种失败都不改变交换状态、不新增回执审计。
    auditsBefore, err := store.AuditEvents(doctor, patient.ID)
    must("查看审计", err)

    if _, err := store.SubmitReceipt(receiver, exchangeID, digest+"-tampered",
        clinical.ReceiptAccepted, ""); !errors.Is(err, clinical.ErrConflict) {
        die("摘要不符应返回 ErrConflict，实际得到 %v", err)
    }
    fmt.Printf("摘要不符登记回执: 被拒绝（%v）\n", clinical.ErrConflict)

    if _, err := store.SubmitReceipt(otherReceiver, exchangeID, digest,
        clinical.ReceiptAccepted, ""); !errors.Is(err, clinical.ErrAccessDenied) {
        die("其他接收方登记应返回 ErrAccessDenied，实际得到 %v", err)
    }
    fmt.Printf("其他接收方登记回执: 被拒绝（%v）\n", clinical.ErrAccessDenied)

    auditsAfter, err := store.AuditEvents(doctor, patient.ID)
    must("再次查看审计", err)
    fmt.Printf("两次失败均未新增回执审计: %v\n", len(auditsAfter) == len(auditsBefore))

    // 内部使用者随后查看这次交换：能看到正式回执；原包内容与摘要保持创建时
    // 的原值，包内仍是当时固化的第 1 版。
    view, err := store.GetExchange(doctor, patient.ID, exchangeID)
    must("内部使用者查看交换", err)
    receiptOK := view.Receipt != nil && view.Receipt.Outcome == clinical.ReceiptAccepted &&
        view.Receipt.RegisteredAt.Equal(conf.RegisteredAt)
    contentOK := len(view.Package.Records) == 1 &&
        view.Package.Records[0].Version == 1 &&
        view.Package.Records[0].Content == "合成诊断：2 型糖尿病 E11"
    fmt.Printf("内部查看交换: 状态=%s，可见正式回执=%v，摘要保持原值=%v，原包内容保持原值=%v\n",
        view.Status, receiptOK, view.Digest == digest, contentOK)
}

func must(step string, err error) {
    if err != nil {
        die("%s失败: %v", step, err)
    }
}

func die(format string, args ...any) {
    fmt.Fprintf(os.Stderr, "example: "+format+"\n", args...)
    os.Exit(1)
}
```

结果说明（对照输出）：

```text
撤回前取包: 成功，包内记录 1 条，交换状态=pending_receipt
撤回后取包: 被拒绝（clinical: access denied），没有交付内容=true
登记接受回执: 成功，交换状态=accepted，回执结果=accepted，已返回登记时间=true
回执后再次取包: 仍被拒绝（clinical: access denied），回执不恢复授权
摘要不符登记回执: 被拒绝（clinical: version conflict）
其他接收方登记回执: 被拒绝（clinical: access denied）
两次失败均未新增回执审计: true
内部查看交换: 状态=accepted，可见正式回执=true，摘要保持原值=true，原包内容保持原值=true
```

- **撤回前**：授权有效，指定接收方取到含 1 条记录的包，交换处于
  `pending_receipt`；接收方据此保存交换标识与摘要。
- **撤回后**：同一次交换再取包得到 `ErrAccessDenied` 且没有任何交付内容；
  但凭此前取包得到的摘要登记接受回执成功，交换转为 `accepted`，确认只含
  交换标识、状态、回执结果与登记时间，没有任何临床内容。
- **回执之后**：再次取包仍被拒绝，说明回执不恢复授权；摘要不符得到
  `ErrConflict`、其他接收方登记得到 `ErrAccessDenied`，二者都未改动交换状态、
  未新增回执审计。
- **内部查看**：内部使用者能看到正式接受回执（登记时间与确认一致），而摘要
  与原包内容（第 1 版诊断）保持创建时的原值。

## 身份模型

| 身份 | 能力 |
| --- | --- |
| 内部使用者 `InternalActor` | 登记、草稿/生效/更正、授权与撤回、查看全部草稿、完整历史与审计 |
| 接收方 `ReceiverActor` | 仅在有效授权范围内读取当前生效版本 |

`clinical.Ready()` 保持基线行为，始终返回 `true`。
