// 命令 encounter_records 演示内部使用者如何用 EncounterRecords 查看一次就诊的
// 完整记录历史：同一次就诊中保留一条医嘱草稿，并准备一条已生效、经过两次更正的
// 诊断。内部使用者查询这次就诊后，输出明确对应到这两条记录：
//
//   - 医嘱草稿：展示草稿标记与当前草稿正文，并明确它没有当前生效版本、也没有
//     任何历史版本；
//   - 诊断：展示当前版本号与当前正文，以及从第一个生效版本到两次更正后的全部
//     历史——每个版本的标识、版本号、上一版本标识与更正原因一一对应；首版没有
//     上一版本和更正原因，后续版本各自指向紧邻的上一版；当前版本就是完整历史
//     中的最新一版，不是额外发生的一次更正。
//
// 同时演示两条使用边界：患者停用后内部使用者仍能取得原有草稿与完整历史；接收方
// 即使持有覆盖该就诊的临床内容授权，也不能调用这个内部查询（ErrAccessDenied，
// 结果不携带任何记录）。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/encounter_records
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

// 诊断三个版本的正文与两次更正的原因：两次更正使用不同的正文和原因。
const (
	diagV1Content = "合成诊断：高血压 I10（初诊记录）"
	diagV2Content = "合成诊断：高血压 I10（补录当日血压测量值）"
	diagV3Content = "合成诊断：原发性高血压 I10（复核确认）"
	diagV2Reason  = "第一次更正：补录当日血压测量值"
	diagV3Reason  = "第二次更正：复核后明确诊断名称"

	orderDraftContent = "合成医嘱草稿：待确认的检查项目"
)

func main() {
	dir, err := os.MkdirTemp("", "clinical-encounter-records-")
	must("创建临时数据目录", err)
	defer os.RemoveAll(dir)

	store, err := clinical.Open(dir)
	must("打开本地存储", err)
	defer store.Close()

	doctor := clinical.InternalActor("doctor-1")
	receiver := clinical.ReceiverActor("insurer-1")

	// ---- 准备资料：合成患者、一次就诊、一条经两次更正的诊断、一条医嘱草稿 ----
	patient, err := store.RegisterPatient(doctor, "合成患者辛")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
	must("登记就诊", err)

	diag, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, diagV1Content)
	must("创建诊断草稿", err)
	v1, err := store.ActivateRecord(doctor, diag.ID)
	must("生效诊断（第 1 版）", err)
	// 两次更正都以当时的当前版本号提交：第一次提交第 1 版生效时的版本号，
	// 第二次提交第一次更正产生的新版本号；两次的正文与更正原因各不相同。
	v2, err := store.CorrectRecord(doctor, diag.ID, v1.Number, diagV2Content, diagV2Reason)
	must("第一次更正诊断", err)
	v3, err := store.CorrectRecord(doctor, diag.ID, v2.Number, diagV3Content, diagV3Reason)
	must("第二次更正诊断", err)

	orderDraft, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Order, orderDraftContent)
	must("创建医嘱草稿（保持草稿状态）", err)

	// 接收方持有覆盖该次就诊诊断的有效授权——用于演示即使有临床内容授权，
	// 也不能调用这个内部查询。
	now := time.Now()
	_, err = store.Grant(doctor, patient.ID, receiver.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
		now, now.Add(24*time.Hour))
	must("为接收方建立诊断授权", err)
	fmt.Println("准备完成: 一次就诊下有一条经两次更正的已生效诊断和一条保持草稿状态的医嘱")

	// ---- 内部使用者查询这次就诊，输出明确对应到两条记录 ----
	rows := queryEncounter(store, doctor, patient.ID, encounter.ID, "停用前查询")

	// 记录列表按记录标识升序排列，与录入先后无关：先录入的是诊断，但它在
	// 列表中的位置由随机生成的记录标识决定，不能把列表顺序当作录入时间顺序。
	fmt.Printf("记录列表按记录标识升序排列=%v（先录入诊断、后录入医嘱草稿，列表顺序不表示录入先后）\n",
		idsAscending(rows))

	// 按记录标识定位两条记录，而不是依赖列表位置。
	byID := map[clinical.ID]clinical.RecordHistory{}
	for _, h := range rows {
		byID[h.Record.ID] = h
	}

	// 医嘱草稿：草稿标记与当前草稿正文都在；没有当前生效版本，也没有历史版本。
	d := byID[orderDraft.ID]
	draftShape := d.HasDraft && d.DraftContent == orderDraftContent &&
		d.CurrentVersion == nil && len(d.Versions) == 0 &&
		d.Record.CurrentVersionID == "" && len(d.Record.Versions) == 0
	fmt.Printf("医嘱草稿: HasDraft=%v，当前草稿正文=%q；无当前生效版本且无历史版本=%v\n",
		d.HasDraft, d.DraftContent, draftShape)

	// 诊断：当前版本号与当前正文。
	h := byID[diag.ID]
	currentOK := h.CurrentVersion != nil && h.CurrentVersion.ID == v3.ID &&
		h.CurrentVersion.Number == 3 && h.CurrentVersion.Content == diagV3Content
	fmt.Printf("诊断当前版本: 版本号=%d，当前正文=%q（即第二次更正的结果=%v）\n",
		h.CurrentVersion.Number, h.CurrentVersion.Content, currentOK)

	// 诊断：从第一个生效版本到两次更正后的完整历史，按版本号旧到新。
	fmt.Println("诊断完整历史（按版本号旧到新）:")
	for _, v := range h.Versions {
		fmt.Printf("  第 %d 版: 版本标识=%s，上一版本标识=%q，更正原因=%q，正文=%q\n",
			v.Number, v.ID, v.PrevID, v.Reason, v.Content)
	}
	chainOK := len(h.Versions) == 3 &&
		h.Versions[0].ID == v1.ID && h.Versions[0].Number == 1 &&
		h.Versions[0].PrevID == "" && h.Versions[0].Reason == "" &&
		h.Versions[1].ID == v2.ID && h.Versions[1].Number == 2 &&
		h.Versions[1].PrevID == v1.ID && h.Versions[1].Reason == diagV2Reason &&
		h.Versions[2].ID == v3.ID && h.Versions[2].Number == 3 &&
		h.Versions[2].PrevID == v2.ID && h.Versions[2].Reason == diagV3Reason
	fmt.Printf("版本链核对: 首版无上一版本与更正原因，第 2、3 版各自指向紧邻的上一版并保留各自原因=%v\n",
		chainOK)
	currentIsLatest := h.CurrentVersion.ID == h.Versions[len(h.Versions)-1].ID
	fmt.Printf("当前版本即完整历史中的最新一版（不是额外发生的一次更正）=%v\n", currentIsLatest)

	// ---- 边界一：接收方即使持有临床内容授权，也不能调用这个内部查询 ----
	denied, err := store.EncounterRecords(receiver, patient.ID, encounter.ID)
	if !errors.Is(err, clinical.ErrAccessDenied) || denied != nil {
		die("接收方调用内部查询应返回 ErrAccessDenied 且不携带记录，实际得到 err=%v、%d 条",
			err, len(denied))
	}
	fmt.Printf("持有有效授权的接收方调用内部查询: 被拒绝（%v），结果不携带任何记录\n",
		clinical.ErrAccessDenied)

	// ---- 边界二：患者停用后，内部使用者仍能取得原有草稿与完整历史 ----
	must("停用患者", store.DeactivatePatient(doctor, patient.ID))
	after := queryEncounter(store, doctor, patient.ID, encounter.ID, "停用后查询")

	afterByID := map[clinical.ID]clinical.RecordHistory{}
	for _, h := range after {
		afterByID[h.Record.ID] = h
	}
	ad := afterByID[orderDraft.ID]
	ah := afterByID[diag.ID]
	preserved := len(after) == 2 &&
		ad.HasDraft && ad.DraftContent == orderDraftContent &&
		ad.CurrentVersion == nil && len(ad.Versions) == 0 &&
		ah.CurrentVersion != nil && ah.CurrentVersion.Number == 3 &&
		ah.CurrentVersion.Content == diagV3Content &&
		len(ah.Versions) == 3 &&
		ah.Versions[0].PrevID == "" && ah.Versions[0].Reason == "" &&
		ah.Versions[1].PrevID == ah.Versions[0].ID && ah.Versions[1].Reason == diagV2Reason &&
		ah.Versions[2].PrevID == ah.Versions[1].ID && ah.Versions[2].Reason == diagV3Reason
	fmt.Printf("停用后查询: 记录数=%d，原有草稿与完整历史（含各版正文与更正原因）保持原样=%v\n",
		len(after), preserved)
}

// queryEncounter 执行一次就诊记录查询并打印结果条数；查询失败时明确提示并
// 终止，不继续使用失败的返回值。
func queryEncounter(store *clinical.Store, actor clinical.Actor,
	patientID, encounterID clinical.ID, title string,
) []clinical.RecordHistory {
	rows, err := store.EncounterRecords(actor, patientID, encounterID)
	must(title, err)
	fmt.Printf("%s: 成功，记录数=%d\n", title, len(rows))
	return rows
}

func idsAscending(rows []clinical.RecordHistory) bool {
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Record.ID >= rows[i].Record.ID {
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
