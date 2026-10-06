// 命令 list_authorizations 演示内部使用者如何用 ListAuthorizations 查看
// 一名患者已保存的授权清单。
//
// 同一名合成患者先后向两个接收方授权：给 insurer-1 一条生效中的整类诊断
// 授权、一条尚未开始的限定记录授权、一条随后被撤回的整类医嘱授权，再给
// insurer-2 一条生效中的整类医嘱授权；另备一名同样授权给 insurer-1 的
// 患者，用来确认“按患者 + 接收方”过滤不会混入其他患者的条目。随后演示：
// 不指定接收方与指定接收方两种清单的差异、已撤回授权仍带撤回时间出现在
// 清单中、两种成功返回空清单的情形、患者停用后内部使用者仍能列出历史
// 授权而接收方读取被拒绝，以及两种失败——查询不存在的患者返回
// ErrNotFound，持有有效授权的接收方调用这一内部查询返回 ErrAccessDenied
// 且不提供任何授权资料。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/list_authorizations
package main

import (
	"errors"
	"fmt"
	"os"
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

	// ---- 准备资料：合成患者、一次就诊、一条已生效诊断与一条已生效医嘱 ----
	patient, err := store.RegisterPatient(doctor, "合成患者己")
	must("登记合成患者", err)
	encounter, err := store.AddEncounter(doctor, patient.ID, time.Now())
	must("登记就诊", err)

	diag, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Diagnosis, "合成诊断：高血压 I10")
	must("创建诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag.ID)
	must("生效诊断", err)

	order, err := store.CreateDraft(doctor, patient.ID, encounter.ID,
		clinical.Order, "合成医嘱：低盐饮食")
	must("创建医嘱草稿", err)
	_, err = store.ActivateRecord(doctor, order.ID)
	must("生效医嘱", err)

	// ---- 同一患者向两个接收方建立四条授权，覆盖四种清单状态 ----
	now := time.Now()

	// 授权一：insurer-1，整类诊断，时间窗覆盖当前时刻（生效中）。
	grantActive, err := store.Grant(doctor, patient.ID, insurer1.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Diagnosis}},
		now.Add(-time.Hour), now.Add(24*time.Hour))
	must("建立整类诊断授权（生效中）", err)

	// 授权二：insurer-1，限定记录范围只选那条诊断，时间窗明天才开始（尚未开始）。
	grantFuture, err := store.GrantSelective(doctor, patient.ID, insurer1.ID,
		nil,
		[]clinical.RecordSelection{{
			EncounterID: encounter.ID,
			Category:    clinical.Diagnosis,
			RecordID:    diag.ID,
		}},
		now.Add(24*time.Hour), now.Add(48*time.Hour))
	must("建立限定记录授权（尚未开始）", err)

	// 授权三：insurer-1，整类医嘱，建立后立即撤回（已撤回）。
	grantRevoked, err := store.Grant(doctor, patient.ID, insurer1.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Order}},
		now.Add(-time.Hour), now.Add(24*time.Hour))
	must("建立整类医嘱授权（随后撤回）", err)
	must("撤回整类医嘱授权", store.Revoke(doctor, patient.ID, grantRevoked.ID))

	// 授权四：insurer-2，整类医嘱（生效中），用于对比两个接收方的清单差异。
	grantOther, err := store.Grant(doctor, patient.ID, insurer2.ID,
		[]clinical.Scope{{EncounterID: encounter.ID, Category: clinical.Order}},
		now.Add(-time.Hour), now.Add(24*time.Hour))
	must("建立另一接收方的整类医嘱授权", err)

	// 每条授权的说明标签，打印时按授权标识对照，便于辨认清单中的每一行。
	labels := map[clinical.ID]string{
		grantActive.ID:  "整类诊断授权（生效中）",
		grantFuture.ID:  "限定记录授权（尚未开始）",
		grantRevoked.ID: "整类医嘱授权（已撤回）",
		grantOther.ID:   "整类医嘱授权（生效中，授予 insurer-2）",
	}

	// ---- 只按患者查询：接收方参数传空字符串 ----
	// 列出该患者授予所有接收方的全部授权，含尚未开始与已撤回的条目；
	// 结果按授权标识升序排列，不是按创建时间。
	all, err := store.ListAuthorizations(doctor, patient.ID, "")
	must("列出患者全部授权", err)
	fmt.Printf("不指定接收方: 共 %d 条（含授予两名接收方的授权，含已撤回）\n", len(all))
	for i, a := range all {
		fmt.Printf("  第%d行: %s | 接收方=%s | 整类范围=%d项 限定选择=%d条 | 已撤回=%v | 此刻有效=%v\n",
			i+1, labels[a.ID], a.ReceiverID, len(a.Scopes), len(a.Selections),
			a.RevokedAt != nil, a.ActiveAt(now))
	}
	fmt.Printf("结果按授权标识升序排列: %v\n", sortedByID(all))

	// ---- 再指定接收方：只保留授予该接收方的条目 ----
	forIns1, err := store.ListAuthorizations(doctor, patient.ID, insurer1.ID)
	must("按接收方 insurer-1 过滤", err)
	fmt.Printf("指定接收方 insurer-1: 共 %d 条（授予 insurer-2 的授权被排除，已撤回授权仍在）\n",
		len(forIns1))
	for i, a := range forIns1 {
		fmt.Printf("  第%d行: %s | 接收方=%s | 已撤回=%v\n",
			i+1, labels[a.ID], a.ReceiverID, a.RevokedAt != nil)
	}

	// 另一名患者也授权给同一个接收方 insurer-1，其条目不能混入上面的清单。
	patient2, err := store.RegisterPatient(doctor, "合成患者庚")
	must("登记第二名合成患者", err)
	encounter2, err := store.AddEncounter(doctor, patient2.ID, time.Now())
	must("登记第二名患者的就诊", err)
	diag2, err := store.CreateDraft(doctor, patient2.ID, encounter2.ID,
		clinical.Diagnosis, "合成诊断：2 型糖尿病 E11")
	must("创建第二名患者的诊断草稿", err)
	_, err = store.ActivateRecord(doctor, diag2.ID)
	must("生效第二名患者的诊断", err)
	grantP2, err := store.Grant(doctor, patient2.ID, insurer1.ID,
		[]clinical.Scope{{EncounterID: encounter2.ID, Category: clinical.Diagnosis}},
		now.Add(-time.Hour), now.Add(24*time.Hour))
	must("建立第二名患者授予 insurer-1 的授权", err)

	leaked := false
	for _, a := range forIns1 {
		if a.ID == grantP2.ID {
			leaked = true
		}
	}
	fmt.Printf("另一患者也授权给 insurer-1，其条目混入上一清单: %v（应为 false）\n", leaked)

	// ---- 两种成功返回空清单的情形：不是患者不存在，也不是查询被拒绝 ----
	// 患者存在但没有任何授权。
	patient3, err := store.RegisterPatient(doctor, "合成患者辛")
	must("登记无授权的合成患者", err)
	emptyByPatient, err := store.ListAuthorizations(doctor, patient3.ID, "")
	must("查询没有任何授权的患者", err)
	fmt.Printf("患者存在但没有授权: 成功返回空清单（%d 条）\n", len(emptyByPatient))

	// 指定的接收方从未获得该患者的授权。
	emptyByReceiver, err := store.ListAuthorizations(doctor, patient.ID, "insurer-9")
	must("查询从未获授权的接收方", err)
	fmt.Printf("指定从未获该患者授权的接收方: 成功返回空清单（%d 条）\n", len(emptyByReceiver))

	// ---- 患者停用后：内部使用者仍能列出历史授权，接收方读取被拒绝 ----
	must("停用第二名患者档案", store.DeactivatePatient(doctor, patient2.ID))
	p2List, err := store.ListAuthorizations(doctor, patient2.ID, "")
	must("停用后列出历史授权", err)
	fmt.Printf("患者停用后: 内部使用者仍能列出 %d 条历史授权\n", len(p2List))
	if _, err := store.Read(insurer1, patient2.ID, encounter2.ID, clinical.Diagnosis); !errors.Is(err, clinical.ErrAccessDenied) {
		die("停用后接收方读取应返回 ErrAccessDenied，实际得到 %v", err)
	}
	fmt.Printf("患者停用后: 接收方读取被拒绝（%v），清单可查不代表接收方能读\n", clinical.ErrAccessDenied)

	// ---- 失败一：查询不存在的患者，返回 ErrNotFound ----
	if _, err := store.ListAuthorizations(doctor, "pat_missing", ""); !errors.Is(err, clinical.ErrNotFound) {
		die("查询不存在的患者应返回 ErrNotFound，实际得到 %v", err)
	}
	fmt.Printf("查询不存在的患者: 返回 ErrNotFound（%v）\n", clinical.ErrNotFound)

	// ---- 失败二：接收方不能调用这个内部查询 ----
	// insurer-1 对该患者持有生效中的授权，也不能查看清单：
	// ErrAccessDenied，且不返回任何授权资料。
	rows, err := store.ListAuthorizations(insurer1, patient.ID, insurer1.ID)
	if !errors.Is(err, clinical.ErrAccessDenied) || len(rows) != 0 {
		die("接收方调用内部查询应返回 ErrAccessDenied 且无数据，实际得到 %d 条、%v", len(rows), err)
	}
	fmt.Printf("持有有效授权的接收方调用内部查询: 被拒绝（%v），返回 %d 条，不含任何授权资料\n",
		clinical.ErrAccessDenied, len(rows))
}

// sortedByID 报告清单是否按授权标识严格升序排列。
func sortedByID(auths []clinical.Authorization) bool {
	for i := 1; i < len(auths); i++ {
		if auths[i-1].ID >= auths[i].ID {
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
