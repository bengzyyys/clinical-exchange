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
- **授权与接收方读取**：`Grant` 建立整类授权，范围由明确的“就诊 + 诊断/医嘱类别”组成；`GrantSelective` 还可在该就诊的诊断或医嘱中明确选出若干条**已生效记录**（`RecordSelection`）。同一条授权可包含多个就诊、类别，整类范围与选定记录范围可以并存；重复选择同一记录只算一次。被选记录更正后授权覆盖它的当前版本，但同一就诊同类的其他记录以及后来新增、生效的记录都不会自动进入限定范围。授权带 `[开始, 截止)` 时间窗；空范围、空选择、空白标识、跨患者、选择草稿或记录与声明的就诊/类别不符均拒绝（空选择不会被当成整类授权），任一选择不合法就拒绝整条授权。接收方 `Read` 只能看到所有当前有效授权允许记录的合集（每条只出现一次、按稳定顺序），看不到草稿、旧版本或更正原因；未开始、已到期、已撤回或无授权一律返回 `ErrAccessDenied`，不泄露未授权记录的标识、数量或内容。整类与限定重叠时可见整类内容；撤回整类授权后只剩其他有效授权明确允许的记录。多授权独立判断，撤回互不影响；可 `Revoke` 提前撤回。内部使用者可用 `ListAuthorizations` 按患者（可再按接收方）列出该患者已保存的**全部**授权（含尚未开始、已到期、已撤回者，按授权标识升序）；清单只是历史视图，列出一条授权不代表接收方此刻能读取临床内容，接收方身份不可调用。
- **打包交换与回执**：内部使用者 `CreateExchange` 指定患者、接收方、一条当前有效授权、已生效记录集合与非空请求号，把记录的**创建时当前版本**固化成包并计算摘要，状态为待回执。请求号必须是完整合法的 UTF-8：空串、全空白，或任何位置夹带无效字节、不完整多字节字符的请求号一律拒绝；请求号原样保存与比较，不做字符形态统一、不去首尾空白，含中文、表情、换行或用户明确输入的合法 U+FFFD 的请求号均可使用，坏字节绝不会被替换成 U+FFFD 后命中已保存交换。所选每条记录都必须被**绑定的那一条授权**自身覆盖（整类或选定记录），不能借用同一接收方的其他授权补足；记录跨患者、夹带草稿、夹带一条不被绑定授权覆盖的记录、授权不符、患者停用一律拒绝，不留交换或审计，也不占用请求号。请求号按内部使用者区分：相同请求号与相同参数（集合顺序无关）重试返回原交换及现有状态，不重新取内容或新增审计；其他参数变化返回 `ErrConflict`。接收方 `FetchPackage` 按等待结束、真正开始核对本次取包权限的时刻重新检查绑定授权与患者状态（提前发出的请求不会延长授权有效期；其他有效授权不能替代），`SubmitReceipt` 凭摘要登记接受/拒绝回执；授权失效或患者停用后取包被拒，但此前包的回执仍可登记，且只返回确认状态。内部使用者可用 `GetExchange`/`ListExchanges` 按患者查看原包与回执，停用后亦可。
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

## 只共享指定诊断：限定记录授权

最小示例中的 `Grant` 是**整类授权**：一旦授权某次就诊的诊断类别，该就诊
下的全部已生效诊断（含授权后才生效的）都对接收方可见。只需要共享其中
**某一条**诊断时，使用 `GrantSelective` 的限定记录范围：

- **整类范围留空，只列限定选择**：`scopes` 传 `nil`，用 `selections`
  明确选出记录。每项 `RecordSelection` 必须同时填三个字段：记录标识
  `RecordID`、记录所属就诊 `EncounterID`、记录类别 `Category`
  （`clinical.Diagnosis` 或 `clinical.Order`），三者必须与实际一致。
- **只能选已生效记录**：建立授权时记录必须存在、属于该患者且已生效；
  选择草稿、空白标识、记录与声明的就诊/类别不符返回 `ErrInvalidArgument`
  （记录不存在返回 `ErrNotFound`，跨患者返回 `ErrMismatchedPatient`）。
- **时间窗覆盖读取时刻**：与 `Grant` 相同的半开区间
  `[startsAt, expiresAt)`，读取时刻落在窗外（未开始或已到期）等同于
  没有授权。
- **接收方读到的是哪条记录、哪个版本**：`Read` 返回的每条
  `EffectiveRecord` 含 `RecordID`、`VersionID`、`Version`（版本号）、
  `Content` 与 `EffectiveAt`，接收方可以明确核对读到的是哪条记录的第
  几版；结果中只有当前生效版本，不含草稿、旧版本或更正原因。
- **选中的是记录，不是某个固定版本**：被选中记录更正后，读取自动跟随到
  它的当前版本（新版本号、新正文），旧版本与更正原因仍不向接收方提供；
  但同一就诊同类的其他记录、以及授权后新增并生效的记录，都不会自动进入
  这条限定授权。
- **限定授权不是对其他授权的缩减**：读取结果是该接收方**所有当前有效
  授权**允许记录的合集。若同一接收方另有一条覆盖该范围的有效整类授权，
  读取会合并两条授权允许的记录，重叠的记录只出现一次——限定授权只增加
  明确允许的记录，不会削减其他授权本来允许的内容。下面的示例中接收方
  只持有这一条限定授权，因此只能看到被选中的那一条。
- **失败是整体的**：任一选择不合法就拒绝整条授权——不保存合法部分、
  不新增授权创建审计，此前允许读取的内容保持原样。整类范围与限定选择
  **同时为空**同样返回 `ErrInvalidArgument`：空选择不表示共享全部诊断。
- **读取被拒**：没有任何覆盖所请求范围的有效授权（未开始、已到期、
  已撤回或根本没有授权），或患者档案已停用，`Read` 返回
  `ErrAccessDenied`，结果中不含任何记录的标识、数量或内容。

下面的完整示例位于
[`examples/selective_grant`](examples/selective_grant/main.go)
（`go run ./examples/selective_grant`）。它独立准备本地存储、内部使用者、
接收方、合成患者与就诊，在同一次就诊中准备两条已生效诊断和一条诊断草稿，
用 `GrantSelective` 只授权第一条生效诊断，演示接收方读取、更正后跟随新
版本、授权后新增记录不自动进入，以及两种建立授权的失败。示例对每一步
业务调用都检查错误：准备资料或授权失败时由 `must` 明确失败发生在哪一步
并终止，不继续把失败返回值当作正式记录或正式授权使用：

```go
// 命令 selective_grant 演示“只共享指定诊断”的限定记录授权：同一次就诊中
// 准备两条已生效诊断和一条诊断草稿，内部使用者用 GrantSelective 只把第一条
// 已生效诊断授权给接收方（整类范围留空）。接收方读取该次就诊的诊断时只能
// 看到这一条；更正被选中诊断后读到新版本号与新正文，旧版本与更正原因不向
// 接收方提供；授权后新增并生效的同类记录不自动进入这条授权。随后演示两种
// 建立授权的失败：限定选择夹带草稿、整类范围与限定选择同时为空，二者都被
// 整体拒绝，不保留合法部分，也不新增授权创建审计。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/selective_grant
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-selective-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")
	stranger := clinical.ReceiverActor("insurer-2")

	// ---- 准备资料：合成患者、一次就诊、两条已生效诊断与一条诊断草稿 ----
	patient, err := store.RegisterPatient(doctor, "合成患者丁")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
	must("登记就诊", err)

	diag1, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断一：高血压 I10")
	must("创建第一条诊断草稿", err)
	v1, err := store.ActivateRecord(doctor, diag1.ID)
	must("生效第一条诊断", err)

	diag2, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断二：2 型糖尿病 E11")
	must("创建第二条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag2.ID)
	must("生效第二条诊断", err)

	draft, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断草稿：内容待补充")
	must("创建诊断草稿（保持草稿状态）", err)
	fmt.Println("准备完成: 同一次就诊有两条已生效诊断和一条诊断草稿")

	// ---- 建立限定授权：整类范围留空，只选第一条已生效诊断 ----
	// 选择中明确填写记录标识及其所属就诊和类别；时间窗 [now, now+24h)
	// 覆盖下面的读取时刻。
	now := time.Now()
	grant, err := store.GrantSelective(doctor, patient.ID, receiver.ID,
		nil, // 整类范围留空：不共享该就诊下的全部诊断
		[]clinical.RecordSelection{{
			EncounterID: encounter.ID,       // 记录所属就诊
			Category:    clinical.Diagnosis, // 记录类别
			RecordID:    diag1.ID,           // 选中的记录标识
		}},
		now, now.Add(24*time.Hour))
	must("建立限定授权", err)
	fmt.Printf("建立限定授权: 成功，整类范围 %d 项，限定选择 %d 条\n",
		len(grant.Scopes), len(grant.Selections))

	// ---- 接收方读取：只得到第一条诊断的当前生效内容 ----
	res, err := store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("接收方首次读取", err)
	onlyFirst := len(res.Records) == 1 &&
		res.Records[0].RecordID == diag1.ID &&
		res.Records[0].Version == v1.Number &&
		res.Records[0].Content == "合成诊断一：高血压 I10"
	fmt.Printf("接收方首次读取: 记录数=%d，只含被选中的第一条（版本=%d）=%v，第二条与草稿均不出现\n",
		len(res.Records), res.Records[0].Version, onlyFirst)

	// 没有任何授权的其他接收方读取同一范围：ErrAccessDenied，结果为空。
	if _, err := store.Read(stranger, patient.ID, encounter.ID, clinical.Diagnosis); !errors.Is(err, clinical.ErrAccessDenied) {
		die("无授权接收方读取应返回 ErrAccessDenied，实际得到 %v", err)
	}
	fmt.Printf("无授权接收方读取: 被拒绝（%v），不泄露任何记录\n", clinical.ErrAccessDenied)

	// ---- 更正被选中的诊断：授权选中的是记录，读取跟随到它的当前版本 ----
	v2, err := store.CorrectRecord(doctor, diag1.ID, v1.Number,
		"合成诊断一：高血压 I10（复核确认）", "补录复核依据")
	must("更正被选中的诊断", err)

	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("更正后读取", err)
	updated := len(res.Records) == 1 &&
		res.Records[0].RecordID == diag1.ID &&
		res.Records[0].Version == v2.Number &&
		res.Records[0].Content == "合成诊断一：高血压 I10（复核确认）"
	fmt.Printf("更正后读取: 仍为 1 条，展示新版本号=%d 与新正文=%v；旧版本与更正原因不在读取结果中\n",
		res.Records[0].Version, updated)

	// ---- 授权后新增并生效的同类记录不自动进入这条限定授权 ----
	diag3, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断三：高脂血症 E78.5")
	must("创建第三条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag3.ID)
	must("生效第三条诊断", err)

	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("新增诊断后读取", err)
	stillOnlyFirst := len(res.Records) == 1 && res.Records[0].RecordID == diag1.ID
	fmt.Printf("授权后新增并生效一条诊断: 读取记录数=%d，仍只有第一条=%v（新记录不自动进入限定授权）\n",
		len(res.Records), stillOnlyFirst)

	// ---- 失败演示一：限定选择夹带草稿，整条授权不成立 ----
	auditsBefore, err := store.AuditEvents(doctor, patient.ID)
	must("查看审计", err)

	_, err = store.GrantSelective(doctor, patient.ID, receiver.ID, nil,
		[]clinical.RecordSelection{
			{EncounterID: encounter.ID, Category: clinical.Diagnosis, RecordID: diag2.ID}, // 合法生效诊断
			{EncounterID: encounter.ID, Category: clinical.Diagnosis, RecordID: draft.ID}, // 草稿：不合法
		}, now, now.Add(24*time.Hour))
	if !errors.Is(err, clinical.ErrInvalidArgument) {
		die("限定选择夹带草稿应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	// 授权未建立：不继续使用任何返回值，合法部分（第二条诊断）也不会被保留。
	fmt.Printf("限定选择夹带草稿: 被拒绝（%v），整条授权不成立，合法部分不保留\n", clinical.ErrInvalidArgument)

	// ---- 失败演示二：整类范围与限定选择同时为空 ----
	_, err = store.GrantSelective(doctor, patient.ID, receiver.ID, nil, nil,
		now, now.Add(24*time.Hour))
	if !errors.Is(err, clinical.ErrInvalidArgument) {
		die("空范围空选择应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	fmt.Printf("整类范围与限定选择同时为空: 被拒绝（%v），空选择不表示共享全部诊断\n", clinical.ErrInvalidArgument)

	// 两次失败都不新增授权创建审计；原先允许读取的内容保持原样。
	auditsAfter, err := store.AuditEvents(doctor, patient.ID)
	must("再次查看审计", err)
	fmt.Printf("两次失败均未新增授权创建审计: %v\n",
		countGrants(auditsAfter) == countGrants(auditsBefore))

	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("失败后读取", err)
	unchanged := len(res.Records) == 1 &&
		res.Records[0].RecordID == diag1.ID &&
		res.Records[0].Version == v2.Number
	fmt.Printf("失败后读取: 内容保持原样（仍只有第一条的第 %d 版）=%v\n",
		res.Records[0].Version, unchanged)
}

// countGrants 统计审计事件中的授权创建条数。
func countGrants(events []clinical.AuditEvent) int {
	n := 0
	for _, e := range events {
		if e.Action == clinical.ActionGranted {
			n++
		}
	}
	return n
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
准备完成: 同一次就诊有两条已生效诊断和一条诊断草稿
建立限定授权: 成功，整类范围 0 项，限定选择 1 条
接收方首次读取: 记录数=1，只含被选中的第一条（版本=1）=true，第二条与草稿均不出现
无授权接收方读取: 被拒绝（clinical: access denied），不泄露任何记录
更正后读取: 仍为 1 条，展示新版本号=2 与新正文=true；旧版本与更正原因不在读取结果中
授权后新增并生效一条诊断: 读取记录数=1，仍只有第一条=true（新记录不自动进入限定授权）
限定选择夹带草稿: 被拒绝（clinical: invalid argument），整条授权不成立，合法部分不保留
整类范围与限定选择同时为空: 被拒绝（clinical: invalid argument），空选择不表示共享全部诊断
两次失败均未新增授权创建审计: true
失败后读取: 内容保持原样（仍只有第一条的第 2 版）=true
```

- **限定读取**：接收方首次读取只得到被选中的第一条诊断（第 1 版），
  同一次就诊的第二条已生效诊断与诊断草稿都不出现；没有任何授权的其他
  接收方读取同一范围得到 `ErrAccessDenied`。
- **更正跟随**：更正被选中的诊断后，读取展示新版本号（第 2 版）与新
  正文；旧版本与更正原因不在 `Read` 的结果结构中，接收方无从取得。
- **不自动扩展**：授权后新增并生效的第三条诊断不进入这条限定授权，
  读取仍只有第一条——限定授权选中的是明确列出的记录本身。
- **失败是整体的**：把一条合法生效诊断与那条草稿一起放入限定选择，
  返回 `ErrInvalidArgument`，整条新授权不成立，合法部分不保留，授权
  创建审计不增加；整类范围与限定选择同时为空同样被拒绝。两次失败后
  接收方读到的内容与之前完全一致。

## 内部使用者查看患者授权清单

`ListAuthorizations(actor, patientID, receiverID)` 仅供**内部使用者**使用：
按患者列出该患者档案下**已保存的全部授权**。它是一份只读的历史视图，不是
“接收方此刻能读到哪些临床内容”——清单里出现一条授权，只说明这条授权被建立
并保存在该患者名下，不代表接收方凭它现在就能 `Read`；接收方实际读取时仍按
既有规则在读取时刻重新逐条判断授权与患者状态，不能用清单代替这项检查。

- **只按患者查询**：`receiverID` 传**空字符串**，列出该患者授予**所有接收方**
  的授权。**再指定接收方**：填写接收方标识，只保留授予该接收方的条目。两个
  过滤条件同时作用：即使另一名患者也授权给了同一个接收方，那一条也不会混入
  本患者的清单；反过来，同患者授予其他接收方的条目也不会因为指定了接收方而
  出现。
- **空清单是成功结果**：患者存在但从无授权，或指定的接收方没有获得该患者的
  任何授权，都正常返回错误为空、长度为 0 的清单——既不表示患者不存在，也不
  表示查询被拒绝。只有患者标识本身不存在时才返回 `ErrNotFound`。
- **结果按授权标识升序排列**（标识的字符串字典序），不是按创建时间排序；
  不要把返回顺序当作建立先后顺序使用。
- **四种生命周期状态全部入列**：当前有效、尚未开始、已经到期、已经撤回的授权
  都在。每条保留**自己的**接收方、整类范围、限定记录范围、时间窗
  `[StartsAt, ExpiresAt)` 与撤回时间 `RevokedAt`；多条授权永远各自独立成行，
  不会把同一接收方或同一范围的多条授权合并成一条，撤回一条也不影响其他条目。
- **整类范围与限定记录范围在结果中可区分**：`Scopes` 非空表示整类范围
  （某就诊下某类别的**全部**已生效记录，含授权后才生效者），`Selections`
  非空表示限定记录范围（只含明确选出的记录）；两者可以同时非空，表示同一条
  授权两种范围并存。列表原样保留这种区别，不会把限定范围改写成整类范围。
- **`ActiveAt(t)` 只判断授权自身**：它只看该授权的时间窗与撤回状态——已撤回
  恒为无效；未撤回时采用**包含开始时刻、不包含截止时刻**的半开区间
  `[StartsAt, ExpiresAt)`：`t == StartsAt` 有效，`t == ExpiresAt` 已失效。它
  不判断患者是否停用，也不判断接收方是谁；能否读取临床内容仍须以实际 `Read`
  /`FetchPackage` 的结果为准。
- **患者停用后仍可列出历史授权**：停用不清空、不拒绝此查询，撤回时间等历史
  字段照样返回；但接收方此时读取或取包一律 `ErrAccessDenied`。
- **两种失败**：
  - 内部使用者查询**不存在的患者**（无论是否指定接收方）返回 `ErrNotFound`，
    不带回任何条目。
  - **接收方不能调用这个内部查询**：即使该接收方持有该患者当前有效的授权，
    也返回 `ErrAccessDenied`，结果为空、不提供任何授权资料——按自己的标识
    过滤同样被拒。查看清单本身是只读操作，不新增授权或审计。

下面的完整示例位于
[`examples/list_authorizations`](examples/list_authorizations/main.go)
（`go run ./examples/list_authorizations`）。它自行准备本地存储、内部使用者、
两名接收方、一名合成患者与一次就诊（两条已生效诊断、一条已生效医嘱），由同一
患者向两个接收方建立 4 条授权——当前有效的整类授权、尚未开始的限定授权、已经
到期的整类授权、建立后即被撤回的限定授权；再登记另一名也授权给同一接收方的
合成患者与一名从无授权的合成患者。示例展示不指定接收方与指定接收方的清单差异、
撤回授权仍在清单中、清单不能替代实际读取、跨患者不混入、成功的空清单，以及
`ErrNotFound` 与接收方 `ErrAccessDenied` 两种失败，最后停用患者对比“内部仍可
列出 / 接收方读取被拒”。示例对每一步业务调用都检查错误：任一准备步骤失败时由
`must` 明确指出失败的操作并立即终止，不把失败返回值当作正式患者、记录或授权
继续使用：

```go
// 命令 list_authorizations 演示内部使用者如何用 ListAuthorizations 查看某名
// 合成患者已保存的授权清单。
//
// 清单是“历史视图”，不是“接收方此刻能读什么”：同一名患者授予两个接收方的
// 当前有效、尚未开始、已经到期与已经撤回的授权都会原样列出，每条保留各自的
// 整类范围、限定记录范围、时间窗与撤回时间。示例同时演示：
//   - 只按患者查询（接收方参数传空字符串）与再指定接收方的差异；
//   - 另一名患者授权给同一个接收方不会混入本患者清单；
//   - 患者存在但没有授权、指定接收方未获该患者授权，都是成功返回空清单；
//   - 清单里有授权不代表接收方此刻能读取（未开始的授权仍读取被拒）；
//   - 两种失败：内部使用者查询不存在的患者得到 ErrNotFound；接收方即使持有
//     有效授权也不能调用这个内部查询，得到 ErrAccessDenied 且无授权资料；
//   - 患者停用后内部使用者仍能列出历史授权，接收方读取则被拒绝。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/list_authorizations
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-list-auth-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	insurer1 := clinical.ReceiverActor("insurer-1")
	insurer2 := clinical.ReceiverActor("insurer-2")
	insurer3 := clinical.ReceiverActor("insurer-3")

	now := time.Now()

	// ---- 准备合成患者、一次就诊与已生效记录 ----
	patient, err := store.RegisterPatient(doctor, "合成患者戊")
	must("登记合成患者戊", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, now)
	must("登记就诊", err)

	diag1, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断一：高血压 I10")
	must("创建第一条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag1.ID)
	must("生效第一条诊断", err)

	diag2, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断二：2 型糖尿病 E11")
	must("创建第二条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag2.ID)
	must("生效第二条诊断", err)

	order1, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Order, "合成医嘱一：空腹血糖检测")
	must("创建医嘱草稿", err)
	_, err = store.ActivateRecord(doctor, order1.ID)
	must("生效医嘱", err)

	// ---- 同一患者向两个接收方建立 4 条授权，覆盖四种生命周期状态 ----

	// 授权一：insurer-1，整类范围（该就诊全部诊断），时间窗覆盖 now，当前有效。
	grantActive, err := store.Grant(doctor, patient.ID, insurer1.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
		now.Add(-time.Hour), now.Add(24*time.Hour))
	must("建立授权一（当前有效·整类诊断）", err)

	// 授权二：insurer-2，限定记录范围（只选第二条诊断），时间窗尚未开始。
	grantFuture, err := store.GrantSelective(doctor, patient.ID, insurer2.ID, nil,
		[]clinical.RecordSelection{{
			EncounterID: encounter.ID,
			Category:    clinical.Diagnosis,
			RecordID:    diag2.ID,
		}},
		now.Add(24*time.Hour), now.Add(48*time.Hour))
	must("建立授权二（尚未开始·限定诊断）", err)

	// 授权三：insurer-2，整类范围（该就诊全部医嘱），时间窗已经到期。
	grantExpired, err := store.Grant(doctor, patient.ID, insurer2.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Order}},
		now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	must("建立授权三（已到期·整类医嘱）", err)

	// 授权四：insurer-1，限定记录范围（只选第一条诊断），时间窗覆盖 now，
	// 建立后由内部使用者撤回——撤回后它仍应出现在清单里。
	grantRevoked, err := store.GrantSelective(doctor, patient.ID, insurer1.ID, nil,
		[]clinical.RecordSelection{{
			EncounterID: encounter.ID,
			Category:    clinical.Diagnosis,
			RecordID:    diag1.ID,
		}},
		now.Add(-time.Hour), now.Add(24*time.Hour))
	must("建立授权四（撤回前·限定诊断）", err)
	must("撤回授权四", store.Revoke(doctor, patient.ID, grantRevoked.ID))

	labels := map[clinical.ID]string{
		grantActive.ID:  "授权一（当前有效·整类诊断）",
		grantFuture.ID:  "授权二（尚未开始·限定诊断）",
		grantExpired.ID: "授权三（已到期·整类医嘱）",
		grantRevoked.ID: "授权四（已撤回·限定诊断）",
	}
	fmt.Println("准备完成: 1 名合成患者、1 次就诊、2 条已生效诊断与 1 条已生效医嘱；" +
		"建立 4 条授权（当前有效、尚未开始、已到期、已撤回各 1 条）")

	// ---- 查询一：只按患者查询，接收方参数为空字符串，列出授予所有接收方的授权 ----
	allRows := printListing(store, doctor, patient.ID, "", labels, now,
		"查询一：只按患者查询")

	// ---- 查询二/三：再指定接收方，只保留授予该接收方的条目 ----
	rowsInsurer1 := printListing(store, doctor, patient.ID, insurer1.ID, labels, now,
		"查询二：再指定接收方")
	rowsInsurer2 := printListing(store, doctor, patient.ID, insurer2.ID, labels, now,
		"查询三：再指定接收方")

	// 核对：条数、患者归属、撤回授权在“全部”与“insurer-1”两张清单中都出现。
	belongsToPatient := func(rows []clinical.Authorization) bool {
		for _, a := range rows {
			if a.PatientID != patient.ID {
				return false
			}
		}
		return true
	}
	listHas := func(rows []clinical.Authorization, id clinical.ID) bool {
		for _, a := range rows {
			if a.ID == id {
				return true
			}
		}
		return false
	}
	shapeOK := len(allRows) == 4 && len(rowsInsurer1) == 2 && len(rowsInsurer2) == 2 &&
		belongsToPatient(allRows) && belongsToPatient(rowsInsurer1) && belongsToPatient(rowsInsurer2) &&
		listHas(allRows, grantRevoked.ID) && listHas(rowsInsurer1, grantRevoked.ID)
	fmt.Printf("清单核对: 不指定接收方 %d 条、insurer-1 %d 条、insurer-2 %d 条；"+
		"各行归属均为患者本人=%v；撤回授权在“全部”与“insurer-1”两张清单中都出现=%v\n",
		len(allRows), len(rowsInsurer1), len(rowsInsurer2),
		shapeOK && idsAscending(allRows), listHas(allRows, grantRevoked.ID) && listHas(rowsInsurer1, grantRevoked.ID))

	// ---- 清单里有授权，不等于接收方此刻能读取临床内容 ----
	// insurer-2 名下有 2 条授权（未开始的限定诊断、已到期的整类医嘱），
	// 但此刻读取诊断仍被拒绝：是否能读要在实际读取时按授权状态重新判断。
	if _, err := store.Read(insurer2, patient.ID, encounter.ID, clinical.Diagnosis); !errors.Is(err, clinical.ErrAccessDenied) {
		die("未开始授权不应允许读取，实际得到 %v", err)
	}
	fmt.Printf("对照读取: insurer-2 清单里有 %d 条授权，但此刻读取诊断仍被拒绝（%v）"+
		"——列出授权不等于此刻能读取临床内容\n", len(rowsInsurer2), clinical.ErrAccessDenied)

	// ---- 另一名患者也授权给同一个接收方，其条目不混入本患者清单 ----
	patient2, err := store.RegisterPatient(doctor, "合成患者己")
	must("登记合成患者己", err)
	encounter2, err := store.AddEncounter(doctor, patient2.ID, now)
	must("登记患者己的就诊", err)
	otherDraft, err := store.CreateDraft(doctor, patient2.ID, encounter2.ID,
		clinical.Diagnosis, "合成诊断：患者己的感冒 J00")
	must("创建患者己的诊断草稿", err)
	_, err = store.ActivateRecord(doctor, otherDraft.ID)
	must("生效患者己的诊断", err)
	grant2, err := store.Grant(doctor, patient2.ID, insurer1.ID,
		[]clinical.Scope{{EncounterID: encounter2.ID, Category: clinical.Diagnosis}},
		now.Add(-time.Hour), now.Add(24*time.Hour))
	must("为患者己建立授权", err)

	rowsB, err := store.ListAuthorizations(doctor, patient2.ID, insurer1.ID)
	must("查询患者己的清单", err)
	rowsInsurer1Again, err := store.ListAuthorizations(doctor, patient.ID, insurer1.ID)
	must("再次查询患者戊/insurer-1 的清单", err)
	noLeakAcrossPatients := len(rowsB) == 1 && rowsB[0].ID == grant2.ID &&
		rowsB[0].PatientID == patient2.ID &&
		len(rowsInsurer1Again) == 2 && sameIDSet(rowsInsurer1Again, rowsInsurer1)
	fmt.Printf("同一接收方跨患者不混入: 患者己/insurer-1 清单只有患者己自己的 1 条授权=%v；"+
		"再查患者戊/insurer-1 仍为此前 %d 条=%v\n",
		len(rowsB) == 1 && rowsB[0].ID == grant2.ID, len(rowsInsurer1),
		len(rowsInsurer1Again) == 2 && sameIDSet(rowsInsurer1Again, rowsInsurer1) && noLeakAcrossPatients)

	// ---- 成功的空清单：患者存在但没有授权 / 指定接收方未获该患者授权 ----
	patient3, err := store.RegisterPatient(doctor, "合成患者庚")
	must("登记从无授权的合成患者庚", err)
	emptyNoGrant, err := store.ListAuthorizations(doctor, patient3.ID, "")
	must("查询从无授权的患者", err)
	emptyStranger, err := store.ListAuthorizations(doctor, patient.ID, insurer3.ID)
	must("查询未获该患者授权的接收方", err)
	fmt.Printf("成功的空清单: 从无授权的患者=%d 条、患者戊搭配未获其授权的接收方 insurer-3=%d 条"+
		"（两次都成功返回，不是患者不存在，也不是被拒绝）\n",
		len(emptyNoGrant), len(emptyStranger))

	// ---- 失败一：内部使用者查询不存在的患者 ----
	missingRows, missingErr := store.ListAuthorizations(doctor, "pat_does_not_exist", "")
	if !errors.Is(missingErr, clinical.ErrNotFound) || len(missingRows) != 0 {
		die("查询不存在患者应返回 0 条与 ErrNotFound，实际得到 %d 条、%v",
			len(missingRows), missingErr)
	}
	fmt.Printf("失败一（内部使用者查询不存在的患者）: %v，返回条数=%d（ErrNotFound）\n",
		missingErr, len(missingRows))

	// ---- 失败二：接收方即使持有有效授权，也不能调用这个内部查询 ----
	deniedRows, deniedErr := store.ListAuthorizations(insurer1, patient.ID, "")
	deniedSelfRows, deniedSelfErr := store.ListAuthorizations(insurer1, patient.ID, insurer1.ID)
	if !errors.Is(deniedErr, clinical.ErrAccessDenied) || len(deniedRows) != 0 ||
		!errors.Is(deniedSelfErr, clinical.ErrAccessDenied) || len(deniedSelfRows) != 0 {
		die("接收方调用内部查询应返回 0 条与 ErrAccessDenied，实际得到 %d 条/%v 与 %d 条/%v",
			len(deniedRows), deniedErr, len(deniedSelfRows), deniedSelfErr)
	}
	fmt.Printf("失败二（持有有效授权的接收方调用内部查询）: 不指定接收方与只过滤自己都被拒绝"+
		"（%v），返回条数=%d/%d（ErrAccessDenied，不提供任何授权资料）\n",
		clinical.ErrAccessDenied, len(deniedRows), len(deniedSelfRows))

	// ---- 患者停用后：内部清单仍是完整历史，接收方读取则被拒绝 ----
	must("停用患者戊", store.DeactivatePatient(doctor, patient.ID))
	afterDeactivate, err := store.ListAuthorizations(doctor, patient.ID, insurer1.ID)
	must("停用后内部使用者查询清单", err)
	storedRevoked, err := store.GetAuthorization(doctor, patient.ID, grantRevoked.ID)
	must("停用后取回已撤回授权", err)
	listedRevoked := findAuthorization(afterDeactivate, grantRevoked.ID)
	revokedPreserved := listedRevoked != nil && listedRevoked.RevokedAt != nil &&
		listedRevoked.RevokedAt.Equal(*storedRevoked.RevokedAt)
	fmt.Printf("患者停用后: 内部清单仍成功（insurer-1 条数=%d），已撤回授权仍带原撤回时间出现=%v\n",
		len(afterDeactivate), revokedPreserved)

	if _, err := store.Read(insurer1, patient.ID, encounter.ID, clinical.Diagnosis); !errors.Is(err, clinical.ErrAccessDenied) {
		die("患者停用后接收方读取应被拒绝，实际得到 %v", err)
	}
	fmt.Printf("患者停用后: 接收方读取被拒绝（%v）——清单不能替代实际读取时的权限检查\n",
		clinical.ErrAccessDenied)
}

// printListing 执行一次清单查询并打印结果。清单本身按授权标识升序返回；
// 展示时为了输出稳定可读，改按（接收方、生命周期、说明标签）重排，并单独
// 报告“按授权标识升序”的核对结果。
func printListing(store *clinical.Store, actor clinical.Actor, patientID clinical.ID,
	receiverArg string, labels map[clinical.ID]string, now time.Time, title string,
) []clinical.Authorization {
	rows, err := store.ListAuthorizations(actor, patientID, receiverArg)
	must(title, err)
	fmt.Printf("%s（接收方参数=%q）: 条数=%d，错误=%v，按授权标识升序=%v\n",
		title, receiverArg, len(rows), err, idsAscending(rows))
	for _, a := range rowsSortedForDisplay(rows, labels, now) {
		printAuthorizationRow(a, labels[a.ID], now)
	}
	return rows
}

func printAuthorizationRow(a clinical.Authorization, label string, now time.Time) {
	// 时间窗状态只描述授权自身的 [StartsAt, ExpiresAt)，不考虑撤回；
	// 撤回状态与 ActiveAt 单独展示。
	windowState := "在时间窗内"
	switch {
	case now.Before(a.StartsAt):
		windowState = "尚未开始"
	case !now.Before(a.ExpiresAt):
		windowState = "已到期"
	}
	revoked := "false"
	if a.RevokedAt != nil {
		revoked = fmt.Sprintf("true（撤回时间已保留=%v）", !a.RevokedAt.IsZero())
	}
	fmt.Printf("  - %s：接收方=%s，整类范围 %d 项（%s），限定记录 %d 条（%s）；"+
		"时间窗状态=%s；已撤回=%s；ActiveAt(now)=%v\n",
		label, a.ReceiverID,
		len(a.Scopes), categoryList(scopeCategories(a.Scopes)...),
		len(a.Selections), categoryList(selectionCategories(a.Selections)...),
		windowState, revoked, a.ActiveAt(now))
}

// rowsSortedForDisplay 返回按展示键排序的副本，不改动查询返回的顺序。
func rowsSortedForDisplay(rows []clinical.Authorization, labels map[clinical.ID]string, now time.Time) []clinical.Authorization {
	out := append([]clinical.Authorization(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := displayRank(out[i], now), displayRank(out[j], now)
		if out[i].ReceiverID != out[j].ReceiverID {
			return out[i].ReceiverID < out[j].ReceiverID
		}
		if ri != rj {
			return ri < rj
		}
		return labels[out[i].ID] < labels[out[j].ID]
	})
	return out
}

// displayRank 仅用于展示排序：当前有效 0、尚未开始 1、已到期 2、已撤回 3。
func displayRank(a clinical.Authorization, now time.Time) int {
	if a.RevokedAt != nil {
		return 3
	}
	switch {
	case now.Before(a.StartsAt):
		return 1
	case !now.Before(a.ExpiresAt):
		return 2
	default:
		return 0
	}
}

func idsAscending(rows []clinical.Authorization) bool {
	for i := 1; i < len(rows); i++ {
		if rows[i-1].ID >= rows[i].ID {
			return false
		}
	}
	return true
}

func sameIDSet(a, b []clinical.Authorization) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[clinical.ID]bool{}
	for _, x := range a {
		seen[x.ID] = true
	}
	for _, x := range b {
		if !seen[x.ID] {
			return false
		}
	}
	return true
}

func findAuthorization(rows []clinical.Authorization, id clinical.ID) *clinical.Authorization {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}

func scopeCategories(sc []clinical.Scope) []string {
	cats := make([]string, 0, len(sc))
	for _, s := range sc {
		cats = append(cats, s.Category)
	}
	return cats
}

func selectionCategories(sel []clinical.RecordSelection) []string {
	cats := make([]string, 0, len(sel))
	for _, s := range sel {
		cats = append(cats, s.Category)
	}
	return cats
}

// categoryList 把类别汇总成稳定的“diagnosis×1、order×2”样式；空列表显示“无”。
func categoryList(cats ...string) string {
	count := map[string]int{}
	for _, c := range cats {
		count[c]++
	}
	var parts []string
	for _, c := range []string{clinical.Diagnosis, clinical.Order} {
		if count[c] > 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", c, count[c]))
		}
	}
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, "、")
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

结果说明（对照输出；授权标识由存储随机生成，输出用建立时的说明标签指代，
因此每次运行文本一致）：

```text
准备完成: 1 名合成患者、1 次就诊、2 条已生效诊断与 1 条已生效医嘱；建立 4 条授权（当前有效、尚未开始、已到期、已撤回各 1 条）
查询一：只按患者查询（接收方参数=""）: 条数=4，错误=<nil>，按授权标识升序=true
  - 授权一（当前有效·整类诊断）：接收方=insurer-1，整类范围 1 项（diagnosis×1），限定记录 0 条（无）；时间窗状态=在时间窗内；已撤回=false；ActiveAt(now)=true
  - 授权四（已撤回·限定诊断）：接收方=insurer-1，整类范围 0 项（无），限定记录 1 条（diagnosis×1）；时间窗状态=在时间窗内；已撤回=true（撤回时间已保留=true）；ActiveAt(now)=false
  - 授权二（尚未开始·限定诊断）：接收方=insurer-2，整类范围 0 项（无），限定记录 1 条（diagnosis×1）；时间窗状态=尚未开始；已撤回=false；ActiveAt(now)=false
  - 授权三（已到期·整类医嘱）：接收方=insurer-2，整类范围 1 项（order×1），限定记录 0 条（无）；时间窗状态=已到期；已撤回=false；ActiveAt(now)=false
查询二：再指定接收方（接收方参数="insurer-1"）: 条数=2，错误=<nil>，按授权标识升序=true
  - 授权一（当前有效·整类诊断）：接收方=insurer-1，整类范围 1 项（diagnosis×1），限定记录 0 条（无）；时间窗状态=在时间窗内；已撤回=false；ActiveAt(now)=true
  - 授权四（已撤回·限定诊断）：接收方=insurer-1，整类范围 0 项（无），限定记录 1 条（diagnosis×1）；时间窗状态=在时间窗内；已撤回=true（撤回时间已保留=true）；ActiveAt(now)=false
查询三：再指定接收方（接收方参数="insurer-2"）: 条数=2，错误=<nil>，按授权标识升序=true
  - 授权二（尚未开始·限定诊断）：接收方=insurer-2，整类范围 0 项（无），限定记录 1 条（diagnosis×1）；时间窗状态=尚未开始；已撤回=false；ActiveAt(now)=false
  - 授权三（已到期·整类医嘱）：接收方=insurer-2，整类范围 1 项（order×1），限定记录 0 条（无）；时间窗状态=已到期；已撤回=false；ActiveAt(now)=false
清单核对: 不指定接收方 4 条、insurer-1 2 条、insurer-2 2 条；各行归属均为患者本人=true；撤回授权在“全部”与“insurer-1”两张清单中都出现=true
对照读取: insurer-2 清单里有 2 条授权，但此刻读取诊断仍被拒绝（clinical: access denied）——列出授权不等于此刻能读取临床内容
同一接收方跨患者不混入: 患者己/insurer-1 清单只有患者己自己的 1 条授权=true；再查患者戊/insurer-1 仍为此前 2 条=true
成功的空清单: 从无授权的患者=0 条、患者戊搭配未获其授权的接收方 insurer-3=0 条（两次都成功返回，不是患者不存在，也不是被拒绝）
失败一（内部使用者查询不存在的患者）: clinical: referenced object not found: patient "pat_does_not_exist"，返回条数=0（ErrNotFound）
失败二（持有有效授权的接收方调用内部查询）: 不指定接收方与只过滤自己都被拒绝（clinical: access denied），返回条数=0/0（ErrAccessDenied，不提供任何授权资料）
患者停用后: 内部清单仍成功（insurer-1 条数=2），已撤回授权仍带原撤回时间出现=true
患者停用后: 接收方读取被拒绝（clinical: access denied）——清单不能替代实际读取时的权限检查
```

- **两张清单的差异**：不指定接收方时，患者授予 insurer-1 与 insurer-2 的
  4 条授权全部返回；指定 `insurer-1` 只剩其名下 2 条，指定 `insurer-2` 只剩
  其名下 2 条。每张清单都核对为按授权标识升序（示例展示时另按接收方与状态
  重排，仅为输出易读）。
- **四种状态都在、范围形态各自保留**：授权一在时间窗内未撤回，
  `ActiveAt(now)=true`；授权二尚未开始、授权三已到期、授权四已撤回，
  `ActiveAt(now)` 均为 `false`。整类授权显示为“整类范围 1 项、限定记录
  0 条”，限定授权相反；授权四撤回后仍出现在“全部”和“insurer-1”两张清单中，
  并带有已保存的撤回时间。
- **列出不等于能读**：insurer-2 清单里有 2 条授权，但其中一条尚未开始、
  一条已到期，此刻读取诊断仍得到 `ErrAccessDenied`。
- **跨患者不混入**：患者己也向 insurer-1 授权后，查患者己只看到自己的 1 条；
  回头再查患者戊/insurer-1 仍是原来的 2 条，同一接收方不会把两名患者的条目
  带到一起。
- **成功的空清单**：从无授权的患者、患者搭配未获其授权的接收方，都成功返回
  0 条；只有内部使用者查不存在的患者才是 `ErrNotFound`。
- **接收方被拒绝**：insurer-1 持有患者当前有效的整类授权，但无论是否按自己
  过滤，调用 `ListAuthorizations` 都得到 `ErrAccessDenied` 且 0 条结果，不
  提供任何授权资料。
- **停用后**：内部使用者列出的历史授权不缺不损（撤回时间与保存值一致），而
  接收方读取同一范围被拒绝——清单只是历史视图，读取权限以实际读取时的检查
  为准。

本次只补齐 `ListAuthorizations` 的使用说明与示例：授权建立、撤回、接收方
读取、交换与回执等已有公开行为与示例均保持不变。

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

`FetchPackage` 成功返回的 `PackageDelivery` 同时含有两样东西：

- `Digest`：**服务摘要**。创建交换时按当时固化的包内容算出、保存在交换上；
  取包只原样返回它，不会重算，授权或回执的任何变化都不改写它。
- `Package`：**包内容**。患者与接收方标识，加上每条记录创建交换时的当前
  生效版本快照。

接收方要确认“收到的内容与摘要一致”，应当**在本地完全根据 `Package` 按相同
规则重算一遍摘要**，再与 `Digest` 逐字（区分大小写）比较。这一计算不访问
存储、不需要授权、不需要任何内部身份，随时可对留存的交付离线复算。核对所需
材料只有取包得到的包内容本身；**患者姓名、草稿、旧版本、更正原因都不在
包内**（接收方本来也看不到），不是核对所需材料。

### 第一步：哪些包内数据参与摘要

参与摘要的是一个字段顺序固定的紧凑 JSON 文档。顶层三个字段，顺序固定：

| 顺序 | JSON 字段 | 类型 | 取自 |
| --- | --- | --- | --- |
| 1 | `patient_id` | 字符串 | `Package.PatientID` |
| 2 | `receiver_id` | 字符串 | `Package.ReceiverID` |
| 3 | `records` | 数组（不为空） | `Package.Records` |

`records` 的每个元素七个字段，顺序同样固定：

| 顺序 | JSON 字段 | 类型 | 取自 `PackagedRecord` |
| --- | --- | --- | --- |
| 1 | `encounter_id` | 字符串 | 就诊标识 |
| 2 | `category` | 字符串 | `"diagnosis"` 或 `"order"` |
| 3 | `record_id` | 字符串 | 记录标识 |
| 4 | `version_id` | 字符串 | 固化版本的版本标识 |
| 5 | `version` | **JSON 整数（无引号）** | 版本号（第 1 版为 `1`） |
| 6 | `effective_at` | 字符串 | 生效时间，见下 |
| 7 | `content` | 字符串 | 该版本的完整正文，逐字参与 |

除此之外的任何数据都不参与摘要。注意：`version_id`（版本标识）与 `version`
（版本号）是两个独立字段；包内固化的是创建交换时的当前生效版本，此后记录被
更正不影响已创建的包。

### 第二步：排列、编码、转义与时间表示

- **记录排列**：序列化前先把记录按 `record_id` 的字符串顺序升序排列（标识为
  ASCII，即逐字节字典序）。因此摘要与 `CreateExchange` 的入参顺序、与取包
  交付中记录的排列顺序都无关；**仅改变记录排列顺序不改变摘要，不代表内容
  被改动**。
- **文档形态**：UTF-8、紧凑 JSON——字段间与数组元素间没有任何额外空格或
  换行；键顺序固定如上；不用 Unicode 转义给中文转义。
- **字符串转义**：等价于 Go `encoding/json` 以默认设置 `Marshal`：
  - 半角双引号 `"` 转义为 `\"`，反斜杠 `\` 转义为 `\\`；
  - 换行转义为 `\n`，另有回车 `\r`、制表符 `\t`、退格 `\b`、换页 `\f`，
    其余控制字符按 `\u00xx` 转义；
  - **默认 HTML 转义保持开启**：可打印的小于号、大于号、与号也要写成
    `<` → `\u003c`、`>` → `\u003e`、`&` → `\u0026`，手工构造核对字节时
    这一处最容易遗漏；
  - 中文（含全角标点）等非 ASCII 字符以 UTF-8 原样出现，例如
    `2型糖尿病（E11.9）`。
- **时间表示**：`effective_at` 先把时刻转到 UTC，再按 Go 的
  `time.RFC3339Nano`（布局 `2006-01-02T15:04:05.999999999Z07:00`）格式化：
  小数秒至多 9 位并去掉尾随的零（`123456000` 纳秒写作 `.123456`），恰好整秒
  则没有小数部分，UTC 结果以 `Z` 结尾，例如
  `2026-03-14T09:07:05.123456789Z`。
- **时间按“时刻”而非“字面表示”参与摘要**：同一绝对时刻换时区写法
  （如 `2026-03-14T18:07:05.123456789+09:00` 与
  `2026-03-14T09:07:05.123456789Z`）经 UTC 归并后完全相同，摘要不变——
  **同一时刻的时区表示差异不是内容改动**；但相差一纳秒、或把小数秒截断成
  整秒，都是不同时刻，摘要必然变化。
- **摘要输出格式**：对上述规范字节序列计算 SHA-256，编码为 **64 个小写
  十六进制字符**（正则 `^[0-9a-f]{64}$`）。`PackageDelivery.Digest` 与
  `Exchange.Digest` 就是这个字符串。

### 第三步：区分包内容与包外信息

下列信息**不参与摘要**，它们的存在或变化既不改变摘要，也不是接收方必须取得
的核对材料：

- **交换标识** `ExchangeID`、**请求号**、**创建交换的内部使用者标识**、
  **绑定授权标识**、**交换创建时间**——同一份包内容用不同请求号、不同发起
  者或另一条等价授权各创建一次，交换互不相同但摘要相同。
- **当前状态**（`pending_receipt`/`accepted`/`rejected`）与**回执**（接受/
  拒绝结果、拒绝原因、登记时间）：登记回执只改变状态与回执，摘要与包内容
  保持创建时的原值。
- **患者姓名**从不进入包；**草稿、旧版本、更正原因**（`Version.Reason`）
  在创建交换时就不进包，接收方无法也无需取得它们来核对。

因此：交换状态变化、回执变化、授权到期或撤回，都不会让同一个包的摘要发生
变化；不能把交换标识、状态、回执、患者姓名、草稿、旧版本或更正原因写成
“接收方核对必须取得的材料”。

### 完整手工示例：诊断 + 医嘱合成包

一份合成包含两条记录（就诊相同；刻意让记录标识、版本标识、版本号互不相同，
生效时间带 9 位小数秒）：

| 字段 | 诊断记录 | 医嘱记录 |
| --- | --- | --- |
| `encounter_id` | `enc_digest_golden_01` | `enc_digest_golden_01` |
| `category` | `diagnosis` | `order` |
| `record_id` | `rec_diag_golden` | `rec_ord_golden` |
| `version_id` | `ver_diag_golden_2nd` | `ver_ord_golden_7th` |
| `version` | `2` | `7` |
| `effective_at` | `2026-03-14T09:07:05.123456789Z` | `2026-03-14T09:07:05.123456789Z` |

两条正文（Go 字面量写法；其中 `\n` 是真实换行、`\"` 是真实半角双引号、
`\\` 是真实反斜杠，`<` 与 `&` 是真实字符）：

```go
"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a<b 与 c&d；目录 C:\\病历"
"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量<10IU 需复核\"；配伍 5%GS&0.9%NS；路径 C:\\泵注"
```

记录按 `record_id` 排序（`rec_diag_golden` 在 `rec_ord_golden` 之前），参与
摘要的**规范字节**确定为（一整行，没有换行；`<`、`&` 已按默认 HTML 转义
写成 `\u003c`、`\u0026`，真实换行写成 `\n`）：

```text
{"patient_id":"pat_digest_golden_01","receiver_id":"rcv_digest_golden","records":[{"encounter_id":"enc_digest_golden_01","category":"diagnosis","record_id":"rec_diag_golden","version_id":"ver_diag_golden_2nd","version":2,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a\u003cb 与 c\u0026d；目录 C:\\病历"},{"encounter_id":"enc_digest_golden_01","category":"order","record_id":"rec_ord_golden","version_id":"ver_ord_golden_7th","version":7,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量\u003c10IU 需复核\"；配伍 5%GS\u00260.9%NS；路径 C:\\泵注"}]}
```

任何读者都可以不依赖本库，直接用 `sha256sum` 复算（单引号内原样粘贴上面的
规范字节；`printf '%s'` 保证末尾不另加换行，换行只能来自字节内的 `\n`）：

```bash
printf '%s' '{"patient_id":"pat_digest_golden_01","receiver_id":"rcv_digest_golden","records":[{"encounter_id":"enc_digest_golden_01","category":"diagnosis","record_id":"rec_diag_golden","version_id":"ver_diag_golden_2nd","version":2,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a\u003cb 与 c\u0026d；目录 C:\\病历"},{"encounter_id":"enc_digest_golden_01","category":"order","record_id":"rec_ord_golden","version_id":"ver_ord_golden_7th","version":7,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量\u003c10IU 需复核\"；配伍 5%GS\u00260.9%NS；路径 C:\\泵注"}]}' | sha256sum
# f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87  -
```

**期望摘要（确定值）**：
`f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87`。即使两条
记录以“医嘱在前”传入、或把生效时间写成同一时刻的 `+09:00` 表示
（`2026-03-14T18:07:05.123456789+09:00`），规范字节与摘要都不变。

**只改动收到的正文**：把诊断正文最后一个汉字“历”改成“厘”，其余字段一律
不动，重算得到另一个确定摘要
`94a2ce6018d932de308ccabea9fd05aabe65087b034c5be87926c43e38b49a99`，与原服务
摘要**不一致**；而原先保存的服务摘要 `f790315c…` 仍在、仍是唯一的比较依据。

### 完整可运行示例

下面的完整示例位于
[`examples/verify_digest`](examples/verify_digest/main.go)
（`go run ./examples/verify_digest`）。它先由内部使用者准备含一条诊断与一条
医嘱的合成包（正文含中文、换行、引号、反斜杠、`<`、`&`，生效时间保留 9 位
小数秒），授权、创建交换后由指定接收方取包；接收方只用交付内容在本地重算
摘要，演示正常输入核对一致、仅改正文一个字后核对不一致（服务摘要不变）、
记录重排与同时区表示变化不影响核对，并紧接着用同一交换演示
`SubmitReceipt` 与本地核对的关系。示例对每一步都检查错误：

```go
// 命令 verify_digest 演示接收方取包后如何核对“收到的内容”与“服务摘要”
// 是否一致。
//
// 现有交换功能只要求取包后保留摘要、在回执中原样回传，却没有交代接收方
// 怎样根据收到的包确认内容与摘要对得上。本示例完全站在接收方一侧：只用
// 取包得到的 PackageDelivery（交换标识、服务摘要、包内容），按与服务端
// 相同的公开规则自行重算摘要并逐字比较——不需要任何内部身份，也不需要
// 患者姓名、草稿、旧版本或更正原因（它们根本不在包内）。
//
// 演示分两部分：
//
//  1. 完整调用链（登记→生效→授权→创建交换→取包→本地核对→回执）：
//     正常收到的包核对一致；只改动收到的正文一个字后核对不一致，原先
//     保存的服务摘要仍在、仍是比较依据；用被改摘要登记回执得到
//     ErrConflict，用保存的服务摘要登记才成功。
//  2. 固定核对向量：用标识固定的合成包给出确定的规范 JSON 与确定摘要，
//     读者可照 README 用 sha256sum 手工复现；记录重排、同一时刻换时区
//     表示都不改变摘要。
//
// 本地核对只是接收方自己的检查：SubmitReceipt 只比较“提交摘要”与
// “交换保存的摘要”，摘要不符返回 ErrConflict，并不重新检查接收方手中
// 的临床内容。本地核对成功只说明手中内容与取包得到的服务摘要一致；
// 要完成这次交换，仍须按既有方式提交回执。
//
// 全程只使用合成患者资料，生效时间保留 9 位小数秒，正文含中文、换行、
// 引号、反斜杠、小于号与与号。运行：
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
	"strings"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

// 固定的小数秒生效时刻（UTC），以及同一时刻的 +09:00 写法。
var (
	fixedEffectiveUTC  = time.Date(2026, 3, 14, 9, 7, 5, 123456789, time.UTC)
	fixedEffectiveZone = time.FixedZone("UTC+9", 9*60*60)
)

// 诊断与医嘱正文：均含中文、半角双引号、反斜杠、小于号、与号与换行。
const (
	fixedDiagContent  = "2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a<b 与 c&d；目录 C:\\病历"
	fixedOrderContent = "医嘱：胰岛素 8IU（餐前）\n注意 \"剂量<10IU 需复核\"；配伍 5%GS&0.9%NS；路径 C:\\泵注"
)

// 固定向量标识。
const (
	fixedPatientID   = "pat_digest_golden_01"
	fixedReceiverID  = "rcv_digest_golden"
	fixedEncounterID = "enc_digest_golden_01"
	fixedDiagRecID   = "rec_diag_golden"
	fixedDiagVerID   = "ver_diag_golden_2nd"
	fixedOrderRecID  = "rec_ord_golden"
	fixedOrderVerID  = "ver_ord_golden_7th"
)

// wantFixedCanonicalJSON 是固定向量参与摘要的精确规范字节（记录按
// record_id 排序后诊断在医嘱之前）。
const wantFixedCanonicalJSON = `{"patient_id":"pat_digest_golden_01","receiver_id":"rcv_digest_golden","records":[{"encounter_id":"enc_digest_golden_01","category":"diagnosis","record_id":"rec_diag_golden","version_id":"ver_diag_golden_2nd","version":2,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a\u003cb 与 c\u0026d；目录 C:\\病历"},{"encounter_id":"enc_digest_golden_01","category":"order","record_id":"rec_ord_golden","version_id":"ver_ord_golden_7th","version":7,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量\u003c10IU 需复核\"；配伍 5%GS\u00260.9%NS；路径 C:\\泵注"}]}`

// 两个期望摘要均可用 sha256sum 对上面的规范字节独立复算（被改向量只
// 改动诊断正文最后一个汉字“历”→“厘”），不是抄自产品的计算结果。
const (
	wantFixedDigest         = "f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87"
	wantTamperedFixedDigest = "94a2ce6018d932de308ccabea9fd05aabe65087b034c5be87926c43e38b49a99"
)

var tamperedDiagContent = strings.Replace(fixedDiagContent, "历", "厘", 1)

func main() {
	runEndToEnd()
	runFixedVector()
}

// ---- 第一部分：真实调用链（包标识由存储随机生成，比较结论每次运行确定）----

func runEndToEnd() {
	dir, err := os.MkdirTemp("", "clinical-verify-digest-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	// 注入固定时钟，让两条记录在固定的小数秒时刻生效，便于对照时间字段；
	// 生产环境使用默认实时时钟，摘要规则完全相同。
	clock := fixedEffectiveUTC
	store, err := clinical.Open(dir, clinical.WithClock(func() time.Time { return clock }))
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor(fixedReceiverID)

	// 内部使用者准备一份“诊断 + 医嘱”的合成包（这部分只是包的来源，
	// 接收方核对时只认取包拿到的内容）。
	patient, err := store.RegisterPatient(doctor, "合成患者丙")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID,
		time.Date(2026, 3, 14, 6, 0, 0, 0, time.UTC))
	must("登记就诊", err)

	diagDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID, clinical.Diagnosis, fixedDiagContent)
	must("创建诊断草稿", err)
	if _, err := store.ActivateRecord(doctor, diagDraft.ID); err != nil {
		die("生效诊断: %v", err)
	}
	orderDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID, clinical.Order, fixedOrderContent)
	must("创建医嘱草稿", err)
	if _, err := store.ActivateRecord(doctor, orderDraft.ID); err != nil {
		die("生效医嘱: %v", err)
	}

	grant, err := store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{
			{EncounterID: encounter.ID, Category: clinical.Diagnosis},
			{EncounterID: encounter.ID, Category: clinical.Order},
		},
		fixedEffectiveUTC.Add(-24*time.Hour), fixedEffectiveUTC.Add(24*time.Hour))
	must("建立授权", err)

	// 故意把医嘱排在诊断前面：摘要与入参/排列顺序无关。
	exchange, err := store.CreateExchange(doctor, patient.ID, receiver.ID,
		grant.ID, []clinical.ID{orderDraft.ID, diagDraft.ID}, "request-verify-digest")
	must("创建交换", err)

	// 接收方取包：一次拿到服务摘要与包内容。服务摘要是创建交换时算好、
	// 保存在交换上的值，取包只原样返回、不会重算。
	delivery, err := store.FetchPackage(receiver, exchange.ID)
	must("取包", err)
	serverDigest := delivery.Digest
	fmt.Println("==== 一、完整调用链 ====")
	fmt.Printf("取包成功：交换状态=%s，包内记录 %d 条\n", delivery.Status, len(delivery.Package.Records))
	puts("服务摘要已随取包取得并保存（接收方须原样保留，回执时回传）")

	// 情形一：正常输入——本地严格按内容重算，与服务摘要逐字相等。
	localDigest := digestOf(delivery.Package)
	fmt.Printf("正常内容：本地重算摘要与服务摘要一致=%v\n", localDigest == serverDigest)

	// 仅改变记录的排列顺序、或把同一时刻换一种时区表示，都不是内容改动。
	reordered := clinical.Package{
		PatientID:  delivery.Package.PatientID,
		ReceiverID: delivery.Package.ReceiverID,
		Records: []clinical.PackagedRecord{
			recordAsOf(delivery.Package.Records, clinical.Order, fixedEffectiveInPlus9()),
			recordAsOf(delivery.Package.Records, clinical.Diagnosis, fixedEffectiveInPlus9()),
		},
	}
	reorderedDigest := digestOf(reordered)
	fmt.Printf("记录重排且生效时间改写为 +09:00 同时刻表示后仍一致=%v\n", reorderedDigest == serverDigest)

	// 情形二：只改动收到的正文一个字（模拟留存/传输过程中的改动）。
	tampered := clonePackage(delivery.Package)
	diag := findRecord(tampered.Records, clinical.Diagnosis)
	diag.Content = strings.Replace(diag.Content, "历", "厘", 1)
	tamperedDigest := digestOf(tampered)
	fmt.Printf("正文改动一个字后：本地重算摘要与服务摘要一致=%v（应为 false）\n",
		tamperedDigest == serverDigest)

	// 核对与回执登记的关系：SubmitReceipt 只比对提交摘要与保存摘要，
	// 不重新检查接收方手中的临床内容——被改摘要被拒，交换状态与保存
	// 的服务摘要都不变。
	if _, err := store.SubmitReceipt(receiver, exchange.ID, tamperedDigest,
		clinical.ReceiptAccepted, ""); !errors.Is(err, clinical.ErrConflict) {
		die("被改摘要登记回执应返回 ErrConflict，实际得到 %v", err)
	}
	again, err := store.FetchPackage(receiver, exchange.ID)
	must("再次取包查看状态", err)
	fmt.Printf("用被改摘要登记回执：被拒绝（%v），交换状态仍为 %s，保存的服务摘要不变=%v\n",
		clinical.ErrConflict, again.Status, again.Digest == serverDigest)

	// 本地核对一致后，仍须按既有方式用保存的服务摘要完成回执。
	conf, err := store.SubmitReceipt(receiver, exchange.ID, serverDigest,
		clinical.ReceiptAccepted, "")
	must("用保存的服务摘要登记接受回执", err)
	fmt.Printf("用保存的服务摘要登记接受回执：成功，交换状态=%s，回执结果=%s\n", conf.Status, conf.Outcome)
}

func puts(msg string) { fmt.Println(msg) }

// ---- 第二部分：固定核对向量（标识与内容固定，摘要可手工复现）----

func runFixedVector() {
	fmt.Println("==== 二、固定核对向量（结果确定，可用 sha256sum 复现）====")

	// 接收方留存的一份合成交换包：刻意把医嘱排在前面，并把医嘱的生效
	// 时间写成 +09:00 时区的同一时刻——重算时仍应得到排序后、UTC 表示
	// 的同一份规范文档。
	pkg := clinical.Package{
		PatientID:  fixedPatientID,
		ReceiverID: fixedReceiverID,
		Records: []clinical.PackagedRecord{
			{
				EncounterID: fixedEncounterID,
				Category:    clinical.Order,
				RecordID:    fixedOrderRecID,
				VersionID:   fixedOrderVerID,
				Version:     7,
				EffectiveAt: time.Date(2026, 3, 14, 18, 7, 5, 123456789, fixedEffectiveZone),
				Content:     fixedOrderContent,
			},
			{
				EncounterID: fixedEncounterID,
				Category:    clinical.Diagnosis,
				RecordID:    fixedDiagRecID,
				VersionID:   fixedDiagVerID,
				Version:     2,
				EffectiveAt: fixedEffectiveUTC,
				Content:     fixedDiagContent,
			},
		},
	}

	digest, canonicalJSON, err := ComputePackageDigest(pkg)
	must("固定向量重算摘要", err)
	fmt.Printf("参与摘要的规范字节:\n%s\n", string(canonicalJSON))
	fmt.Printf("规范字节与固定向量逐字一致: %v\n", string(canonicalJSON) == wantFixedCanonicalJSON)
	fmt.Printf("期望摘要（sha256sum 独立复算）: %s\n", wantFixedDigest)
	fmt.Printf("本地根据内容算出的摘要:       %s\n", digest)
	fmt.Printf("核对是否一致: %v\n", digest == wantFixedDigest)

	// 只改正文一个字：摘要完全改变。
	tamperedPkg := clonePackage(pkg)
	tamperedPkg.Records[0].Content = tamperedDiagContent // clonePackage 已按标识排序，0 为诊断
	tamperedDigest, _, err := ComputePackageDigest(tamperedPkg)
	must("被改固定向量重算摘要", err)
	fmt.Printf("正文改动一个字后期望摘要: %s\n", wantTamperedFixedDigest)
	fmt.Printf("正文改动一个字后实算摘要: %s\n", tamperedDigest)
	fmt.Printf("被改摘要与原服务摘要一致: %v\n", tamperedDigest == wantFixedDigest)

	// 重排 + 换时区表示：摘要保持原值。
	shuffled := clinical.Package{
		PatientID:  pkg.PatientID,
		ReceiverID: pkg.ReceiverID,
		Records: []clinical.PackagedRecord{
			recordAsOf(pkg.Records, clinical.Order, fixedEffectiveInPlus9()),
			recordAsOf(pkg.Records, clinical.Diagnosis, fixedEffectiveInPlus9()),
		},
	}
	shuffledDigest, _, err := ComputePackageDigest(shuffled)
	must("重排/换时区固定向量重算摘要", err)
	fmt.Printf("重排并全部改用 +09:00 同时刻表示后摘要: %s（保持不变=%v）\n",
		shuffledDigest, shuffledDigest == wantFixedDigest)
}

// ComputePackageDigest 是接收方一侧的摘要核对函数：只根据取包得到的包
// 内容，按服务端创建交换时相同的规则重算摘要。它不访问存储、不检查
// 授权，是纯函数，可随时对留存的 PackageDelivery.Package 复算。
//
// 规则（详见 README“接收方核对包摘要”）：
//   - 输出一个 UTF-8、无多余空白、字段顺序固定的 JSON 文档；
//   - 顶层字段：patient_id(字符串)、receiver_id(字符串)、records(数组)；
//   - records 先按 record_id 的字符串升序排列；
//   - 每条记录字段顺序固定：encounter_id、category、record_id、
//     version_id 均为字符串，version 为 JSON 整数，effective_at 为
//     “先转 UTC 再按 RFC3339Nano 格式化”的字符串，content 为字符串；
//   - 字符串按 Go encoding/json 默认规则转义：双引号→\"、反斜杠→\\、
//     换行→\n 等控制字符转义，且默认 HTML 转义把 <、>、& 写成
//     \u003c、\u003e、\u0026；中文等非 ASCII 字符以 UTF-8 原样保留；
//   - 对上述字节序列求 SHA-256，编码为 64 位小写十六进制字符串。
func ComputePackageDigest(p clinical.Package) (digest string, canonical []byte, err error) {
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

	recs := make([]clinical.PackagedRecord, len(p.Records))
	copy(recs, p.Records)
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
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), raw, nil
}

func fixedEffectiveInPlus9() time.Time {
	return fixedEffectiveUTC.In(fixedEffectiveZone)
}

// digestOf 是 ComputePackageDigest 的便捷封装：本示例只需要摘要字符串，
// 规范字节仅在固定向量部分打印。参与序列化的都是字符串/整数等基础类型，
// 正常不应出错。
func digestOf(p clinical.Package) string {
	d, _, err := ComputePackageDigest(p)
	if err != nil {
		die("重算摘要失败: %v", err)
	}
	return d
}

func findRecord(recs []clinical.PackagedRecord, category string) *clinical.PackagedRecord {
	for i := range recs {
		if recs[i].Category == category {
			return &recs[i]
		}
	}
	return nil
}

// recordAsOf 取出指定类别的记录副本，并把生效时间改写为同一时刻的另一种
// 时区表示（用于演示时区表示无关）。
func recordAsOf(recs []clinical.PackagedRecord, category string, at time.Time) clinical.PackagedRecord {
	r := *findRecord(recs, category)
	r.EffectiveAt = at
	return r
}

func clonePackage(p clinical.Package) clinical.Package {
	q := clinical.Package{PatientID: p.PatientID, ReceiverID: p.ReceiverID}
	q.Records = append([]clinical.PackagedRecord(nil), p.Records...)
	sort.Slice(q.Records, func(i, j int) bool { return q.Records[i].RecordID < q.Records[j].RecordID })
	return q
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

结果说明（对照输出；第二部分为确定值，任何机器上都相同）：

```text
==== 一、完整调用链 ====
取包成功：交换状态=pending_receipt，包内记录 2 条
服务摘要已随取包取得并保存（接收方须原样保留，回执时回传）
正常内容：本地重算摘要与服务摘要一致=true
记录重排且生效时间改写为 +09:00 同时刻表示后仍一致=true
正文改动一个字后：本地重算摘要与服务摘要一致=false（应为 false）
用被改摘要登记回执：被拒绝（clinical: version conflict），交换状态仍为 pending_receipt，保存的服务摘要不变=true
用保存的服务摘要登记接受回执：成功，交换状态=accepted，回执结果=accepted
==== 二、固定核对向量（结果确定，可用 sha256sum 复现）====
参与摘要的规范字节:
{"patient_id":"pat_digest_golden_01","receiver_id":"rcv_digest_golden","records":[{"encounter_id":"enc_digest_golden_01","category":"diagnosis","record_id":"rec_diag_golden","version_id":"ver_diag_golden_2nd","version":2,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"2型糖尿病（E11.9）\n患者自述：\"多饮、多尿\"，标记 a\u003cb 与 c\u0026d；目录 C:\\病历"},{"encounter_id":"enc_digest_golden_01","category":"order","record_id":"rec_ord_golden","version_id":"ver_ord_golden_7th","version":7,"effective_at":"2026-03-14T09:07:05.123456789Z","content":"医嘱：胰岛素 8IU（餐前）\n注意 \"剂量\u003c10IU 需复核\"；配伍 5%GS\u00260.9%NS；路径 C:\\泵注"}]}
规范字节与固定向量逐字一致: true
期望摘要（sha256sum 独立复算）: f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87
本地根据内容算出的摘要:       f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87
核对是否一致: true
正文改动一个字后期望摘要: 94a2ce6018d932de308ccabea9fd05aabe65087b034c5be87926c43e38b49a99
正文改动一个字后实算摘要: 94a2ce6018d932de308ccabea9fd05aabe65087b034c5be87926c43e38b49a99
被改摘要与原服务摘要一致: false
重排并全部改用 +09:00 同时刻表示后摘要: f790315c6ceffc7d462d574b3e789acbf8ee71f95b028934cbe2bd2be9a9fe87（保持不变=true）
```

- **三个摘要要分清**：`服务提供的摘要`是取包返回、交换创建时保存的
  `f790315c…`；`本地根据内容算出的摘要`是接收方只凭包内容按上面的规则重算
  的值；二者逐字相等才是“核对一致”。正文被改一个字后本地值变为
  `94a2ce60…`，与保存的服务摘要不一致——而服务摘要没有变，仍是比较依据。
- **排列顺序与时区表示**：把医嘱排到前面、把生效时间改写成同一时刻的
  `+09:00` 写法，本地摘要仍是 `f790315c…`。这两种变化都不表示内容被改动。
- **非 Go 实现**同样可以核对：只要复现“按 `record_id` 排序 → 固定字段顺序
  的紧凑 UTF-8 JSON（含 `<`、`>`、`&` 的 `\u00xx` 转义）→ UTC
  RFC3339Nano 时间 → SHA-256 小写十六进制”即可，结果与语言无关。

### 本地核对与回执登记的关系

本地核对和 `SubmitReceipt` 是两件事，必须分开理解：

- **本地核对回答的是**：“我手中这份包内容，按公开规则算出的摘要，是否等于
  取包时服务交付、并保存在交换上的那份摘要。”一致只说明手中内容与交换创建
  时固化的内容相符；它发生在接收方本地，服务端并不知情。
- **`SubmitReceipt` 不做这件事**。它的比较只有一项与摘要有关：提交的
  `digest` 是否**逐字等于交换保存的 `Digest`**。相符才登记接受/拒绝回执；
  不符返回 `ErrConflict`，交换状态不变。调用参数里没有包内容，服务端
  **不会重新检查接收方手中的临床内容**，也无法发现“摘要对、内容被本地误
  改”这类情形——那正是接收方必须自行做本地核对的原因。
- 因此正确顺序是：取包 → 保存 `ExchangeID` 与服务摘要 → 本地按内容重算并
  比对 → **核对一致后，仍按既有方式把保存的服务摘要原样回传**
  （`SubmitReceipt(..., delivery.Digest, ReceiptAccepted, "")`）完成交换；
  授权到期、撤回或患者停用后，对停用前已取得的包仍可这样登记。若本地核对
  不一致，不应把被改内容当作已核验交付，可凭保存的服务摘要按既有方式登记
  拒绝回执（拒绝原因必填），而不是提交被改后的摘要——那样只会得到
  `ErrConflict`。

本次只补齐使用说明与示例：摘要格式、`FetchPackage` 的取包权限判定与
`SubmitReceipt` 的回执判定均保持不变，也没有新增任何交换操作。

## 身份模型

| 身份 | 能力 |
| --- | --- |
| 内部使用者 `InternalActor` | 登记、草稿/生效/更正、授权与撤回、按患者查看授权清单、查看全部草稿、完整历史与审计 |
| 接收方 `ReceiverActor` | 仅在有效授权范围内读取当前生效版本 |

`clinical.Ready()` 保持基线行为，始终返回 `true`。
