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

## 接收方核对包摘要

接收方 `FetchPackage` 成功后拿到两样东西：固化的包内容
（`PackageDelivery.Package`）和服务附在交付上的摘要
（`PackageDelivery.Digest`）。光“保留摘要并在回执中原样回传”只能让服务登记
回执，不能让接收方自己确认**手中的内容确实与摘要对应的那份一致**。核对方法
不引入任何新接口或新权限：接收方在**本地**按下面的固定规则，从“收到的包
字段”重算一遍摘要，再与服务摘要逐字（区分大小写）比较即可。交换库计算摘要
用的就是这套规则，因此任何一方照此重算都能得到与既有交付一致的结果。

### 参与摘要的包内数据：字段名、类型与顺序

摘要是对一段规范 JSON 字节求 SHA-256。顶层对象与每条记录的字段**名称、
JSON 类型、出现顺序**都固定如下（顺序不同就算另一个文档）：

顶层（`Package`）：

| JSON 字段 | 类型 | 含义 |
| --- | --- | --- |
| `patient_id` | 字符串 | 包所属患者标识 |
| `receiver_id` | 字符串 | 指定接收方标识 |
| `records` | 数组 | 固化的各记录版本，见下 |

`records` 中每条记录（`PackagedRecord`），按此顺序：

| JSON 字段 | 类型 | 含义 |
| --- | --- | --- |
| `encounter_id` | 字符串 | 就诊标识 |
| `category` | 字符串 | 类别，取值为字面量 `"diagnosis"` 或 `"order"` |
| `record_id` | 字符串 | 记录标识 |
| `version_id` | 字符串 | 该固化版本的版本标识（与 `record_id` 是两个不同字段） |
| `version` | 数字（整数，不加引号） | 版本号，如 `2`、`7` |
| `effective_at` | 字符串 | 生效时间，表示法见下 |
| `content` | 字符串 | 完整正文，逐字参与 |

只有上述字段参与摘要。正文 `content` 按**逐字字符串**处理：首尾空格、一个
换行、一个标点的差别都算差异。包里**没有**患者姓名、草稿、旧版本、更正原因
（`Version.Reason`），这些也**不是**接收方需要另行取得的核对材料——核对仅针对
交付中的这些字段；接收方的 `Read` 同样看不到草稿、旧版本与更正原因。

### 文字编码与转义

- 整个文档是 **UTF-8** 编码的 JSON，**紧凑输出，无任何多余空白或换行**。
- 中文（及其它非 ASCII 字符）按 **UTF-8 原样字节**写出，**不**转成
  `\uXXXX`。
- 字符串内按 JSON 转义：半角双引号 `"` → `\"`，反斜杠 `\` → `\\`，
  换行符 → `\n`（其它控制字符按 JSON 规则，如制表符 `\t`、回车 `\r`）。
- 序列化开启 HTML 转义（与 Go `encoding/json` 的默认 `json.Marshal` 一致）：
  正文里的 `<`、`>`、`&` 即使出现在字符串中也分别写成
  `\u003c`、`\u003e`、`\u0026`。这只是换一种转义写法，反解析回字符串后
  仍是原来的 `<`、`>`、`&`，文字含义不变；接收方重算时必须使用同样的转义，
  不能把它们改回原样字符再算。
- 摘要是对这段**精确的 UTF-8 字节序列**求 SHA-256，末尾**不追加换行或空白**。

### 时间表示

- `effective_at` 先把时刻统一转到 **UTC**，再按 Go 的
  `time.RFC3339Nano` 格式化为字符串：
  `YYYY-MM-DDTHH:MM:SS.fffffffffZ`，例如
  `2026-03-14T09:07:05.123456789Z`。
- 小数秒**保留到实际精度**并去掉尾随零：`.123456000` 写成 `.123456Z`；
  整秒时刻省略小数部分（`...:05Z`）。丢掉或补出小数位都会得到不同摘要；
  时刻相差一纳秒摘要也不同。
- 摘要是**基于时刻（instant）**的，与原时区写法无关：同一时刻用 UTC、`+9`、
  `-5` 表示，转成 UTC 后字符串相同、摘要相同。因此“换了同一时刻的时区表示”
  不是内容改动。

### 记录排列规则

- 序列化前，记录一律按 `record_id` 的**字节（字典）升序**排列；
  `CreateExchange` 入参集合中的顺序、交付切片中的顺序都不影响摘要
  （重复的记录标识在创建时已合并为一条）。因此“仅改变记录排列顺序”不是
  内容改动。

### 摘要输出格式

- SHA-256 摘要以**小写十六进制**编码，固定 **64 个字符**（只含 `0-9a-f`），
  例如 `f790315c…a9fe87`。这正是 `PackageDelivery.Digest` 与
  `Exchange.Digest` 中保存、回执时须原样回传的字符串。

下面这份规范字节就是后文金向量参与哈希的**精确内容**（诊断
`rec_diag_golden` 按 `record_id` 排在医嘱 `rec_ord_golden` 之前；注意中文
原样、`<`/`&` 被转义、引号反斜杠与换行的写法）：

```text
{"patient_id":"pat_digest_golden_01","receiver_id":"rcv_digest_golden","records":[{"encounter_id":"enc_digest_golden_01","category":"diagnosis","record_id":"rec_diag_golden","version_id":"ver_diag_golden_2nd","version":2,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a\u003cb 与 c\u0026d；目录 C:\\病历"},{"encounter_id":"enc_digest_golden_01","category":"order","record_id":"rec_ord_golden","version_id":"ver_ord_golden_7th","version":7,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量\u003c10IU 需复核\"；配伍 5%GS\u00260.9%NS；路径 C:\\泵注"}]}
```

对这段字节求 SHA-256（等价于 `printf '%s' '<上面整行>' | sha256sum`）得到：

```text
f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87
```

### 包内容与包外信息

摘要只覆盖上面的包内容，**交换标识、绑定授权标识、请求号、创建交换的内部
使用者、交换当前状态、交换创建时间，以及回执的结果/原因/登记时间都不参与
摘要**。因此交换从 `pending_receipt` 变为 `accepted`/`rejected`、登记或
重交回执，都不会改变摘要；同一包内容经由不同交换标识、不同请求号或不同
（同样合法的）授权交付，摘要相同。取包权限随绑定授权与患者状态变化，但那只
决定**能否取包**，不影响包摘要本身。

### 完整使用示例

完整程序位于
[`examples/verify_digest`](examples/verify_digest/main.go)
（`go run ./examples/verify_digest`）。它先用上面的**固定金向量**（输入与期望
摘要都确定，可逐字节复现），再走真实公开流程（登记合成患者 → 就诊 → 诊断/
医嘱 → 授权 → 创建交换 → 接收方取包）。示例中的 `ReceiverDigest` 只用交付
中的公开字段、在接收方一侧独立重算，不调用交换库的任何内部函数；正文包含
中文、换行、引号、反斜杠、`<`、`&`，生效时间保留九位小数秒：

```go
// 命令 verify_digest 面向接收方演示“取包后如何独立核对摘要”。
//
// 接收方取包（FetchPackage）拿到的是固化的包内容（clinical.Package）与服务
// 附在交付上的摘要（PackageDelivery.Digest）。本示例不依赖任何未导出能力：
// 接收方按文档中公开的规范序列化规则，在本地从“收到的字段”重算一遍摘要，
// 再与服务摘要逐字比较——
//
//	本地摘要 == 服务摘要：收到的内容与服务固化并标注的那份逐字节一致；
//	本地摘要 != 服务摘要：收到的内容至少有一处与服务保存的不同（哪怕只改
//	                     一个字符、一个换行、丢一位小数秒）。
//
// 示例分两部分：
//
//	第一部分用文档中的固定金向量（确定的输入、确定的期望摘要），证明本地
//	重算规则可复现既有交付；只改正文一个字符即得到不同摘要。
//	第二部分走真实公开流程（登记合成患者→就诊→诊断/医嘱→授权→创建交换→
//	接收方取包），对真实交付做本地核对、重排/时区等价核对、正文篡改核对，
//	最后说明 SubmitReceipt 只比较“提交的摘要字符串”与“交换保存的摘要”，
//	并不重新检查接收方手中的临床内容。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/verify_digest
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

func main() {
	// ===== 第一部分：固定金向量，输入与期望摘要都是确定的 =====

	golden := clinical.Package{
		PatientID:  "pat_digest_golden_01",
		ReceiverID: "rcv_digest_golden",
		Records: []clinical.PackagedRecord{
			// 故意把医嘱放在诊断前面：规范序列化会先按 record_id 排序，
			// 入参排列不影响摘要。
			{
				EncounterID: "enc_digest_golden_01",
				Category:    clinical.Order,
				RecordID:    "rec_ord_golden",
				VersionID:   "ver_ord_golden_7th",
				Version:     7,
				EffectiveAt: time.Date(2026, 3, 14, 9, 7, 5, 123456789, time.UTC),
				Content:     "医嘱：胰岛素 8IU（餐前）\n注意 \"剂量<10IU 需复核\"；配伍 5%GS&0.9%NS；路径 C:\\泵注",
			},
			{
				EncounterID: "enc_digest_golden_01",
				Category:    clinical.Diagnosis,
				RecordID:    "rec_diag_golden",
				VersionID:   "ver_diag_golden_2nd",
				Version:     2,
				EffectiveAt: time.Date(2026, 3, 14, 9, 7, 5, 123456789, time.UTC),
				Content:     "2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a<b 与 c&d；目录 C:\\病历",
			},
		},
	}

	const wantGoldenDigest = "f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87"

	fmt.Println("== 金向量：按文档规则在本地从收到的字段重算 ==")
	fmt.Printf("规范字节: %s\n", mustCanonical(golden))
	goldenLocal := ReceiverDigest(golden)
	fmt.Printf("本地摘要: %s\n", goldenLocal)
	fmt.Printf("核对结果: 本地摘要 == 期望摘要 %v\n", goldenLocal == wantGoldenDigest)

	// 只改收到的诊断正文中的一个字符（小于号 -> 书名号），其余全部不动。
	tampered := golden
	tampered.Records = append([]clinical.PackagedRecord(nil), golden.Records...)
	tampered.Records[1].Content = "2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a《b 与 c&d；目录 C:\\病历"
	tamperedLocal := ReceiverDigest(tampered)
	fmt.Printf("只改一个字符后的本地摘要: %s\n", tamperedLocal)
	fmt.Printf("核对结果: 改动后摘要 == 原期望摘要 %v；原先保存的服务摘要仍是 %s\n",
		tamperedLocal == wantGoldenDigest, wantGoldenDigest)

	// ===== 第二部分：真实公开流程，对一份合成交换包端到端核对 =====

	fmt.Println()
	fmt.Println("== 端到端：真实合成交换包的取包核对与回执 ==")

	dir, err := os.MkdirTemp("", "clinical-verify-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	// 固定时钟到带小数秒的时刻，使生效时间保留纳秒小数（非整秒）。
	clockTime := time.Date(2026, 3, 14, 9, 7, 5, 123456789, time.UTC)
	store, err := clinical.Open(dir, clinical.WithClock(func() time.Time { return clockTime }))
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")

	patient, err := store.RegisterPatient(doctor, "摘要核对合成患者")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, clockTime.Add(-3*time.Hour))
	must("登记就诊", err)

	// 诊断与医嘱正文都含中文、换行、半角双引号、反斜杠、小于号、与号。
	diagDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID, clinical.Diagnosis,
		"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a<b 与 c&d；目录 C:\\病历")
	must("创建诊断草稿", err)
	if _, err := store.ActivateRecord(doctor, diagDraft.ID); err != nil {
		die("生效诊断失败: %v", err)
	}
	orderDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID, clinical.Order,
		"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量<10IU 需复核\"；配伍 5%GS&0.9%NS；路径 C:\\泵注")
	must("创建医嘱草稿", err)
	if _, err := store.ActivateRecord(doctor, orderDraft.ID); err != nil {
		die("生效医嘱失败: %v", err)
	}

	grant, err := store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{
			{EncounterID: encounter.ID, Category: clinical.Diagnosis},
			{EncounterID: encounter.ID, Category: clinical.Order},
		},
		clockTime.Add(-24*time.Hour), clockTime.Add(24*time.Hour))
	must("建立授权", err)

	// 故意“医嘱在前、诊断在后”提交：服务固化时按 record_id 排序，与入参顺序无关。
	exchange, err := store.CreateExchange(doctor, patient.ID, receiver.ID, grant.ID,
		[]clinical.ID{orderDraft.ID, diagDraft.ID}, "request-verify-digest")
	must("创建交换", err)

	delivery, err := store.FetchPackage(receiver, exchange.ID)
	must("接收方取包", err)

	serviceDigest := delivery.Digest //取包时服务交付并保存的摘要：核对的比较基准，应原样留存。

	// 1) 正常输入：本地按收到的字段重算，与服务摘要一致。
	localDigest := ReceiverDigest(delivery.Package)
	fmt.Printf("正常取包核对: 本地摘要 == 服务摘要 %v（服务摘要=%s）\n",
		localDigest == serviceDigest, serviceDigest)
	fmt.Println("说明: 端到端各标识由服务随机生成，故该十六进制值每次运行不同；")
	fmt.Println("      确定性的固定值见上方金向量。此处要对照的是各“是否一致”的结论。")

	// 2) 仅改变记录排列顺序：内容未变，摘要必须相同。
	reordered := delivery.Package
	reordered.Records = append([]clinical.PackagedRecord(nil), delivery.Package.Records...)
	reordered.Records[0], reordered.Records[1] = reordered.Records[1], reordered.Records[0]
	fmt.Printf("仅调换记录顺序: 本地摘要 == 服务摘要 %v\n",
		ReceiverDigest(reordered) == serviceDigest)

	// 3) 同一时刻换时区表示（+9/-5 与 UTC 是同一瞬间）：内容未变，摘要必须相同。
	east := time.FixedZone("UTC+9", 9*60*60)
	west := time.FixedZone("UTC-5", -5*60*60)
	rezoned := delivery.Package
	rezoned.Records = append([]clinical.PackagedRecord(nil), delivery.Package.Records...)
	rezoned.Records[0].EffectiveAt = rezoned.Records[0].EffectiveAt.In(east)
	rezoned.Records[1].EffectiveAt = rezoned.Records[1].EffectiveAt.In(west)
	fmt.Printf("仅换同一时刻的时区表示: 本地摘要 == 服务摘要 %v\n",
		ReceiverDigest(rezoned) == serviceDigest)

	// 4) 只改收到的诊断正文一个字符：本地摘要立即不同；保存的服务摘要保持原值，
	//    仍是比较依据。
	received := delivery.Package
	received.Records = append([]clinical.PackagedRecord(nil), delivery.Package.Records...)
	for i := range received.Records {
		if received.Records[i].Category == clinical.Diagnosis {
			received.Records[i].Content =
				"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a《b 与 c&d；目录 C:\\病历"
		}
	}
	tamperedDigest := ReceiverDigest(received)
	fmt.Printf("只改收到的正文一个字符: 本地摘要 == 服务摘要 %v；本地新摘要 != 服务摘要 %v；服务摘要保持原值 %v\n",
		tamperedDigest == serviceDigest, tamperedDigest != serviceDigest, serviceDigest == delivery.Digest)

	// 5) 核对与回执的关系：SubmitReceipt 比较“提交的摘要”与“交换保存的摘要”，
	//    不重新检查接收方手中的临床内容。
	conf, err := store.SubmitReceipt(receiver, exchange.ID, serviceDigest, clinical.ReceiptAccepted, "")
	must("凭原服务摘要登记接受回执", err)
	fmt.Printf("凭原服务摘要登记回执: 成功，交换状态=%s，回执结果=%s\n", conf.Status, conf.Outcome)

	if _, err := store.SubmitReceipt(receiver, exchange.ID, tamperedDigest,
		clinical.ReceiptAccepted, ""); !errors.Is(err, clinical.ErrConflict) {
		die("用被改正文算出的摘要登记回执应得到 ErrConflict，实际 %v", err)
	}
	fmt.Printf("凭被改正文算出的摘要登记回执: 被拒绝（%v）；服务并未读取本地正文，只比对摘要字符串\n",
		clinical.ErrConflict)
}

// ReceiverDigest 是接收方一侧的独立重算：严格按文档的规范序列化规则，
// 从“收到的包字段”算出 64 位小写十六进制 sha256。它不调用交换库的任何
// 内部函数——这正是接收方不预先信任服务摘要、自行核对所需要做的事。
func ReceiverDigest(p clinical.Package) string {
	recs := append([]clinical.PackagedRecord(nil), p.Records...)
	// 记录先按 record_id 字节序升序排列，与入参/交付中的排列无关。
	sort.Slice(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID })

	cp := canonicalPackage{
		PatientID:  p.PatientID,
		ReceiverID: p.ReceiverID,
		Records:    make([]canonicalRecord, 0, len(recs)),
	}
	for _, r := range recs {
		cp.Records = append(cp.Records, canonicalRecord{
			EncounterID: r.EncounterID,
			Category:    r.Category,
			RecordID:    r.RecordID,
			VersionID:   r.VersionID,
			Version:     r.Version,
			// 时间先转 UTC，再按 RFC3339Nano 表示（保留小数秒、去掉尾随零）。
			EffectiveAt: r.EffectiveAt.UTC().Format(time.RFC3339Nano),
			Content:     r.Content,
		})
	}
	// json.Marshal 默认紧凑无空白，并对 <、>、& 做 \u003c/\u003e/\u0026
	// 转义；中文等非 ASCII 字符按 UTF-8 原样写出。
	raw, err := json.Marshal(cp)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw) // 对精确的 UTF-8 字节序列求摘要，不追加换行。
	return hex.EncodeToString(sum[:])
}

func mustCanonical(p clinical.Package) string {
	recs := append([]clinical.PackagedRecord(nil), p.Records...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID })
	cp := canonicalPackage{
		PatientID:  p.PatientID,
		ReceiverID: p.ReceiverID,
		Records:    make([]canonicalRecord, 0, len(recs)),
	}
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
		panic(err)
	}
	return string(raw)
}

// 字段名与顺序固定的规范结构，与交换库内部摘要规则一致。
type canonicalRecord struct {
	EncounterID string `json:"encounter_id"`
	Category    string `json:"category"`
	RecordID    string `json:"record_id"`
	VersionID   string `json:"version_id"`
	Version     int    `json:"version"`
	EffectiveAt string `json:"effective_at"`
	Content     string `json:"content"`
}

type canonicalPackage struct {
	PatientID  string            `json:"patient_id"`
	ReceiverID string            `json:"receiver_id"`
	Records    []canonicalRecord `json:"records"`
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

可对照的输出（金向量部分的两个摘要是确定值；端到端那行服务摘要因
患者/记录标识由服务随机生成，**每次运行不同**，要对照的是各“是否一致”的
结论）：

```text
== 金向量：按文档规则在本地从收到的字段重算 ==
规范字节: {"patient_id":"pat_digest_golden_01", … ,"content":"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量\u003c10IU 需复核\"；配伍 5%GS\u00260.9%NS；路径 C:\\泵注"}]}
本地摘要: f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87
核对结果: 本地摘要 == 期望摘要 true
只改一个字符后的本地摘要: 11adfc39d2e697525a145d33b3494437efebfc04b2932114449d278a4b7f0607
核对结果: 改动后摘要 == 原期望摘要 false；原先保存的服务摘要仍是 f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87

== 端到端：真实合成交换包的取包核对与回执 ==
正常取包核对: 本地摘要 == 服务摘要 true（服务摘要=<每次运行不同>）
说明: 端到端各标识由服务随机生成，故该十六进制值每次运行不同；
      确定性的固定值见上方金向量。此处要对照的是各“是否一致”的结论。
仅调换记录顺序: 本地摘要 == 服务摘要 true
仅换同一时刻的时区表示: 本地摘要 == 服务摘要 true
只改收到的正文一个字符: 本地摘要 == 服务摘要 false；本地新摘要 != 服务摘要 true；服务摘要保持原值 true
凭原服务摘要登记回执: 成功，交换状态=accepted，回执结果=accepted
凭被改正文算出的摘要登记回执: 被拒绝（clinical: version conflict）；服务并未读取本地正文，只比对摘要字符串
```

结果说明（区分三个量）：

- **服务摘要**：取包时 `delivery.Digest` 给出、交换创建时已保存的那个
  64 位十六进制值，是核对与回执共同的比较基准，应原样留存。
- **本地摘要**：接收方用 `ReceiverDigest` 从**收到的字段**独立算出的值。
- **是否匹配**：上述两个字符串逐字比较的布尔结果。
- **正常输入**：两者相等（金向量中本地值正好等于上文固定期望摘要
  `f790…e87`），说明收到的包与服务固化并标注的那份逐字节一致。
- **只改动收到的正文**（示例中诊断正文的一个 `<` 改成 `《`）：本地摘要变为
  `11ad…0607`，与服务摘要**不匹配**；服务保存的摘要不会被接收方一侧的改动
  影响，仍是原来的 `f790…e87`，继续作为比较依据。调换记录顺序、或把同一
  时刻换成 `+9`/`-5` 时区表示，本地摘要都**保持不变**——那不是内容改动。

### 本地核对与回执登记的关系

- **本地核对回答的是“我手里这份对不对”**：本地摘要与服务摘要一致，只证明
  接收方收到的 `Package` 字节与服务创建时固化、并在交付中标注的那份一致；
  它纯粹发生在接收方本地，不向服务登记任何东西，也不改变交换状态。
- **`SubmitReceipt` 回答的是“服务是否记录我确认了这次交换”**：它只比较
  **提交的摘要字符串**与**该交换保存的摘要**——相等才把状态从
  `pending_receipt` 置为 `accepted`/`rejected`；不符返回 `ErrConflict` 且
  状态不变。它**不会重新读取或检查接收方手中的临床内容**：示例里用“被改
  正文算出的摘要”登记即得到 `ErrConflict`，原因只是字符串对不上，而非服务
  发现了正文被改。
- **为什么本地核对成功后仍要按既有方式提交回执**：本地核对成功只让接收方
  自己放心；内部使用者需要通过回执才知道这份包已被接受或拒绝（首次成功
  登记还会写回执审计）。因此确认无误后，仍应取包时留存的**原服务摘要**
  （而不是对被改副本另算的值）按既有方式调用 `SubmitReceipt`。取包权限、
  回执判定与摘要格式均保持不变，本节只是补齐接收方一侧的核对方法与示例。

## 身份模型

| 身份 | 能力 |
| --- | --- |
| 内部使用者 `InternalActor` | 登记、草稿/生效/更正、授权与撤回、查看全部草稿、完整历史与审计 |
| 接收方 `ReceiverActor` | 仅在有效授权范围内读取当前生效版本 |

`clinical.Ready()` 保持基线行为，始终返回 `true`。
