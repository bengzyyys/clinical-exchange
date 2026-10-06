// 命令 receipt_after_revoke 演示交换回执的允许范围：接收方已成功取到包之后，
// 绑定授权被撤回，继续取包会被拒绝（ErrAccessDenied，且没有交付内容），
// 但接收方凭此次取包得到的交换标识与摘要仍能登记接受回执。
//
// 全程只使用合成患者资料。运行：
//
//	go run ./examples/receipt_after_revoke
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
