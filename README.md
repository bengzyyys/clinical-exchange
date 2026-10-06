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

## 只共享指定诊断（限定记录授权）

最小示例用 `Grant` 把一次就诊的诊断**整类**共享。当只需要共享其中某一条
（例如“只共享第一条诊断，不共享其余诊断”）时，用 `GrantSelective` 的
**限定记录范围**：整类范围留空，在 `RecordSelection` 中明确填写记录标识
及其所属就诊与类别。要点：

- **读到的是哪条记录、哪个版本**：`Read` 结果中的每条 `EffectiveRecord`
  都带记录标识（`RecordID`）、版本标识（`VersionID`）与版本号
  （`Version`）。限定授权选中的是**记录**而非某个固定版本：被选记录更正
  后，读取展示新的版本号与正文；旧版本与更正原因（`Version.Reason`）
  始终不向接收方提供。同一就诊同类**后来新增并生效**的记录也不会自动
  进入这条限定授权。
- **与其他授权是合并而非缩减**：接收方 `Read` 看到的是其**所有当前有效
  授权**允许记录的合集，重叠记录只出现一次。同一接收方若另有一条覆盖
  该范围的整类授权，读取会合并两者允许的记录——不能把限定授权理解为
  对其他授权的缩减。
- **什么情况下会被拒绝**：
  - 限定选择中夹带草稿（或记录与声明的就诊/类别不符、空白标识等）：
    整条授权返回 `ErrInvalidArgument`，**不保留合法部分**，也不新增
    授权创建审计，原先允许读取的内容保持原样。
  - 整类范围与限定选择**同时为空**：同样返回 `ErrInvalidArgument`——
    空选择不表示共享全部诊断。
  - 接收方没有任何覆盖所请求就诊+类别的有效授权（未开始、已到期、
    已撤回或档案停用）：`Read` 返回 `ErrAccessDenied`，不泄露未授权
    记录的标识、数量或内容。

下面的完整示例位于
[`examples/selective_grant`](examples/selective_grant/main.go)
（`go run ./examples/selective_grant`）。它独立准备本地存储、内部使用者、
接收方、合成患者与就诊，在同一次就诊中准备**两条已生效诊断和一条诊断
草稿**，用 `GrantSelective` 只选择第一条生效诊断（整类范围留空，授权
时间窗覆盖读取时刻）；随后更正被选中的诊断再次读取，演示新增同类记录
不自动进入、与整类授权的合并去重，以及两种授权失败情形。示例对每一步
业务调用都检查错误：准备资料或授权失败时明确说明失败发生在哪一步并
终止，不继续把失败返回值当作正式记录使用：

```go
// 命令 selective_grant 演示“只共享指定诊断”的限定记录授权：在同一次就诊
// 中准备两条已生效诊断与一条诊断草稿，用 GrantSelective 只选出第一条生效
// 诊断（整类范围留空），接收方 Read 时只能看到这一条记录的当前生效版本。
//
// 随后演示：
//
//  1. 更正被选中的诊断后，再次读取展示新的版本号与正文；旧版本与更正原因
//     仍不向接收方提供——选中的是记录而非某个固定版本。
//  2. 同一就诊同类后来新增并生效的记录不会自动进入这条限定授权。
//  3. 同一接收方另有一条覆盖该范围的整类授权时，读取合并全部有效授权
//     允许的记录，重叠记录只出现一次——限定授权不是对其他授权的缩减。
//  4. 把一条合法生效诊断与那条草稿一起放入限定选择：整条授权被拒绝
//     （ErrInvalidArgument），不保留合法部分，也不新增授权创建审计，
//     原先允许读取的内容保持原样。
//  5. 整类范围与限定选择同时为空同样被拒绝——空选择不表示共享全部诊断。
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
	dir, err := os.MkdirTemp("", "clinical-selective-grant-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")

	// ---- 准备资料：合成患者、一次就诊、两条已生效诊断与一条诊断草稿 ----
	// 任一步失败都直接终止，不把失败返回值当作正式记录继续使用。
	patient, err := store.RegisterPatient(doctor, "合成患者丁")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
	must("登记就诊", err)

	diag1, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断一：高血压 I10")
	must("创建第一条诊断草稿", err)
	diag1V1, err := store.ActivateRecord(doctor, diag1.ID)
	must("生效第一条诊断", err)

	diag2, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断二：2 型糖尿病 E11")
	must("创建第二条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag2.ID)
	must("生效第二条诊断", err)

	// 第三条只建草稿、不生效：它不应出现在任何接收方读取结果中。
	draft, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断草稿：甲状腺功能异常待查")
	must("创建诊断草稿（不生效）", err)

	// ---- 建立限定授权：整类范围留空，只明确选出第一条生效诊断 ----
	// 选择中显式填写记录标识及其所属就诊与类别；时间窗 [now, now+24h)
	// 覆盖随后的读取时刻。接收方此时只持有这一条限定授权。
	now := time.Now()
	grant, err := store.GrantSelective(doctor, patient.ID, receiver.ID,
		nil, // 整类范围留空：不按类别共享
		[]clinical.RecordSelection{{
			EncounterID: encounter.ID,
			Category:    clinical.Diagnosis,
			RecordID:    diag1.ID,
		}},
		now, now.Add(24*time.Hour))
	must("建立限定授权", err)
	fmt.Printf("限定授权已建立：整类范围 %d 项，限定选择 %d 条\n",
		len(grant.Scopes), len(grant.Selections))

	// ---- 接收方读取：只得到第一条诊断的当前生效内容 ----
	res, err := store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("接收方读取诊断", err)
	if len(res.Records) != 1 || res.Records[0].RecordID != diag1.ID {
		die("限定授权下应只读到第一条诊断，实际读到 %d 条", len(res.Records))
	}
	fmt.Printf("首次读取：仅 1 条，正文=%q，第 %d 版\n",
		res.Records[0].Content, res.Records[0].Version)

	// ---- 更正被选中的诊断：读取展示新版本号与正文 ----
	// 选中的是记录而非某个固定版本：更正后授权覆盖它的当前版本。
	// EffectiveRecord 只有当前生效版本的标识、版本号、正文与生效时间，
	// 旧版本与更正原因（Version.Reason）不在读取结果中。
	diag1V2, err := store.CorrectRecord(doctor, diag1.ID, diag1V1.Number,
		"合成诊断一：高血压 I10（复核确认）", "补录复核依据")
	must("更正第一条诊断", err)
	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("更正后接收方读取诊断", err)
	if len(res.Records) != 1 || res.Records[0].Version != diag1V2.Number ||
		res.Records[0].VersionID != diag1V2.ID {
		die("更正后应读到第一条诊断的第 %d 版，实际 %+v", diag1V2.Number, res.Records)
	}
	fmt.Printf("更正后读取：仍仅 1 条，正文=%q，第 %d 版（旧版本与更正原因不提供）\n",
		res.Records[0].Content, res.Records[0].Version)

	// ---- 后来新增并生效的同类记录不会自动进入这条限定授权 ----
	diag3, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断三：高脂血症 E78.5")
	must("创建第三条诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag3.ID)
	must("生效第三条诊断", err)
	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("新增诊断后接收方读取诊断", err)
	if len(res.Records) != 1 || res.Records[0].RecordID != diag1.ID {
		die("新增生效诊断不应进入限定授权，实际读到 %d 条", len(res.Records))
	}
	fmt.Printf("新增并生效第三条诊断后读取：仍仅 1 条（新记录不自动进入限定授权）\n")

	// ---- 失败演示一：合法生效诊断与草稿一起放入限定选择 ----
	// 任一选择不合法即拒绝整条授权：不保留合法部分，也不新增授权创建审计。
	auditsBefore, err := store.AuditEvents(doctor, patient.ID)
	must("查看授权前审计", err)
	_, err = store.GrantSelective(doctor, patient.ID, receiver.ID, nil,
		[]clinical.RecordSelection{
			{EncounterID: encounter.ID, Category: clinical.Diagnosis, RecordID: diag2.ID},
			{EncounterID: encounter.ID, Category: clinical.Diagnosis, RecordID: draft.ID},
		},
		now, now.Add(24*time.Hour))
	if !errors.Is(err, clinical.ErrInvalidArgument) {
		die("选择中含草稿应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	fmt.Printf("限定选择夹带草稿：整条授权被拒绝（%v）\n", clinical.ErrInvalidArgument)

	auditsAfter, err := store.AuditEvents(doctor, patient.ID)
	must("查看授权后审计", err)
	if len(auditsAfter) != len(auditsBefore) {
		die("失败的授权不应新增审计：之前 %d 条，之后 %d 条", len(auditsBefore), len(auditsAfter))
	}
	fmt.Printf("失败授权未保留合法部分、未新增授权创建审计：审计条数不变=%v\n",
		len(auditsAfter) == len(auditsBefore))

	// 原先允许读取的内容保持原样：仍只有第一条诊断的当前版本。
	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("失败授权后接收方读取诊断", err)
	if len(res.Records) != 1 || res.Records[0].RecordID != diag1.ID ||
		res.Records[0].Version != diag1V2.Number {
		die("失败授权后读取应保持原样，实际读到 %d 条", len(res.Records))
	}
	fmt.Printf("失败授权后读取：仍仅第一条诊断第 %d 版，原授权不受影响\n", diag1V2.Number)

	// ---- 失败演示二：整类范围与限定选择同时为空 ----
	// 空选择不表示共享全部诊断，同样返回 ErrInvalidArgument。
	_, err = store.GrantSelective(doctor, patient.ID, receiver.ID,
		nil, nil, now, now.Add(24*time.Hour))
	if !errors.Is(err, clinical.ErrInvalidArgument) {
		die("整类范围与限定选择同时为空应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	fmt.Printf("整类范围与限定选择同时为空：被拒绝（%v），空选择不表示共享全部诊断\n",
		clinical.ErrInvalidArgument)

	// ---- 合并演示：同一接收方另有一条覆盖该范围的整类授权 ----
	// 读取合并全部有效授权允许的记录，重叠记录只出现一次；限定授权
	// 不是对其他授权的缩减。
	_, err = store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
		now, now.Add(24*time.Hour))
	must("建立整类授权", err)
	res, err = store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("整类授权后接收方读取诊断", err)
	seen := map[clinical.ID]int{}
	for _, r := range res.Records {
		seen[r.RecordID]++
	}
	mergeOK := len(res.Records) == 3 &&
		seen[diag1.ID] == 1 && seen[diag2.ID] == 1 && seen[diag3.ID] == 1 &&
		seen[draft.ID] == 0
	if !mergeOK {
		die("整类+限定合并后应读到 3 条生效诊断且各一次，实际 %+v", res.Records)
	}
	fmt.Printf("另有整类授权后读取：合并为 %d 条生效诊断，重叠的第一条只出现 %d 次，草稿仍不出现\n",
		len(res.Records), seen[diag1.ID])
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
限定授权已建立：整类范围 0 项，限定选择 1 条
首次读取：仅 1 条，正文="合成诊断一：高血压 I10"，第 1 版
更正后读取：仍仅 1 条，正文="合成诊断一：高血压 I10（复核确认）"，第 2 版（旧版本与更正原因不提供）
新增并生效第三条诊断后读取：仍仅 1 条（新记录不自动进入限定授权）
限定选择夹带草稿：整条授权被拒绝（clinical: invalid argument）
失败授权未保留合法部分、未新增授权创建审计：审计条数不变=true
失败授权后读取：仍仅第一条诊断第 2 版，原授权不受影响
整类范围与限定选择同时为空：被拒绝（clinical: invalid argument），空选择不表示共享全部诊断
另有整类授权后读取：合并为 3 条生效诊断，重叠的第一条只出现 1 次，草稿仍不出现
```

- **限定授权生效后**：接收方只持有这一条限定授权，读取该次就诊的诊断
  只得到第一条的当前生效内容（第 1 版）；同一次就诊的另一条已生效诊断
  和那条草稿都不出现。
- **更正之后**：再次读取展示新的版本号（第 2 版）与新正文；旧版本和
  更正原因仍不向接收方提供。这说明选中的是记录而非某个固定版本。
- **新增同类记录**：后来新增并生效的第三条诊断不会自动进入这条限定
  授权，读取仍只有第一条。
- **失败情形**：把一条合法生效诊断与草稿一起放入限定选择，返回
  `ErrInvalidArgument`，整条新授权不成立——不保留合法部分、不新增授权
  创建审计，原先允许读取的内容保持原样；整类范围与限定选择同时为空
  同样被拒绝，空选择不表示共享全部诊断。
- **与整类授权合并**：同一接收方另有一条覆盖该范围的整类授权后，读取
  合并全部有效授权允许的记录（3 条生效诊断），重叠的第一条只出现一次，
  草稿仍不出现；限定授权不是对其他授权的缩减。

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
| 内部使用者 `InternalActor` | 登记、草稿/生效/更正、授权与撤回、查看全部草稿、完整历史与审计 |
| 接收方 `ReceiverActor` | 仅在有效授权范围内读取当前生效版本 |

`clinical.Ready()` 保持基线行为，始终返回 `true`。
