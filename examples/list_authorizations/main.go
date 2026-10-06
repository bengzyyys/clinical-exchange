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
