// 命令 draft_update 演示内部使用者如何用 UpdateDraft 修改尚未生效的诊断草稿，
// 以及“保存草稿”与“正式生效”对共享内容的不同影响。
//
// 示例自行准备本地存储、一名合成患者与一次就诊：就诊中一条诊断已经生效，
// 另一条同类诊断仍是草稿（本示例的修改目标），并为接收方建立覆盖这次就诊
// 诊断的有效整类授权。随后依次演示：
//   - 用 UpdateDraft 替换目标草稿的完整正文：新正文含中文、换行与有意保留的
//     首尾空格，逐字原样保存；记录标识不变，仍没有当前生效版本，历史版本为空，
//     另一条已生效诊断不受影响；保存草稿不新增版本或审计事件；
//   - 接收方在有效授权下读取：修改前后都只得到已生效的那条诊断，得不到目标
//     草稿的正文或标识；
//   - 目标仍是草稿时，提交仅含空格、换行的正文返回 ErrInvalidArgument，先前
//     成功保存的草稿仍保留；正文还必须是合法 UTF-8，非法内容同样被拒绝，
//     不会部分保存；
//   - 把修改后的草稿生效：第一个正式版本号为 1，采用最后一次成功保存的完整
//     正文，旧草稿文字不会成为历史版本，接收方此时才读到目标诊断；
//   - 对已生效记录再用 UpdateDraft 返回 ErrActive，当前内容与版本历史不变，
//     后续修改应使用 CorrectRecord。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/draft_update
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

// 本示例用到的正文。draftRevised 含中文、换行与有意保留的首尾空格，
// 用来展示 UpdateDraft 逐字原样保存、不去除空白。
const (
	activeContent = "合成诊断甲：高血压 I10"
	draftInitial  = "合成诊断乙（草稿）：2 型糖尿病 E11，初步意见待复核"
	draftRevised  = "  合成诊断乙：2 型糖尿病 E11\n  复核意见：建议三个月后随访复查  "

	// blankContent 去掉空白后为空，只用于演示“正文全为空白”的失败。
	blankContent = " \n\t "
)

// invalidUTF8Content 末尾是一个没有写完整的多字节字符（0xE4 0xB8 缺少后续
// 字节）。用拼接构造，避免源码文件本身出现非法 UTF-8。
var invalidUTF8Content = "合成诊断乙：2 型糖尿病 E11（备注" + string([]byte{0xE4, 0xB8})

func main() {
	dir, err := os.MkdirTemp("", "clinical-draft-update-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")

	// ---- 准备资料：一名合成患者、一次就诊、一条已生效诊断、一条诊断草稿 ----
	patient, err := store.RegisterPatient(doctor, "合成患者癸")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
	must("登记就诊", err)

	// 记录一（诊断甲）：创建后即生效，整个示例期间保持第 1 版不变。
	diagActive, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, activeContent)
	must("创建诊断甲草稿", err)
	vActive, err := store.ActivateRecord(doctor, diagActive.ID)
	must("生效诊断甲（第 1 版）", err)

	// 记录二（诊断乙）：本示例的修改目标，先生效前一直是草稿。
	diagTarget, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, draftInitial)
	must("创建诊断乙草稿（修改目标）", err)

	// 为接收方建立覆盖本次就诊诊断的有效整类授权，时间窗 [now, now+24h)。
	now := time.Now()
	grant, err := store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
		now, now.Add(24*time.Hour))
	must("为接收方建立本次就诊诊断的有效整类授权", err)

	fmt.Println("准备完成: 1 名合成患者、1 次就诊；1 条已生效诊断 + 1 条诊断草稿；接收方持有效整类授权")
	fmt.Printf("  诊断甲（已生效）记录标识=%s，第 1 版版本标识=%s\n", diagActive.ID, vActive.ID)
	fmt.Printf("  诊断乙（修改目标，仍是草稿）记录标识=%s，初始草稿正文=%q\n", diagTarget.ID, draftInitial)
	fmt.Printf("  接收方授权=%s（整类：本次就诊全部诊断）\n", grant.ID)

	// ---- 修改前的内部就诊记录视图 ----
	targetBefore := mustHistory(store, doctor, patient.ID, encounter.ID, diagTarget.ID)
	activeBefore := mustHistory(store, doctor, patient.ID, encounter.ID, diagActive.ID)
	fmt.Println("\n【修改前·内部视图】")
	printDraftState("诊断乙（目标草稿）", targetBefore)
	printActiveState("诊断甲（已生效）", activeBefore)

	// 修改前接收方读取：只得到已生效的诊断甲，得不到目标草稿。
	resBefore := mustRead(store, receiver, patient.ID, encounter.ID)
	fmt.Println("\n【修改前·接收方读取】")
	printReceiverView(resBefore)
	requireReceiverSeesOnlyActive(resBefore, diagActive.ID, diagTarget.ID)

	// ---- 用 UpdateDraft 替换目标草稿的完整正文 ----
	// 新正文含中文、换行与有意保留的首尾空格，全部逐字原样保存。
	auditsBeforeUpdate := mustAuditCount(store, doctor, patient.ID)
	updated, err := store.UpdateDraft(doctor, diagTarget.ID, draftRevised)
	must("保存诊断乙的新草稿正文", err)
	if updated.ID != diagTarget.ID || updated.DraftContent != draftRevised {
		die("UpdateDraft 返回的记录应标识不变、正文为新内容，实际 %+v", updated)
	}
	fmt.Printf("\n【保存草稿·成功】UpdateDraft 替换完整正文: 记录标识=%s（不变），新草稿正文=%q\n",
		updated.ID, updated.DraftContent)
	fmt.Println("  新正文含中文、换行与首尾各两个空格，逐字原样保存（上方带引号的输出中可见换行与首尾空格）")

	// ---- 修改后的内部就诊记录视图：标识不变，仍是草稿，另一条诊断不受影响 ----
	targetAfter := mustHistory(store, doctor, patient.ID, encounter.ID, diagTarget.ID)
	activeAfter := mustHistory(store, doctor, patient.ID, encounter.ID, diagActive.ID)
	fmt.Println("\n【修改后·内部视图】")
	printDraftState("诊断乙（目标草稿）", targetAfter)
	printActiveState("诊断甲（已生效）", activeAfter)
	if targetAfter.Record.ID != diagTarget.ID || targetAfter.DraftContent != draftRevised ||
		targetAfter.CurrentVersion != nil || len(targetAfter.Versions) != 0 {
		die("保存草稿后目标记录应标识不变、正文为新内容、仍无生效版本与历史，实际 %+v", targetAfter)
	}
	if activeAfter.CurrentVersion == nil || activeAfter.CurrentVersion.ID != vActive.ID ||
		activeAfter.CurrentVersion.Content != activeContent || len(activeAfter.Versions) != 1 {
		die("保存草稿不应影响已生效的诊断甲，实际 %+v", activeAfter)
	}
	auditsAfterUpdate := mustAuditCount(store, doctor, patient.ID)
	if auditsAfterUpdate != auditsBeforeUpdate {
		die("保存草稿不应新增审计事件，实际 %d -> %d", auditsBeforeUpdate, auditsAfterUpdate)
	}
	fmt.Printf("保存草稿的影响: 不新增版本（历史仍为 %d 版）、不新增审计事件（%d 条不变）；"+
		"这是对草稿的保存，不是对生效记录的更正\n",
		len(targetAfter.Versions), auditsAfterUpdate)

	// 修改后接收方读取：仍只得到诊断甲，得不到目标草稿的正文或标识。
	resAfterUpdate := mustRead(store, receiver, patient.ID, encounter.ID)
	fmt.Println("\n【修改后·接收方读取】")
	printReceiverView(resAfterUpdate)
	requireReceiverSeesOnlyActive(resAfterUpdate, diagActive.ID, diagTarget.ID)
	fmt.Println("  保存草稿对接收方不可见: 修改前后读取结果一致，目标草稿的正文与标识都不出现")

	// ---- 预期失败一：目标仍是草稿时，提交仅含空格、换行的正文 ----
	blank, err := store.UpdateDraft(doctor, diagTarget.ID, blankContent)
	switch {
	case err == nil:
		die("全空白正文应被拒绝，却保存成功: %+v", blank)
	case errors.Is(err, clinical.ErrInvalidArgument):
		fmt.Printf("\n【全空白正文·被拒】UpdateDraft 提交仅含空格、换行的正文 %q: %v\n", blankContent, err)
	default:
		die("全空白正文应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	afterBlank := mustHistory(store, doctor, patient.ID, encounter.ID, diagTarget.ID)
	if afterBlank.DraftContent != draftRevised {
		die("全空白被拒后先前保存的草稿应保留，实际正文=%q", afterBlank.DraftContent)
	}
	fmt.Printf("  先前成功保存的草稿仍保留: 正文=%q\n", afterBlank.DraftContent)

	// ---- 预期失败二：正文夹带非法 UTF-8 ----
	invalid, err := store.UpdateDraft(doctor, diagTarget.ID, invalidUTF8Content)
	switch {
	case err == nil:
		die("非法 UTF-8 正文应被拒绝，却保存成功: %+v", invalid)
	case errors.Is(err, clinical.ErrInvalidArgument):
		fmt.Printf("\n【非法 UTF-8·被拒】UpdateDraft 提交末尾多字节字符未写完整的正文: %v\n", err)
	default:
		die("非法 UTF-8 正文应返回 ErrInvalidArgument，实际得到 %v", err)
	}
	afterInvalid := mustHistory(store, doctor, patient.ID, encounter.ID, diagTarget.ID)
	if afterInvalid.DraftContent != draftRevised {
		die("非法 UTF-8 被拒后草稿不应被部分保存，实际正文=%q", afterInvalid.DraftContent)
	}
	fmt.Printf("  非法内容不会部分保存: 草稿仍为先前保存的正文=%q\n", afterInvalid.DraftContent)

	// ---- 把修改后的草稿生效：第 1 版采用最后一次成功保存的完整正文 ----
	v1, err := store.ActivateRecord(doctor, diagTarget.ID)
	must("生效诊断乙（第 1 版）", err)
	if v1.Number != 1 || v1.Content != draftRevised || v1.PrevID != "" || v1.Reason != "" {
		die("生效应产生第 1 版且内容为最后保存的草稿，实际 %+v", v1)
	}
	fmt.Printf("\n【生效·成功】诊断乙生效为第 %d 版: 版本标识=%s，上一版本=无，更正原因=无\n",
		v1.Number, v1.ID)
	fmt.Printf("  第 1 版正文=%q（即最后一次成功保存的草稿，首尾空格与换行原样进入正式版本）\n",
		v1.Content)
	targetActivated := mustHistory(store, doctor, patient.ID, encounter.ID, diagTarget.ID)
	if targetActivated.HasDraft || len(targetActivated.Versions) != 1 ||
		targetActivated.Versions[0].Content != draftRevised {
		die("生效后应无草稿、历史只有第 1 版，实际 %+v", targetActivated)
	}
	fmt.Printf("  旧草稿文字 %q 不会成为历史版本: 历史版本数=%d（只有生效产生的第 1 版）\n",
		draftInitial, len(targetActivated.Versions))

	// 生效后接收方才读到目标诊断。
	resActivated := mustRead(store, receiver, patient.ID, encounter.ID)
	fmt.Println("\n【生效后·接收方读取】")
	printReceiverView(resActivated)
	var targetSeen *clinical.EffectiveRecord
	for i := range resActivated.Records {
		if resActivated.Records[i].RecordID == diagTarget.ID {
			targetSeen = &resActivated.Records[i]
		}
	}
	if targetSeen == nil || targetSeen.Version != 1 || targetSeen.Content != draftRevised {
		die("生效后接收方应读到诊断乙的第 1 版且正文为最后保存的草稿，实际 %+v", resActivated.Records)
	}
	fmt.Printf("  接收方此时才读到诊断乙: 版本=%d，正文与生效版本逐字一致=%v\n",
		targetSeen.Version, targetSeen.Content == v1.Content)

	// ---- 预期失败三：对已生效记录再走草稿路径 ----
	attempt, err := store.UpdateDraft(doctor, diagTarget.ID, "合成诊断乙：2 型糖尿病 E11（试图直接覆盖）")
	switch {
	case err == nil:
		die("已生效记录不应再接受 UpdateDraft，却保存成功: %+v", attempt)
	case errors.Is(err, clinical.ErrActive):
		fmt.Printf("\n【已生效记录·被拒】对诊断乙再次 UpdateDraft: %v\n", err)
	default:
		die("已生效记录 UpdateDraft 应返回 ErrActive，实际得到 %v", err)
	}
	final := mustHistory(store, doctor, patient.ID, encounter.ID, diagTarget.ID)
	if final.CurrentVersion == nil || final.CurrentVersion.Number != 1 ||
		final.CurrentVersion.Content != draftRevised || len(final.Versions) != 1 {
		die("ErrActive 后当前内容与版本历史应不变，实际 %+v", final)
	}
	fmt.Printf("  当前内容与版本历史不变: 当前版本=%d，历史版本数=%d；"+
		"后续修改已生效记录应使用 CorrectRecord（带当前版本号与非空更正原因）\n",
		final.CurrentVersion.Number, len(final.Versions))
}

// printDraftState 打印草稿形态的记录：草稿标记、当前草稿正文，并核对没有
// 当前生效版本、没有历史版本。
func printDraftState(label string, h clinical.RecordHistory) {
	fmt.Printf("  %s: 记录标识=%s，草稿标记=%v，草稿正文=%q\n",
		label, h.Record.ID, h.HasDraft, h.DraftContent)
	if h.CurrentVersion != nil {
		die("%s不应有当前生效版本，实际 %+v", label, h.CurrentVersion)
	}
	if len(h.Versions) != 0 {
		die("%s不应有历史版本，实际 %+v", label, h.Versions)
	}
	fmt.Printf("    当前生效版本: 无（CurrentVersion 为 nil）；历史版本: 无（共 %d 版）\n",
		len(h.Versions))
}

// printActiveState 打印已生效记录的当前版本号、版本标识与正文。
func printActiveState(label string, h clinical.RecordHistory) {
	if h.CurrentVersion == nil {
		die("%s应有当前生效版本", label)
	}
	fmt.Printf("  %s: 记录标识=%s，当前版本=%d，版本标识=%s，正文=%q，历史版本数=%d\n",
		label, h.Record.ID, h.CurrentVersion.Number, h.CurrentVersion.ID,
		h.CurrentVersion.Content, len(h.Versions))
}

// printReceiverView 逐条打印接收方读到的记录标识、版本号与正文。
func printReceiverView(res clinical.ReadResult) {
	fmt.Printf("  记录数=%d\n", len(res.Records))
	for _, r := range res.Records {
		fmt.Printf("    记录标识=%s，版本=%d，正文=%q\n", r.RecordID, r.Version, r.Content)
	}
}

// requireReceiverSeesOnlyActive 核对接收方只读到已生效的诊断甲：目标草稿的
// 标识与正文都不出现在结果中。
func requireReceiverSeesOnlyActive(res clinical.ReadResult, activeID, targetID clinical.ID) {
	if len(res.Records) != 1 || res.Records[0].RecordID != activeID {
		die("接收方应只读到已生效的诊断甲 %q，实际 %+v", activeID, res.Records)
	}
	for _, r := range res.Records {
		if r.RecordID == targetID {
			die("接收方不应看到目标草稿的标识 %q", targetID)
		}
	}
}

// mustHistory 由内部使用者查询指定就诊下的记录，并按记录标识取回这一条记录的
// 完整视图。查询失败或记录不在结果中，都视为示例失败并终止。
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

// mustRead 由接收方读取本次就诊的诊断；本示例中接收方始终持有有效授权，
// 读取失败视为示例失败并终止。
func mustRead(store *clinical.Store, receiver clinical.Actor, patientID, encounterID clinical.ID) clinical.ReadResult {
	res, err := store.Read(receiver, patientID, encounterID, clinical.Diagnosis)
	must("接收方读取本次就诊诊断", err)
	return res
}

// mustAuditCount 返回该患者当前的审计事件总数。
func mustAuditCount(store *clinical.Store, actor clinical.Actor, patientID clinical.ID) int {
	events, err := store.AuditEvents(actor, patientID)
	must("查看审计事件", err)
	return len(events)
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
