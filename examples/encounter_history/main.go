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
