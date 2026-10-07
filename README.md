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
- **草稿 → 生效 → 更正**：诊断/医嘱先存为草稿，可改可删；`ActivateRecord` 固化当时的完整内容与时间。生效记录不可直接覆盖或删除；`CorrectRecord` 必须带非空且为合法 UTF-8 的原因和当前版本号，成功后生成新版本（保留旧内容、`PrevID` 版本链与原样原因），原因夹带任何无效 UTF-8 字节（坏字节位于开头、中间、结尾，或最后一个多字节字符没有写完整）一律返回 `ErrInvalidArgument` 与零值版本，不替换、不截断、不留下新版本或更正审计，版本过期返回 `ErrConflict`。
- **授权与接收方读取**：`Grant` 建立整类授权，范围由明确的“就诊 + 诊断/医嘱类别”组成；`GrantSelective` 还可在该就诊的诊断或医嘱中明确选出若干条**已生效记录**（`RecordSelection`）。同一条授权可包含多个就诊、类别，整类范围与选定记录范围可以并存；重复选择同一记录只算一次。被选记录更正后授权覆盖它的当前版本，但同一就诊同类的其他记录以及后来新增、生效的记录都不会自动进入限定范围。授权带 `[开始, 截止)` 时间窗；空范围、空选择、空白标识、跨患者、选择草稿或记录与声明的就诊/类别不符均拒绝（空选择不会被当成整类授权），任一选择不合法就拒绝整条授权。接收方 `Read` 只能看到所有当前有效授权允许记录的合集（每条只出现一次、按稳定顺序），看不到草稿、旧版本或更正原因；未开始、已到期、已撤回或无授权一律返回 `ErrAccessDenied`，不泄露未授权记录的标识、数量或内容。整类与限定重叠时可见整类内容；撤回整类授权后只剩其他有效授权明确允许的记录。多授权独立判断，撤回互不影响；可 `Revoke` 提前撤回。内部使用者可用 `ListAuthorizations` 按患者（可再按接收方）列出该患者已保存的**全部**授权（含尚未开始、已到期、已撤回者，按授权标识升序）；清单只是历史视图，列出一条授权不代表接收方此刻能读取临床内容，接收方身份不可调用。
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

## 内部使用者查看一次就诊的完整记录历史

最小示例展示了“草稿 → 生效 → 更正”，并提到更正后旧内容会保留；但保留下来的
旧版本与每次更正的原因要怎样取回来？这项查看由内部查询
`EncounterRecords(actor, patientID, encounterID) ([]RecordHistory, error)`
提供：给定患者与某次就诊，返回该就诊下**每条记录的完整视图**，包括尚未生效的
草稿正文、当前生效版本，以及从第一个生效版本到最近一次更正的全部历史版本。

- **一条记录一个 `RecordHistory`，三种状态在同一结构里区分清楚**：
  - `HasDraft` / `DraftContent`：草稿标记与**当前草稿正文**。记录尚处于草稿
    阶段时 `HasDraft=true`；一旦生效，草稿被固化进第 1 版并清掉，此后
    `HasDraft=false`、`DraftContent` 为空。
  - `CurrentVersion *Version`：**当前生效版本**；尚是草稿的记录没有生效版本，
    该字段为 `nil`（不是“第 0 版”，也不是空正文的版本）。
  - `Versions []Version`：**完整版本历史**，按版本号由旧到新排列；草稿没有
    任何版本，因此长度为 0。
  - 外层 `Record` 还带有记录标识、所属患者/就诊、类别，以及按旧到新保存的
    版本标识列表 `Record.Versions`，可用来把当前版本与历史逐版对应。
- **每个 `Version` 能独立对上**：`ID`（版本标识）、`Number`（版本号，第 1 版
  为 1，每更正一次加 1）、`Content`（该版完整正文）、`CreatedAt`（生效或更正
  时间）、`PrevID`（上一版本标识）、`Reason`（更正原因）。
  - **首版没有上一版本、也没有更正原因**：由草稿生效产生，`PrevID` 与
    `Reason` 都为空。
  - **后续每个版本都指向紧邻的上一版**：第 2 版 `PrevID` 等于第 1 版 `ID`，
    第 3 版 `PrevID` 等于第 2 版 `ID`；更正只挂在当时的当前版本之后，不跨版。
  - **当前版本同时就是完整历史中的最新一版**：`CurrentVersion` 与
    `Versions` 的最后一个元素是同一个版本，它不是在历史之外额外发生的一次
    更正；历史里有几版，记录就被更正过（版数 − 1）次。
  - 取得每版“原文”和每次“为什么更正”，读的就是这些 `Content` 与 `Reason`，
    不需要再按版本标识逐个回查。
- **草稿只在内部视图里出现**：草稿没有当前生效版本、没有历史版本；它不会被
  `ActivateRecord` 之外的任何方式表现成“一版”。
- **两种排序不要混用**：
  - **记录列表按记录标识的字符串字典序升序**排列，**不是录入时间顺序**——先
    录入的记录标识未必排在前面，不能用返回先后推断哪条先建。
  - **每条记录的版本历史按版本号由旧到新**排列（1、2、3……），这才是该记录
    的更正先后。
- **使用边界**：
  - **仅供内部使用者**：接收方调用一律返回 `ErrAccessDenied`，结果为 `nil`、
    不携带任何记录。**即使该接收方持有覆盖这次就诊临床内容的有效授权也一样**：
    授权只开放接收方自己的 `Read`（当前生效版本），不开放草稿、旧版本与更正
    原因，持权并不能把接收方变成内部使用者。
  - **患者停用后内部使用者仍可查看**：停用只阻止继续写入与接收方读取，不删
    历史；停用前的草稿与完整版本链仍能通过本查询原样取得。
  - 患者标识不存在返回 `ErrNotFound`；就诊不存在或不属于该患者返回
    `ErrNotFound` / `ErrMismatchedPatient`；这些失败同样不返回任何记录视图。
  - 该查询是只读的，不新增版本或审计；返回的是独立副本，在手中的结果上整理
    内容不会写回正式病历。

下面的完整示例位于
[`examples/encounter_history`](examples/encounter_history/main.go)
（`go run ./examples/encounter_history`）。它自行准备本地存储、一名合成患者与
一次就诊：在该就诊中**保留一条医嘱草稿**（从不生效），另准备一条**已生效且
经过两次更正的诊断**——三次正文、两次更正原因各不相同，两次更正分别以当时的
当前版本号（先 1 后 2）提交。随后由内部使用者查询这次就诊，按记录定位并逐版
打印；再演示接收方持权仍被拒、患者停用后内部仍可取回草稿与完整历史。示例对
准备资料和查询的每一步都检查错误：任一调用失败由 `must`/`die` 明确指出失败的
操作并立即终止，不把失败返回值当作正式患者、记录或历史继续使用：

```go
// 命令 encounter_history 演示内部使用者如何用 EncounterRecords 查看一次就诊
// 的完整记录历史。
//
// 现有说明已经讲过“更正会保留旧版本”，但没有展示内部使用者怎样取得每版
// 正文与每次更正的原因。本示例围绕 EncounterRecords 把这条使用路径走完：
//   - 准备本地存储、一名合成患者与一次就诊；
//   - 在该就诊中保留一条医嘱草稿（不生效，故没有当前生效版本与历史版本）；
//   - 另准备一条已生效、经过两次更正的诊断：三次正文与两次更正原因各不同，
//     每次更正都以当时的当前版本号提交；
//   - 内部使用者查询这次就诊：结果按记录标识排列，两条记录都能根据输出
//     明确对应；版本历史按版本号由旧到新，各版标识、版本号、上一版标识、
//     更正原因与正文逐一对应；
//   - 先验证权限边界：持有本次就诊临床内容授权的接收方，走 Read 只能读到
//     诊断当前生效版本，调用本内部查询则返回 ErrAccessDenied 且无记录；
//   - 再停用患者，演示停用后内部查询仍带医嘱草稿与诊断的完整历史，而接收方
//     的 Read 与本内部查询都仍被拒绝。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/encounter_history
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-encounter-history-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")

	// ---- 准备资料：一名合成患者、一次就诊、一条医嘱草稿、一条两次更正的诊断 ----
	patient, err := store.RegisterPatient(doctor, "合成患者辛")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
	must("登记就诊", err)

	// 记录一（医嘱）：保存为草稿后不再推进，整个示例期间一直是草稿。
	// 它永远不会有当前生效版本，也没有任何历史版本。
	orderDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Order, "合成医嘱草稿：复查项目待主治确认")
	must("创建医嘱草稿（保持草稿状态）", err)

	// 记录二（诊断）：草稿 -> 第 1 版生效 -> 第一次更正为第 2 版 ->
	// 第二次更正为第 3 版。两次更正正文与原因各不同，并且分别以更正发生时
	// 的当前版本号（先 1 后 2）提交——不能预先写成 2。
	diag, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断：急性上呼吸道感染 J06")
	must("创建诊断草稿", err)
	v1, err := store.ActivateRecord(doctor, diag.ID)
	must("生效诊断（第 1 版）", err)
	v2, err := store.CorrectRecord(doctor, diag.ID, v1.Number,
		"合成诊断：急性上呼吸道感染 J06（伴咽部充血）", "复核时补充咽部体征")
	must("第一次更正诊断（以当前版本号 1 提交）", err)
	v3, err := store.CorrectRecord(doctor, diag.ID, v2.Number,
		"合成诊断：急性上呼吸道感染 J06（伴咽部充血，已排除肺炎）", "影像复核排除肺炎")
	must("第二次更正诊断（以当前版本号 2 提交）", err)

	fmt.Println("准备完成: 1 名合成患者、1 次就诊；就诊内有 1 条医嘱草稿与 1 条已生效且两次更正的诊断")
	fmt.Printf("诊断版本链准备: %s(版本 %d) -> %s(版本 %d, 原因=%q) -> %s(版本 %d, 原因=%q)\n",
		v1.ID, v1.Number, v2.ID, v2.Number, v2.Reason, v3.ID, v3.Number, v3.Reason)

	// ---- 内部使用者查询这次就诊：两条记录按记录标识升序返回 ----
	histories, err := store.EncounterRecords(doctor, patient.ID, encounter.ID)
	must("内部使用者查询就诊记录", err)
	fmt.Printf("\n就诊记录查询: 错误=%v，记录数=%d，结果按记录标识升序=%v\n",
		err, len(histories), recordsByIDCached(histories))

	if len(histories) != 2 {
		die("这次就诊应当恰好有 2 条记录（医嘱草稿 + 诊断），实际得到 %d 条", len(histories))
	}

	// 按记录标识在结果中定位两条记录。输出中用“医嘱草稿”/“诊断”标注，
	// 读者可以把下面的逐字段输出对应到这两条记录，而不是只看到一个结构体。
	orderH := findRecordHistory(histories, orderDraft.ID)
	diagH := findRecordHistory(histories, diag.ID)
	if orderH == nil || diagH == nil {
		die("查询结果应同时包含医嘱草稿 %q 与诊断 %q，实际为 %v",
			orderDraft.ID, diag.ID, recordIDList(histories))
	}

	printOrderDraft(orderH)
	printDiagnosisHistory(diagH, v1, v2, v3)

	// 当前版本同时也是完整历史中的最新一版：它不是额外发生的一次更正。
	latest := diagH.Versions[len(diagH.Versions)-1]
	if latest.ID != diagH.CurrentVersion.ID || latest.Number != diagH.CurrentVersion.Number {
		die("当前版本应当就是历史列表中的最新一版，实际当前=%+v 最新=%+v",
			diagH.CurrentVersion, latest)
	}
	fmt.Printf("当前版本即历史最新版: 当前版本 %s（版本 %d）== 历史末版 %s（版本 %d）=%v；"+
		"它不是额外发生的一次更正\n",
		diagH.CurrentVersion.ID, diagH.CurrentVersion.Number,
		latest.ID, latest.Number, latest.ID == diagH.CurrentVersion.ID)

	// 版本链完整性核对：每个后续版本的 PrevID 指向紧邻的上一版，首版无上一版。
	if err := checkVersionChain(diagH.Versions); err != nil {
		die("诊断版本链有误: %v", err)
	}
	fmt.Println("版本链核对: 首版无上一版且无更正原因；第 2、3 版各自指向紧邻的上一版；历史按版本号旧到新")

	// ---- 使用边界一：接收方即使持有该就诊的临床内容授权，也不能用此内部查询 ----
	// 在患者仍处于活动状态时，为接收方建立覆盖本次就诊“诊断”类别的有效整类
	// 授权（医嘱草稿本就不对接收方开放）。接收方走自己的 Read 入口，只能读到
	// 诊断的当前生效版本——读不到草稿、旧版本与更正原因。
	now := time.Now()
	grant, err := store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
		now, now.Add(24*time.Hour))
	must("为接收方建立本次就诊诊断的有效授权", err)
	rres, err := store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis)
	must("接收方按接收方入口读取本次就诊诊断", err)
	seesOnlyCurrent := len(rres.Records) == 1 &&
		rres.Records[0].RecordID == diag.ID &&
		rres.Records[0].Version == v3.Number &&
		rres.Records[0].Content == v3.Content
	fmt.Printf("\n边界演示: 接收方持有覆盖本次就诊诊断的有效授权（授权 %s）\n", grant.ID)
	fmt.Printf("  接收方 Read 入口: 记录数=%d，只读到诊断当前生效版本（版本 %d，与第 3 版一致）=%v；"+
		"草稿、旧版本、更正原因均不在结果中\n",
		len(rres.Records), rres.Records[0].Version, seesOnlyCurrent)

	// 但 EncounterRecords 是内部查询入口：哪怕授权就在这次就诊上、时间窗有效，
	// 接收方调用也一律 ErrAccessDenied，结果为 nil、不携带任何记录（草稿、
	// 旧版本、更正原因都不会随拒绝结果泄露）。被拒不随患者停用而改变，下面
	// 在患者活动时就先验证一次。
	denied, deniedErr := store.EncounterRecords(receiver, patient.ID, encounter.ID)
	if !errors.Is(deniedErr, clinical.ErrAccessDenied) || denied != nil {
		die("接收方调用 EncounterRecords 应返回 nil 与 ErrAccessDenied，实际得到 %d 条、%v",
			len(denied), deniedErr)
	}
	fmt.Printf("  接收方 EncounterRecords 入口: 被拒绝（%v），返回记录数=%d（不提供草稿、旧版本或更正原因）\n",
		deniedErr, len(denied))

	// ---- 使用边界二：患者停用后，内部使用者仍能取得原有草稿与完整历史 ----
	must("停用患者", store.DeactivatePatient(doctor, patient.ID))
	afterOff, err := store.EncounterRecords(doctor, patient.ID, encounter.ID)
	must("停用后内部使用者再次查询就诊记录", err)
	orderAfter := findRecordHistory(afterOff, orderDraft.ID)
	diagAfter := findRecordHistory(afterOff, diag.ID)
	if orderAfter == nil || !orderAfter.HasDraft || orderAfter.DraftContent != "合成医嘱草稿：复查项目待主治确认" ||
		len(orderAfter.Versions) != 0 ||
		diagAfter == nil || diagAfter.CurrentVersion == nil || diagAfter.CurrentVersion.Number != 3 ||
		len(diagAfter.Versions) != 3 {
		die("停用后查询结果应当仍含原医嘱草稿与诊断全部 3 个版本，实际 %v",
			recordIDList(afterOff))
	}
	fmt.Printf("\n患者停用后内部查询: 错误=%v，记录数=%d；医嘱草稿仍在（草稿标记=%v，历史版本数=%d），"+
		"诊断当前版本=%d、历史版本数=%d（原有草稿与完整历史均保留）\n",
		err, len(afterOff), orderAfter.HasDraft, len(orderAfter.Versions),
		diagAfter.CurrentVersion.Number, len(diagAfter.Versions))

	// 停用后接收方的两种读取都不允许：Read 被拒，内部查询同样仍被拒且无记录。
	if _, err := store.Read(receiver, patient.ID, encounter.ID, clinical.Diagnosis); !errors.Is(err, clinical.ErrAccessDenied) {
		die("患者停用后接收方 Read 应返回 ErrAccessDenied，实际得到 %v", err)
	}
	denied2, deniedErr2 := store.EncounterRecords(receiver, patient.ID, encounter.ID)
	if !errors.Is(deniedErr2, clinical.ErrAccessDenied) || denied2 != nil {
		die("患者停用后接收方调用内部查询应返回 nil 与 ErrAccessDenied，实际得到 %d 条、%v",
			len(denied2), deniedErr2)
	}
	fmt.Printf("患者停用后接收方读取: Read 被拒绝（%v）；EncounterRecords 仍被拒绝（%v），返回记录数=%d\n",
		clinical.ErrAccessDenied, deniedErr2, len(denied2))
}

// printOrderDraft 打印草稿形态的记录：只有草稿标记与当前草稿正文，
// 没有当前生效版本（CurrentVersion 为 nil），也没有历史版本。
func printOrderDraft(h *clinical.RecordHistory) {
	fmt.Printf("\n记录一（%s，记录标识=%s，所属就诊=%s）:\n", "医嘱草稿", h.Record.ID, h.Record.EncounterID)
	fmt.Printf("  类别=%s，草稿标记 HasDraft=%v，当前草稿正文=%q\n",
		h.Record.Category, h.HasDraft, h.DraftContent)
	if h.CurrentVersion != nil {
		die("医嘱草稿不应有当前生效版本，实际得到 %+v", h.CurrentVersion)
	}
	if len(h.Record.Versions) != 0 || len(h.Versions) != 0 {
		die("医嘱草稿不应有任何历史版本，实际标识列表=%v 历史=%+v",
			h.Record.Versions, h.Versions)
	}
	fmt.Printf("  当前生效版本: 无（CurrentVersion 为 nil；草稿尚未生效，不展示为任何版本）\n")
	fmt.Printf("  历史版本: 无（共 %d 个版本；草稿阶段没有版本，更正历史无从谈起）\n", len(h.Versions))
}

// printDiagnosisHistory 打印已生效并更正两次的诊断：当前版本号/正文，以及
// 从首版到两次更正后的完整历史；逐版打印标识、版本号、上一版标识与原因。
func printDiagnosisHistory(h *clinical.RecordHistory, v1, v2, v3 clinical.Version) {
	fmt.Printf("\n记录二（%s，记录标识=%s，所属就诊=%s）:\n", "诊断", h.Record.ID, h.Record.EncounterID)
	fmt.Printf("  类别=%s，草稿标记 HasDraft=%v\n", h.Record.Category, h.HasDraft)
	if h.CurrentVersion == nil {
		die("已生效诊断应有当前生效版本")
	}
	fmt.Printf("  当前生效版本: 版本号=%d，版本标识=%s，当前正文=%q\n",
		h.CurrentVersion.Number, h.CurrentVersion.ID, h.CurrentVersion.Content)
	if want := []clinical.ID{v1.ID, v2.ID, v3.ID}; !equalIDs(h.Record.Versions, want) {
		die("诊断版本标识列表应为 %v，实际 %v", want, h.Record.Versions)
	}
	fmt.Printf("  当前版本即完整历史中的最新一版（版本号 %d）；完整历史按版本号由旧到新如下（共 %d 版）:\n",
		h.CurrentVersion.Number, len(h.Versions))
	for i, v := range h.Versions {
		prev := "无（这是第一个生效版本）"
		if v.PrevID != "" {
			prev = fmt.Sprintf("%s", v.PrevID)
		}
		reason := "无（首版由草稿生效，没有更正原因）"
		if v.Reason != "" {
			reason = fmt.Sprintf("%q", v.Reason)
		}
		fmt.Printf("    历史[%d]: 版本标识=%s，版本号=%d，上一版本标识=%s，更正原因=%s，正文=%q\n",
			i, v.ID, v.Number, prev, reason, v.Content)
	}
}

// checkVersionChain 校验历史按版本号旧到新、首版无 PrevID/Reason、
// 后续版本 PrevID 指向紧邻上一版的标识。
func checkVersionChain(vs []clinical.Version) error {
	if len(vs) == 0 {
		return fmt.Errorf("历史为空")
	}
	for i, v := range vs {
		if v.Number != i+1 {
			return fmt.Errorf("历史[%d] 版本号=%d，应按旧到新为 %d", i, v.Number, i+1)
		}
		if i == 0 {
			if v.PrevID != "" || v.Reason != "" {
				return fmt.Errorf("首版不应有上一版本或更正原因，得到 PrevID=%q Reason=%q",
					v.PrevID, v.Reason)
			}
			continue
		}
		if v.PrevID != vs[i-1].ID {
			return fmt.Errorf("版本 %d 的 PrevID=%q 应指向紧邻上一版 %q",
				v.Number, v.PrevID, vs[i-1].ID)
		}
		if strings.TrimSpace(v.Reason) == "" {
			return fmt.Errorf("版本 %d 是更正产生的版本，更正原因不应为空", v.Number)
		}
	}
	return nil
}

// findRecordHistory 按记录标识定位一条记录视图；不存在返回 nil。
func findRecordHistory(hs []clinical.RecordHistory, id clinical.ID) *clinical.RecordHistory {
	for i := range hs {
		if hs[i].Record.ID == id {
			return &hs[i]
		}
	}
	return nil
}

// recordsByIDCached 核对结果是否按记录标识升序（字符串字典序）。
func recordsByIDCached(hs []clinical.RecordHistory) bool {
	for i := 1; i < len(hs); i++ {
		if hs[i-1].Record.ID >= hs[i].Record.ID {
			return false
		}
	}
	return true
}

func recordIDList(hs []clinical.RecordHistory) []clinical.ID {
	ids := make([]clinical.ID, len(hs))
	for i, h := range hs {
		ids[i] = h.Record.ID
	}
	return ids
}

func equalIDs(a, b []clinical.ID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

结果说明（对照一次真实运行的输出；`rec_`/`ver_`/`auth_` 标识由存储随机
生成，每次运行不同，需要核对的是版本号、正文、原因与各标识之间的**指向关系**，
而不是具体字面值）：

```text
准备完成: 1 名合成患者、1 次就诊；就诊内有 1 条医嘱草稿与 1 条已生效且两次更正的诊断
诊断版本链准备: ver_c6d125e72b1cda01b339300a(版本 1) -> ver_aa11d90ca26a91e778e89966(版本 2, 原因="复核时补充咽部体征") -> ver_eabd066a2a1f36561bb5f440(版本 3, 原因="影像复核排除肺炎")

就诊记录查询: 错误=<nil>，记录数=2，结果按记录标识升序=true

记录一（医嘱草稿，记录标识=rec_f7f398fd46936456134430f0，所属就诊=enc_39377be23340bab6ecfc10c8）:
  类别=order，草稿标记 HasDraft=true，当前草稿正文="合成医嘱草稿：复查项目待主治确认"
  当前生效版本: 无（CurrentVersion 为 nil；草稿尚未生效，不展示为任何版本）
  历史版本: 无（共 0 个版本；草稿阶段没有版本，更正历史无从谈起）

记录二（诊断，记录标识=rec_175d5067691613b36bb090f9，所属就诊=enc_39377be23340bab6ecfc10c8）:
  类别=diagnosis，草稿标记 HasDraft=false
  当前生效版本: 版本号=3，版本标识=ver_eabd066a2a1f36561bb5f440，当前正文="合成诊断：急性上呼吸道感染 J06（伴咽部充血，已排除肺炎）"
  当前版本即完整历史中的最新一版（版本号 3）；完整历史按版本号由旧到新如下（共 3 版）:
    历史[0]: 版本标识=ver_c6d125e72b1cda01b339300a，版本号=1，上一版本标识=无（这是第一个生效版本），更正原因=无（首版由草稿生效，没有更正原因），正文="合成诊断：急性上呼吸道感染 J06"
    历史[1]: 版本标识=ver_aa11d90ca26a91e778e89966，版本号=2，上一版本标识=ver_c6d125e72b1cda01b339300a，更正原因="复核时补充咽部体征"，正文="合成诊断：急性上呼吸道感染 J06（伴咽部充血）"
    历史[2]: 版本标识=ver_eabd066a2a1f36561bb5f440，版本号=3，上一版本标识=ver_aa11d90ca26a91e778e89966，更正原因="影像复核排除肺炎"，正文="合成诊断：急性上呼吸道感染 J06（伴咽部充血，已排除肺炎）"
当前版本即历史最新版: 当前版本 ver_eabd066a2a1f36561bb5f440（版本 3）== 历史末版 ver_eabd066a2a1f36561bb5f440（版本 3）=true；它不是额外发生的一次更正
版本链核对: 首版无上一版且无更正原因；第 2、3 版各自指向紧邻的上一版；历史按版本号旧到新

边界演示: 接收方持有覆盖本次就诊诊断的有效授权（授权 auth_25259bdfcd557520edf68ecf）
  接收方 Read 入口: 记录数=1，只读到诊断当前生效版本（版本 3，与第 3 版一致）=true；草稿、旧版本、更正原因均不在结果中
  接收方 EncounterRecords 入口: 被拒绝（clinical: access denied），返回记录数=0（不提供草稿、旧版本或更正原因）

患者停用后内部查询: 错误=<nil>，记录数=2；医嘱草稿仍在（草稿标记=true，历史版本数=0），诊断当前版本=3、历史版本数=3（原有草稿与完整历史均保留）
患者停用后接收方读取: Read 被拒绝（clinical: access denied）；EncounterRecords 仍被拒绝（clinical: access denied），返回记录数=0
```

- **两条记录按记录标识区分，不按录入先后**：这次就诊返回 2 条并核对为按记录
  标识升序；医嘱虽是后建的，但是否排前只取决于标识字典序。示例按记录标识
  定位，分别按“草稿”和“已更正诊断”两种形态打印，输出能明确对应到这两条记录。
- **草稿形态**：医嘱 `HasDraft=true`，打印出当前草稿正文；`CurrentVersion`
  为 `nil`（明确“没有当前生效版本”），历史版本数为 0——草稿既不是第 0 版，
  也没有旧版可更正。
- **当前生效内容**：诊断的当前版本号是 3、正文是第二次更正后的版本；这与
  接收方 `Read` 能读到的内容一致，但内部视图在此之外还给出完整历史。
- **完整版本历史**：3 个版本按版本号 1→2→3 由旧到新。第 1 版上一版本与更正
  原因均为“无”；第 2 版 `PrevID` 指向第 1 版、原因为“复核时补充咽部体征”；
  第 3 版 `PrevID` 指向第 2 版、原因为“影像复核排除肺炎”。三版正文逐字保留，
  内部使用者据此可取得每版原文与每次更正的原因。
- **当前版本不是额外一次更正**：当前版本（版本 3）与历史末版是同一标识、同一
  版本号；历史共 3 版对应“生效 1 次 + 更正 2 次”，并不存在第 4 次更正。
- **接收方边界**：接收方持有覆盖该就诊诊断的有效授权，`Read` 仍只读到当前
  生效版本（版本 3）；调用 `EncounterRecords` 在患者活动时就得到
  `ErrAccessDenied` 且 0 条记录，草稿、旧版本与更正原因都不随拒绝结果泄露。
- **停用后**：内部使用者查询仍返回 2 条——医嘱草稿（`HasDraft=true`、0 个
  历史版本）与诊断（当前版本 3、完整 3 版历史）原样保留；而接收方的 `Read`
  与 `EncounterRecords` 都被拒绝。

本次只补齐 `EncounterRecords` 的使用说明与示例：入口签名、`RecordHistory`/
`Version` 返回内容、草稿/当前版本/历史的区分、排序与权限规则均沿用已有公开
行为，没有新增或改动任何接口。

## 更正被版本冲突拒绝后：重新查看当前内容再更正

前文已经说明 `CorrectRecord` 必须携带**当前版本号**，版本过期返回
`ErrConflict`；但没有展示使用者拿着旧版本号提交被拒之后，怎样取回最新内容、
再完成这次更正。本节围绕同一条合成诊断把这条使用路径走完。

- **先分清三个标识，更正参数要的是整数版本号**：
  - **记录标识 `Record.ID`**：标“哪一条记录”。草稿生效、之后无论更正多少次，
    它都不变；更正不换记录标识。
  - **版本标识 `Version.ID`**：标“哪一个不可变版本”。生效产生第 1 版标识，
    每次更正再产生一个全新标识；历史靠 `Version.PrevID` 逐版链接。
  - **整数版本号 `Version.Number`**：第 1 版为 1，每成功更正一次加 1。
    `CorrectRecord(actor, recordID, expectedCurrentVersion, content, reason)`
    的第三个参数要的是**记录当前版本的整数版本号**（当前 `Version.Number`），
    不是记录标识，也不是版本标识。
- **冲突是怎么发生的**：两名内部使用者可以各自通过 `EncounterRecords` 取得同
  一条记录当时的当前版本。若一人先以版本号 1 更正成功（记录成为第 2 版），
  另一人仍用手中早先取得的版本号 1 提交**不同的完整正文与非空原因**，库在落盘
  前发现当前版本号已是 2，返回 `ErrConflict`——哪怕正文和原因本身都合法。
- **被拒不产生任何东西**：冲突返回的 `Version` 是**零值**（版本号 0、标识为空），
  不是“第 2 版”，更不是某个新第 3 版，**不能把失败返回值当成已经保存的新版本
  继续使用**。正式记录仍是第 2 版，正文与原因保留第一次更正的结果；版本历史
  仍只有两版，更正审计仍只有第一次更正那一条。被拒者提交的正文不会以任何形式
  并入正式记录。
- **被拒后的正确做法：重新查看 → 重新确认内容 → 用最新版本号提交完整正文**：
  1. 用 `EncounterRecords`（或 `Chart`）重新取回这条记录，以**新查到的当前
     版本**为准；手中早先那份停留在第 1 版的结果已不能用于提交。
  2. **重新确认要保留的完整正文**：更正提交的是**完整正文**，库**不会自动合并**
     两名使用者的修改——不会把被拒正文与第 2 版正文拼接。若要保留第 2 版已经
     确认的变化，必须在重新组织的正文里显式写上，再追加本次补充。
  3. 以重新查到的当前版本号（此时为 2）与非空原因再次 `CorrectRecord`，成功
     产生紧接第 2 版的第 3 版：`PrevID` 指向第 2 版标识，第 1、2 版原样保留。
- **区分两种预期失败**：
  - **`ErrConflict`（版本冲突）**：正文、原因都合法，仅“当前版本号”已过期。
  - **`ErrInvalidArgument`（参数不合法）**：例如**更正原因全为空白**（只含
    空格、制表、换行也算空白），或正文全为空白；即使版本号填的就是当前版本号
    也拒绝。参数校验在冲突检查之前进行，与是否并发无关。
  - 两种失败都**不产生新版本、不新增更正审计**，记录与历史保持提交前状态。
- **其他错误不属于“预期失败”**：准备存储、登记患者/就诊、草稿生效、重新查询
  或“应当成功”的更正一旦出错，示例用 `must`/`die` 明确指出失败的操作并立即
  终止，绝不带着失败返回值继续往下走。

下面的完整示例位于
[`examples/correction_conflict`](examples/correction_conflict/main.go)
（`go run ./examples/correction_conflict`）。它自行准备本地存储、一名合成患者、
一次就诊和一条已生效诊断（第 1 版）；两名不同的内部使用者先各自取得第 1 版
信息，其中一人更正成功产生第 2 版，另一人仍用旧版本号 1 提交不同正文与非空
原因，明确收到 `ErrConflict`，并核对正式记录仍为第 2 版、历史仍只有两版、无
新增更正审计；随后再演示“原因全为空白”得到 `ErrInvalidArgument`。最后由被拒
的使用者重新查看当前内容，在第 2 版已确认正文的基础上重新组织完整正文，用最新
版本号成功更正出第 3 版。示例对每一步准备、查询与提交都检查错误：

```go
// 命令 correction_conflict 演示更正遇到版本冲突后的完整处理过程：两名内部
// 使用者先各自取得同一条诊断的第 1 版信息，一人先更正成功产生第 2 版，另一人
// 仍用手中的旧版本号提交不同正文与非空原因，得到 ErrConflict；被拒者重新查看
// 记录的当前内容，在已确认的第 2 版正文基础上重新组织完整正文，用最新版本号
// 再次更正，成功产生第 3 版。
//
// 示例同时区分两种预期失败：
//   - 版本号已过期（内容、原因都合法）→ ErrConflict；
//   - 更正原因全为空白（哪怕版本号就是当前版本号）→ ErrInvalidArgument。
//
// 两种失败都不产生新版本或更正审计，失败返回的 Version 是零值，不能当成已经
// 保存的新版本继续使用。示例还明确区分记录标识、版本标识与整数版本号：
// CorrectRecord 需要的是当前版本的整数版本号 Version.Number。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/correction_conflict
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

// 同一条合成诊断的三个正式版本正文，以及一次被拒更正的正文。
const (
	contentV1 = "合成诊断：急性支气管炎 J20"
	contentV2 = "合成诊断：急性支气管炎 J20（已听诊确认）"
	reasonV2  = "听诊复核确认体征"

	// rejectedContent/rejectedReason 是第二名使用者基于旧版本号提交的内容：
	// 它永远不会被保存——库不自动合并任何人的修改。
	rejectedContent = "合成诊断：急性支气管炎 J20（疑为肺炎，建议影像）"
	rejectedReason  = "建议影像排查肺炎"

	// 第 3 版由被拒者重新查看后提交：完整正文里保留第 2 版已经确认的
	// “（已听诊确认）”，再追加本次补充，而不是只写追加片段。
	contentV3 = "合成诊断：急性支气管炎 J20（已听诊确认；补充复诊随访安排）"
	reasonV3  = "冲突后重新查看：保留已听诊确认结论，补充复诊随访安排"

	// blankReasonAttempt 只用于演示“原因全为空白”的失败，本身不会被保存。
	blankReasonAttemptContent = "合成诊断：急性支气管炎 J20（空白原因的尝试不会保存）"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-correction-conflict-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	// 两名不同的内部使用者；他们各自独立查看、独立提交更正。
	doctorA := clinical.InternalActor("doctor-a")
	doctorB := clinical.InternalActor("doctor-b")

	// ---- 准备：一名合成患者、一次就诊、一条已生效的诊断（第 1 版）----
	patient, err := store.RegisterPatient(doctorA, "合成患者壬")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctorA, patient.ID, time.Now())
	must("登记就诊", err)
	draft, err := store.CreateDraft(doctorA, patient.ID, encounter.ID,
		clinical.Diagnosis, contentV1)
	must("创建诊断草稿", err)
	v1, err := store.ActivateRecord(doctorA, draft.ID)
	must("生效诊断（第 1 版）", err)

	fmt.Println("准备完成: 1 名合成患者、1 次就诊、1 条已生效诊断")
	fmt.Printf("  记录标识 Record.ID = %s（记录全程不变，更正不换标识）\n", draft.ID)
	fmt.Printf("  第 1 版: 版本标识 Version.ID = %s，整数版本号 Version.Number = %d\n", v1.ID, v1.Number)
	fmt.Println("标识区分: 记录标识标“哪条记录”；版本标识标“哪一个不可变版本”；" +
		"CorrectRecord 要带的是当前版本的整数版本号 Number，不是前两个标识")

	// ---- 两名内部使用者都先取得第 1 版的信息 ----
	// 各自通过内部查询取回当前版本；此刻两人看到的都是第 1 版。
	heldA := mustHistory(store, doctorA, patient.ID, encounter.ID, draft.ID)
	heldB := mustHistory(store, doctorB, patient.ID, encounter.ID, draft.ID)
	if heldA.CurrentVersion == nil || heldB.CurrentVersion == nil ||
		heldA.CurrentVersion.Number != 1 || heldB.CurrentVersion.Number != 1 ||
		heldA.CurrentVersion.ID != v1.ID || heldB.CurrentVersion.ID != v1.ID {
		die("两名使用者取得的都应是第 1 版（版本标识 %s），实际 A=%+v B=%+v",
			v1.ID, heldA.CurrentVersion, heldB.CurrentVersion)
	}
	fmt.Printf("\n两人各自查看: doctor-a 与 doctor-b 取得同一当前版本——"+
		"版本号=%d，版本标识=%s\n", heldB.CurrentVersion.Number, heldB.CurrentVersion.ID)

	// ---- 第一次更正（成功）：doctor-a 用当时的当前版本号 1 提交 ----
	v2, err := store.CorrectRecord(doctorA, draft.ID, heldA.CurrentVersion.Number,
		contentV2, reasonV2)
	must("doctor-a 第一次更正诊断", err)
	fmt.Printf("\n【第一次更正·成功】doctor-a 以版本号 %d 提交: 记录成为第 %d 版\n",
		heldA.CurrentVersion.Number, v2.Number)
	fmt.Printf("  新版本标识=%s，上一版本标识=%s（指向第 1 版 %s）=%v\n",
		v2.ID, v2.PrevID, v1.ID, v2.PrevID == v1.ID)
	fmt.Printf("  新正文=%q，更正原因=%q\n", v2.Content, v2.Reason)

	// ---- 第二次提交（被拒）：doctor-b 仍用手中的旧版本号 1 ----
	// 正文与原因都非空、合法，唯一的问题是版本号已不是当前版本号。
	rejected, err := store.CorrectRecord(doctorB, draft.ID, heldB.CurrentVersion.Number,
		rejectedContent, rejectedReason)
	switch {
	case err == nil:
		die("doctor-b 用旧版本号更正应当被拒绝，却返回了新版本 %+v", rejected)
	case errors.Is(err, clinical.ErrConflict):
		// 预期失败：返回的 Version 是零值，绝不是“第 2 版”或某个新第 3 版，
		// 不能把它当作已保存结果继续传递。
		if rejected != (clinical.Version{}) {
			die("冲突时返回值应为零值 Version，实际得到 %+v", rejected)
		}
		fmt.Printf("\n【第二次提交·被拒】doctor-b 仍以取得时的版本号 %d 提交不同正文: %v\n",
			heldB.CurrentVersion.Number, err)
		fmt.Printf("  错误类型=ErrConflict；失败返回值是零值版本（版本号=%d、版本标识=%q），"+
			"不是已保存的新版本，不能继续使用\n", rejected.Number, rejected.ID)
	default:
		die("doctor-b 旧版本号更正应返回 ErrConflict，实际得到 %v", err)
	}

	// ---- 核对被拒后的正式记录：仍是第 2 版，历史仍只有两版，无新增更正审计 ----
	afterConflict := mustHistory(store, doctorB, patient.ID, encounter.ID, draft.ID)
	cur := afterConflict.CurrentVersion
	if cur.Number != 2 || cur.ID != v2.ID || cur.Content != contentV2 ||
		cur.Reason != reasonV2 || cur.PrevID != v1.ID {
		die("冲突后正式记录应仍为第 2 版且正文/原因保留第一次更正结果，实际当前版本=%+v", cur)
	}
	if len(afterConflict.Versions) != 2 {
		die("冲突后历史应仍只有两版，实际有 %d 版", len(afterConflict.Versions))
	}
	if strings.Contains(cur.Content, "影像") || strings.Contains(cur.Content, "肺炎") {
		die("被拒正文不得进入正式记录，实际当前正文=%q", cur.Content)
	}
	correctedSoFar := mustCorrectionAudits(store, doctorA, patient.ID)
	if len(correctedSoFar) != 1 || correctedSoFar[0].ActorID != doctorA.ID {
		die("冲突后应只有 doctor-a 的 1 条更正审计，实际 %+v", correctedSoFar)
	}
	fmt.Printf("被拒后核对: 正式记录仍为第 %d 版（版本标识=%s），正文与原因保留第一次更正结果=%v；"+
		"历史版本数=%d；更正审计数=%d（被拒提交没有新增审计）\n",
		cur.Number, cur.ID, cur.Content == contentV2 && cur.Reason == reasonV2,
		len(afterConflict.Versions), len(correctedSoFar))

	// ---- 另一种预期失败：更正原因全为空白 → ErrInvalidArgument ----
	// 这次版本号用的就是当前版本号 2，参数问题（原因空白）与并发无关；
	// 它同样不产生新版本或更正审计。
	auditsBeforeBlank := len(mustCorrectionAudits(store, doctorA, patient.ID))
	blankReason := " \t\n " // 去掉空白后为空
	blank, err := store.CorrectRecord(doctorB, draft.ID, cur.Number,
		blankReasonAttemptContent, blankReason)
	switch {
	case err == nil:
		die("原因全为空白的更正应当被拒绝，却返回了新版本 %+v", blank)
	case errors.Is(err, clinical.ErrInvalidArgument):
		if blank != (clinical.Version{}) {
			die("参数非法时返回值应为零值 Version，实际得到 %+v", blank)
		}
		fmt.Printf("\n【原因空白·被拒】以当前版本号 %d 提交、但更正原因为全空白 %q: %v\n",
			cur.Number, blankReason, err)
		fmt.Println("  错误类型=ErrInvalidArgument（参数本身不合法，与版本是否过期无关）；同样不产生新版本或审计")
	default:
		die("原因全白应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	afterBlank := mustHistory(store, doctorB, patient.ID, encounter.ID, draft.ID)
	auditsAfterBlank := len(mustCorrectionAudits(store, doctorA, patient.ID))
	if afterBlank.CurrentVersion.Number != 2 || afterBlank.CurrentVersion.ID != v2.ID ||
		len(afterBlank.Versions) != 2 || auditsAfterBlank != auditsBeforeBlank {
		die("空白原因失败后记录应仍为第 2 版两版历史且审计不增加，实际版本号=%d 历史=%d 审计=%d→%d",
			afterBlank.CurrentVersion.Number, len(afterBlank.Versions), auditsBeforeBlank, auditsAfterBlank)
	}
	fmt.Printf("  失败后核对: 仍为第 %d 版、历史 %d 版、更正审计 %d 条（未增加）\n",
		afterBlank.CurrentVersion.Number, len(afterBlank.Versions), auditsAfterBlank)
	fmt.Printf("两种失败的区别: ErrConflict=内容与原因都合法、仅版本号过期；" +
		"ErrInvalidArgument=原因（或正文）空白等参数问题，在冲突检查之前就被拒绝\n")

	// ---- 被拒者重新查看记录的当前内容 ----
	// 手中的 heldB 停留在第 1 版，已不能用于提交；以重新查到的当前版本为准。
	refreshed := mustHistory(store, doctorB, patient.ID, encounter.ID, draft.ID)
	latest := refreshed.CurrentVersion
	fmt.Printf("\n被拒后重新查看: doctor-b 看到记录当前为第 %d 版（版本标识=%s）\n",
		latest.Number, latest.ID)
	fmt.Printf("  当前正文=%q，当前更正原因=%q\n", latest.Content, latest.Reason)
	if latest.Number != 2 || latest.ID != v2.ID || latest.Content != contentV2 {
		die("重新查看应得到第 2 版第一次更正后的内容，实际=%+v", latest)
	}

	// ---- 确认要保留的内容后，用最新版本号再次更正 ----
	// 更正提交的是完整正文：库里不会把 rejectedContent 与别人的修改自动合并，
	// 所以新正文必须显式保留第 2 版已确认的“（已听诊确认）”，再写本次补充。
	v3, err := store.CorrectRecord(doctorB, draft.ID, latest.Number, contentV3, reasonV3)
	must("doctor-b 以最新版本号重新更正", err)
	fmt.Printf("\n【第三次提交·成功】doctor-b 重新确认内容后以当前版本号 %d 提交: 记录成为第 %d 版\n",
		latest.Number, v3.Number)
	fmt.Printf("  新版本标识=%s，上一版本标识=%s（指向第 2 版 %s）=%v\n",
		v3.ID, v3.PrevID, v2.ID, v3.PrevID == v2.ID)
	fmt.Printf("  新正文=%q\n", v3.Content)
	fmt.Printf("  保留第 2 版已确认变化（“已听诊确认”）=%v；体现本次补充（“复诊随访安排”）=%v\n",
		strings.Contains(v3.Content, "已听诊确认"), strings.Contains(v3.Content, "复诊随访安排"))
	fmt.Printf("  更正原因=%q；被拒正文（影像/肺炎）没有被合并进来=%v\n",
		v3.Reason, !strings.Contains(v3.Content, "影像") && !strings.Contains(v3.Content, "肺炎"))

	// ---- 最终核对：三版完整打印，前两版未被覆盖；两次成功更正各有审计 ----
	final := mustHistory(store, doctorA, patient.ID, encounter.ID, draft.ID)
	fmt.Printf("\n最终版本历史（记录标识仍为 %s，全程未变；共 %d 版）:\n",
		draft.ID, len(final.Versions))
	for i, v := range final.Versions {
		printVersion(i, v)
	}
	if len(final.Versions) != 3 {
		die("最终应有三版，实际 %d 版", len(final.Versions))
	}
	got := final.Versions
	if got[0].ID != v1.ID || got[0].Number != 1 || got[0].Content != contentV1 ||
		got[0].PrevID != "" || got[0].Reason != "" {
		die("第 1 版被覆盖或字段异常: %+v", got[0])
	}
	if got[1].ID != v2.ID || got[1].Number != 2 || got[1].Content != contentV2 ||
		got[1].PrevID != v1.ID || got[1].Reason != reasonV2 {
		die("第 2 版被覆盖或字段异常: %+v", got[1])
	}
	if got[2].ID != v3.ID || got[2].Number != 3 || got[2].Content != contentV3 ||
		got[2].PrevID != v2.ID || got[2].Reason != reasonV3 {
		die("第 3 版字段异常: %+v", got[2])
	}
	if final.CurrentVersion.Number != 3 || final.CurrentVersion.ID != v3.ID {
		die("当前版本应为第 3 版，实际=%+v", final.CurrentVersion)
	}
	fmt.Println("核对: 第 3 版接在第 2 版之后；第 1、2 版正文、上一版本标识与原因均原样保留，没有被覆盖")

	correctedAudits := mustCorrectionAudits(store, doctorA, patient.ID)
	if len(correctedAudits) != 2 {
		die("两次成功更正应各有 1 条更正审计（共 2 条），实际 %d 条", len(correctedAudits))
	}
	fmt.Printf("更正审计: 共 %d 条，分别由 %s、%s 发起；两次被拒提交均未留下审计\n",
		len(correctedAudits), correctedAudits[0].ActorID, correctedAudits[1].ActorID)
	if correctedAudits[0].ActorID != doctorA.ID || correctedAudits[1].ActorID != doctorB.ID {
		die("更正审计顺序异常: %+v", correctedAudits)
	}
}

// mustHistory 由内部使用者查询指定就诊下的记录，并按记录标识取回这一条记录的
// 完整视图。查询本身失败、或记录不在结果中，都视为示例准备/核对失败并终止。
func mustHistory(store *clinical.Store, actor clinical.Actor, patientID, encounterID, recordID clinical.ID) clinical.RecordHistory {
	rows, err := store.EncounterRecords(actor, patientID, encounterID)
	must("内部使用者查询就诊记录", err)
	for i := range rows {
		if rows[i].Record.ID == recordID {
			return rows[i]
		}
	}
	die("查询结果中找不到记录 %q", recordID)
	return clinical.RecordHistory{}
}

// mustCorrectionAudits 取回该患者的全部更正审计，按发生顺序旧到新。
func mustCorrectionAudits(store *clinical.Store, actor clinical.Actor, patientID clinical.ID) []clinical.AuditEvent {
	events, err := store.AuditEvents(actor, patientID)
	must("查看审计事件", err)
	var out []clinical.AuditEvent
	for _, e := range events {
		if e.Action == clinical.ActionCorrected {
			out = append(out, e)
		}
	}
	return out
}

// printVersion 逐版打印版本号、版本标识、上一版本标识、更正原因与完整正文。
func printVersion(index int, v clinical.Version) {
	prev := "无（首版由草稿生效）"
	if v.PrevID != "" {
		prev = v.PrevID
	}
	reason := "无（首版没有更正原因）"
	if v.Reason != "" {
		reason = fmt.Sprintf("%q", v.Reason)
	}
	fmt.Printf("  历史[%d]: 版本号=%d，版本标识=%s，上一版本标识=%s，更正原因=%s\n",
		index, v.Number, v.ID, prev, reason)
	fmt.Printf("           正文=%q\n", v.Content)
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

启动方式：在模块根目录执行 `go run ./examples/correction_conflict`（不需要任何
外部服务或参数；示例自建临时数据目录，退出时自动清理）。结果说明（对照一次真实
运行的输出；`rec_`/`ver_` 标识由存储随机生成，每次运行不同，需要核对的是版本号、
正文、原因与各标识之间的**指向关系**，而不是具体字面值）：

```text
准备完成: 1 名合成患者、1 次就诊、1 条已生效诊断
  记录标识 Record.ID = rec_42131172426516508044cf15（记录全程不变，更正不换标识）
  第 1 版: 版本标识 Version.ID = ver_46d1cd2fb3d270af54fb50a8，整数版本号 Version.Number = 1
标识区分: 记录标识标“哪条记录”；版本标识标“哪一个不可变版本”；CorrectRecord 要带的是当前版本的整数版本号 Number，不是前两个标识

两人各自查看: doctor-a 与 doctor-b 取得同一当前版本——版本号=1，版本标识=ver_46d1cd2fb3d270af54fb50a8

【第一次更正·成功】doctor-a 以版本号 1 提交: 记录成为第 2 版
  新版本标识=ver_428e93b98546de06dc36b93e，上一版本标识=ver_46d1cd2fb3d270af54fb50a8（指向第 1 版 ver_46d1cd2fb3d270af54fb50a8）=true
  新正文="合成诊断：急性支气管炎 J20（已听诊确认）"，更正原因="听诊复核确认体征"

【第二次提交·被拒】doctor-b 仍以取得时的版本号 1 提交不同正文: clinical: version conflict: record "rec_42131172426516508044cf15" is at version 2, not 1
  错误类型=ErrConflict；失败返回值是零值版本（版本号=0、版本标识=""），不是已保存的新版本，不能继续使用
被拒后核对: 正式记录仍为第 2 版（版本标识=ver_428e93b98546de06dc36b93e），正文与原因保留第一次更正结果=true；历史版本数=2；更正审计数=1（被拒提交没有新增审计）

【原因空白·被拒】以当前版本号 2 提交、但更正原因为全空白 " \t\n ": clinical: invalid argument: correction reason is required
  错误类型=ErrInvalidArgument（参数本身不合法，与版本是否过期无关）；同样不产生新版本或审计
  失败后核对: 仍为第 2 版、历史 2 版、更正审计 1 条（未增加）
两种失败的区别: ErrConflict=内容与原因都合法、仅版本号过期；ErrInvalidArgument=原因（或正文）空白等参数问题，在冲突检查之前就被拒绝

被拒后重新查看: doctor-b 看到记录当前为第 2 版（版本标识=ver_428e93b98546de06dc36b93e）
  当前正文="合成诊断：急性支气管炎 J20（已听诊确认）"，当前更正原因="听诊复核确认体征"

【第三次提交·成功】doctor-b 重新确认内容后以当前版本号 2 提交: 记录成为第 3 版
  新版本标识=ver_22be5c178822a9a475d8bd4d，上一版本标识=ver_428e93b98546de06dc36b93e（指向第 2 版 ver_428e93b98546de06dc36b93e）=true
  新正文="合成诊断：急性支气管炎 J20（已听诊确认；补充复诊随访安排）"
  保留第 2 版已确认变化（“已听诊确认”）=true；体现本次补充（“复诊随访安排”）=true
  更正原因="冲突后重新查看：保留已听诊确认结论，补充复诊随访安排"；被拒正文（影像/肺炎）没有被合并进来=true

最终版本历史（记录标识仍为 rec_42131172426516508044cf15，全程未变；共 3 版）:
  历史[0]: 版本号=1，版本标识=ver_46d1cd2fb3d270af54fb50a8，上一版本标识=无（首版由草稿生效），更正原因=无（首版没有更正原因）
           正文="合成诊断：急性支气管炎 J20"
  历史[1]: 版本号=2，版本标识=ver_428e93b98546de06dc36b93e，上一版本标识=ver_46d1cd2fb3d270af54fb50a8，更正原因="听诊复核确认体征"
           正文="合成诊断：急性支气管炎 J20（已听诊确认）"
  历史[2]: 版本号=3，版本标识=ver_22be5c178822a9a475d8bd4d，上一版本标识=ver_428e93b98546de06dc36b93e，更正原因="冲突后重新查看：保留已听诊确认结论，补充复诊随访安排"
           正文="合成诊断：急性支气管炎 J20（已听诊确认；补充复诊随访安排）"
核对: 第 3 版接在第 2 版之后；第 1、2 版正文、上一版本标识与原因均原样保留，没有被覆盖
更正审计: 共 2 条，分别由 doctor-a、doctor-b 发起；两次被拒提交均未留下审计
```

主要输出含义：

- **标识与版本号**：开头先打印记录标识（全程不变）、第 1 版的版本标识与整数
  版本号，强调 `CorrectRecord` 第三个参数是整数版本号。
- **哪次成功、哪次被拒**：三次提交分别以“【第一次更正·成功】”“【第二次提交·
  被拒】”“【第三次提交·成功】”标出。被拒那次明确打印 `ErrConflict`，并核对
  返回值是零值版本（版本号 0、标识为空），不会被当成新版本使用。
- **被拒后状态不变**：随后核对正式记录仍为第 2 版、正文与原因保留第一次更正
  结果、历史只有两版、更正审计只有 1 条；被拒提交中的“影像/肺炎”正文没有进入
  正式记录。
- **两种失败可区分**：“原因空白”单独打印 `ErrInvalidArgument`，且即使使用当前
  版本号 2 也被拒；失败后版本数与审计数都不增加。
- **重新查看后成功接版**：被拒者重新查看得到当前第 2 版的正文与原因，再以版本
  号 2 提交完整正文，成功成为第 3 版；第 3 版正文同时保留第 2 版的“已听诊确认”
  和本次“复诊随访安排”，被拒正文没有被自动合并。
- **三版完整历史与两次审计**：最后逐版打印版本号、版本标识、上一版本标识、更正
  原因与正文——第 1 版无上一版本与原因，第 2 版指向第 1 版，第 3 版指向第 2 版；
  前两版未被覆盖，两次成功更正各有自己的更正审计（共 2 条），两次被拒均无审计。

本次只补齐“冲突后重新查看再更正”的使用说明与示例：`CorrectRecord` 的版本号与
原因要求、`ErrConflict`/`ErrInvalidArgument` 语义、`EncounterRecords` 历史查看
及审计行为均沿用已有公开入口与错误语义，草稿、生效、更正、历史查看等既有功能
与其他示例保持不变。

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
- **拒绝原因必须非空白且是合法 UTF-8**：拒绝回执的原因只作空白检查还不够——
  原因夹带任何无效 UTF-8 字节（坏字节位于开头、中间、结尾，或最后一个多字节
  字符没有写完整）一律返回 `ErrInvalidArgument` 与空回执确认，不替换成其他
  字符、不删掉坏字节、也不只保存前半段后继续登记。原因是本地快照要原样落盘
  的字段：若放行坏字节，当次内部查询还能看到原字节，关闭重开后却会变成替换
  字符，原样原因重交也会因此被判为原因不同。失败后交换保持提交前状态，不留下
  拒绝原因、登记时间或新增回执审计，原包与摘要保持原样；接收方随后换用合法
  原因仍能登记这份尚未完成的回执。合法原因（含中文、换行、引号、反斜杠与首尾
  空格）按原文保存；用户明确输入的合法替换字符“�”本身可以保存，它与无效字节
  不是一回事。**接受回执不保存原因**：传入的原因即使夹带无效字节也一律忽略为
  空，既不会因此拒绝接受，也不影响相同接受回执的重交结果。

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
  拒绝回执（拒绝原因必填，且必须是合法 UTF-8），而不是提交被改后的摘要——那样只会得到
  `ErrConflict`。

本次只补齐使用说明与示例：摘要格式、`FetchPackage` 的取包权限判定与
`SubmitReceipt` 的回执判定均保持不变，也没有新增任何交换操作。

## 身份模型

| 身份 | 能力 |
| --- | --- |
| 内部使用者 `InternalActor` | 登记、草稿/生效/更正、授权与撤回、按患者查看授权清单、查看全部草稿、完整历史与审计 |
| 接收方 `ReceiverActor` | 仅在有效授权范围内读取当前生效版本 |

`clinical.Ready()` 保持基线行为，始终返回 `true`。
