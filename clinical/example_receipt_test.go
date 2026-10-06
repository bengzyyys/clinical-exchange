package clinical_test

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bengzyyys/clinical-exchange/clinical"
)

// 接收方已取到包、随后绑定授权被撤回、最后登记接受回执：撤回会立即阻止
// 继续取包，但不会撤销指定接收方凭此前取包得到的摘要确认这次交换的资格；
// 回执成功也不会恢复授权。
func ExampleStore_SubmitReceipt_afterAuthorizationRevoked() {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	dir, err := os.MkdirTemp("", "clinical-exchange-example")
	must(err)
	defer os.RemoveAll(dir)

	s, err := clinical.Open(dir)
	must(err)
	defer s.Close()

	doc := clinical.InternalActor("doctor-1")
	ins := clinical.ReceiverActor("insurer-1")
	other := clinical.ReceiverActor("insurer-2")

	// 内部使用者准备合成患者资料：一条已生效诊断和覆盖它的有效授权。
	p, err := s.RegisterPatient(doc, "合成患者甲")
	must(err)
	e, err := s.AddEncounter(doc, p.ID, time.Now())
	must(err)
	r, err := s.CreateDraft(doc, p.ID, e.ID, clinical.Diagnosis, "高血压 I10")
	must(err)
	if _, err := s.ActivateRecord(doc, r.ID); err != nil {
		panic(err)
	}
	now := time.Now()
	a, err := s.Grant(doc, p.ID, ins.ID,
		[]clinical.Scope{{EncounterID: e.ID, Category: clinical.Diagnosis}},
		now, now.Add(24*time.Hour))
	must(err)

	// 创建交换，由指定接收方取包；取包成功后才允许使用交付结果继续。
	x, err := s.CreateExchange(doc, p.ID, ins.ID, a.ID, []clinical.ID{r.ID}, "req-2026-0001")
	must(err)
	del, err := s.FetchPackage(ins, x.ID)
	must(err)
	fmt.Println("取包状态:", del.Status, "记录数:", len(del.Package.Records))

	// 取包之后撤回绑定授权：再次取包被拒，交付为空（不含包内容与摘要）。
	must(s.Revoke(doc, p.ID, a.ID))
	denied, err := s.FetchPackage(ins, x.ID)
	fmt.Println("撤回后取包被拒:", errors.Is(err, clinical.ErrAccessDenied),
		"交付为空:", denied.Digest == "" && len(denied.Package.Records) == 0)

	// 两个失败条件：摘要与这次交换不符返回 ErrConflict；换成其他接收方
	// 登记返回 ErrAccessDenied。两者都不改变交换状态，也不增加回执审计。
	auditBefore, err := s.AuditEvents(doc, p.ID)
	must(err)
	_, err = s.SubmitReceipt(ins, del.ExchangeID, "deadbeef", clinical.ReceiptAccepted, "")
	fmt.Println("摘要不符:", errors.Is(err, clinical.ErrConflict))
	_, err = s.SubmitReceipt(other, del.ExchangeID, del.Digest, clinical.ReceiptAccepted, "")
	fmt.Println("其他接收方:", errors.Is(err, clinical.ErrAccessDenied))
	mid, err := s.GetExchange(doc, p.ID, x.ID)
	must(err)
	auditMid, err := s.AuditEvents(doc, p.ID)
	must(err)
	fmt.Println("失败尝试后状态:", mid.Status, "已有回执:", mid.Receipt != nil,
		"审计未增加:", len(auditMid) == len(auditBefore))

	// 指定接收方凭这次成功取包得到的交换标识与摘要登记接受回执。
	// 确认只含交换标识、状态、回执结果与登记时间，不含任何包内容。
	conf, err := s.SubmitReceipt(ins, del.ExchangeID, del.Digest, clinical.ReceiptAccepted, "")
	must(err)
	fmt.Println("回执确认:", conf.Status, conf.Outcome, "登记时间有效:", !conf.RegisteredAt.IsZero())

	// 回执成功不会恢复授权：下一次取包仍然被拒。
	_, err = s.FetchPackage(ins, x.ID)
	fmt.Println("回执后取包仍被拒:", errors.Is(err, clinical.ErrAccessDenied))

	// 内部使用者查看这次交换：正式回执已登记，原包与摘要保持原值。
	got, err := s.GetExchange(doc, p.ID, x.ID)
	must(err)
	fmt.Println("内部查看:", got.Status, got.Receipt.Outcome,
		"摘要未变:", got.Digest == x.Digest,
		"内容未变:", got.Package.Records[0].Content == "高血压 I10")

	// Output:
	// 取包状态: pending_receipt 记录数: 1
	// 撤回后取包被拒: true 交付为空: true
	// 摘要不符: true
	// 其他接收方: true
	// 失败尝试后状态: pending_receipt 已有回执: false 审计未增加: true
	// 回执确认: accepted accepted 登记时间有效: true
	// 回执后取包仍被拒: true
	// 内部查看: accepted accepted 摘要未变: true 内容未变: true
}
