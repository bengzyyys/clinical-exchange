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
- **停用**：停用后不能新增就诊、改草稿、生效、更正、新建授权或创建交换，接收方也不能继续读取或取包；但指定接收方仍可凭此前取包得到的摘要，对停用前已创建的交换登记回执（只返回确认状态，见“交换回执”一节）；内部使用者仍能查看完整历史。重复停用/撤回幂等，不产生额外变化。
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

## 交换回执：授权撤回后确认已收到的包

取包和回执是两套独立的资格判断，调用方需要区分“还能取包”和“还能确认
已经收到的包”：

- 授权到期、被撤回或患者档案停用会**阻止继续取包**：`FetchPackage` 返回
  `ErrAccessDenied`，交付结果为空，不含包内容、摘要或交换状态。
- 但接收方**此前已经成功取到包**的，其凭当时得到的交换标识与摘要确认这次
  交换的资格**不会被撤销**：`SubmitReceipt` 仍可登记接受/拒绝回执。
- 回执成功**不会恢复授权**，也不会让下一次取包重新获准。
- 回执确认（`ReceiptConfirmation`）只含交换标识、当前状态、回执结果与登记
  时间，**不包含任何包内容**；内部使用者仍可按现有身份限制用
  `GetExchange`/`ListExchanges` 查看原包与正式回执。
- 两个失败条件：摘要与这次交换不符返回 `ErrConflict`；换成其他接收方登记
  返回 `ErrAccessDenied`。两种情况都不改变原有交换状态，也不增加回执审计。

下面是一份完整示例：内部使用者准备一条已生效记录和覆盖它的有效授权，
创建交换后由指定接收方取包；随后撤回该授权，接收方再次取包被拒，但仍能
凭这次成功取包得到的交换标识和摘要登记接受回执。同样的场景也以可运行的
形式收录在 `clinical/example_receipt_test.go` 中。

```go
package main

import (
    "errors"
    "fmt"
    "log"
    "time"

    "github.com/bengzyyys/clinical-exchange/clinical"
)

func must(err error) {
    if err != nil {
        log.Fatal(err)
    }
}

func main() {
    s, err := clinical.Open("./data")
    must(err)
    defer s.Close()

    doc := clinical.InternalActor("doctor-1")
    ins := clinical.ReceiverActor("insurer-1")
    other := clinical.ReceiverActor("insurer-2")

    // 内部使用者准备合成患者资料：一条已生效诊断和覆盖它的有效授权。
    p, err := s.RegisterPatient(doc, "合成患者甲")
    must(err)
    e, err := s.AddEncounter(doc, p.ID, time.Now())
    must(err)
    r, err := s.CreateDraft(doc, p.ID, e.ID, clinical.Diagnosis, "高血压 I10")
    must(err)
    if _, err := s.ActivateRecord(doc, r.ID); err != nil {
        log.Fatal(err)
    }
    now := time.Now()
    a, err := s.Grant(doc, p.ID, ins.ID,
        []clinical.Scope{{EncounterID: e.ID, Category: clinical.Diagnosis}},
        now, now.Add(24*time.Hour))
    must(err)

    // 创建交换，由指定接收方取包；取包成功后才允许使用交付结果继续。
    x, err := s.CreateExchange(doc, p.ID, ins.ID, a.ID, []clinical.ID{r.ID}, "req-2026-0001")
    must(err)
    del, err := s.FetchPackage(ins, x.ID)
    must(err)
    fmt.Println("取包状态:", del.Status, "记录数:", len(del.Package.Records))

    // 取包之后撤回绑定授权：再次取包被拒，交付为空（不含包内容与摘要）。
    must(s.Revoke(doc, p.ID, a.ID))
    denied, err := s.FetchPackage(ins, x.ID)
    fmt.Println("撤回后取包被拒:", errors.Is(err, clinical.ErrAccessDenied),
        "交付为空:", denied.Digest == "" && len(denied.Package.Records) == 0)

    // 两个失败条件：摘要与这次交换不符返回 ErrConflict；换成其他接收方
    // 登记返回 ErrAccessDenied。两者都不改变交换状态，也不增加回执审计。
    auditBefore, err := s.AuditEvents(doc, p.ID)
    must(err)
    _, err = s.SubmitReceipt(ins, del.ExchangeID, "deadbeef", clinical.ReceiptAccepted, "")
    fmt.Println("摘要不符:", errors.Is(err, clinical.ErrConflict))
    _, err = s.SubmitReceipt(other, del.ExchangeID, del.Digest, clinical.ReceiptAccepted, "")
    fmt.Println("其他接收方:", errors.Is(err, clinical.ErrAccessDenied))
    mid, err := s.GetExchange(doc, p.ID, x.ID)
    must(err)
    auditMid, err := s.AuditEvents(doc, p.ID)
    must(err)
    fmt.Println("失败尝试后状态:", mid.Status, "已有回执:", mid.Receipt != nil,
        "审计未增加:", len(auditMid) == len(auditBefore))

    // 指定接收方凭这次成功取包得到的交换标识与摘要登记接受回执。
    // 确认只含交换标识、状态、回执结果与登记时间，不含任何包内容。
    conf, err := s.SubmitReceipt(ins, del.ExchangeID, del.Digest, clinical.ReceiptAccepted, "")
    must(err)
    fmt.Println("回执确认:", conf.Status, conf.Outcome, "登记时间有效:", !conf.RegisteredAt.IsZero())

    // 回执成功不会恢复授权：下一次取包仍然被拒。
    _, err = s.FetchPackage(ins, x.ID)
    fmt.Println("回执后取包仍被拒:", errors.Is(err, clinical.ErrAccessDenied))

    // 内部使用者查看这次交换：正式回执已登记，原包与摘要保持原值。
    got, err := s.GetExchange(doc, p.ID, x.ID)
    must(err)
    fmt.Println("内部查看:", got.Status, got.Receipt.Outcome,
        "摘要未变:", got.Digest == x.Digest,
        "内容未变:", got.Package.Records[0].Content == "高血压 I10")
}
```

结果说明（对应上面的输出）：

```text
取包状态: pending_receipt 记录数: 1
撤回后取包被拒: true 交付为空: true
摘要不符: true
其他接收方: true
失败尝试后状态: pending_receipt 已有回执: false 审计未增加: true
回执确认: accepted accepted 登记时间有效: true
回执后取包仍被拒: true
内部查看: accepted accepted 摘要未变: true 内容未变: true
```

可以看出权限撤回前后发生的变化：撤回前接收方正常取到 1 条记录的包；
撤回后取包立即被拒且拿不到任何交付内容，但交换本身仍是待回执状态——
两次失败登记（摘要错、换人）都没有动它；随后指定接收方凭此前拿到的
摘要登记回执成功，状态转为已接受并记下登记时间，确认中不包含临床内容；
回执之后授权并未恢复，取包依旧被拒；内部查看能看到正式回执，而原包
摘要与内容保持创建时的值。

## 身份模型

| 身份 | 能力 |
| --- | --- |
| 内部使用者 `InternalActor` | 登记、草稿/生效/更正、授权与撤回、查看全部草稿、完整历史与审计 |
| 接收方 `ReceiverActor` | 仅在有效授权范围内读取当前生效版本 |

`clinical.Ready()` 保持基线行为，始终返回 `true`。
