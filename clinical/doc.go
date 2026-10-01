// Package clinical 提供在本机运行的临床档案与授权查阅能力，只处理合成患者资料。
//
// 基本用法是先用 [Open] 在调用方指定的数据目录打开一个 [Store]：
//
//	s, err := clinical.Open("/path/to/data")
//
// 所有写操作都由内部使用者（[InternalActor]）执行：登记患者（Store.RegisterPatient）、
// 登记就诊（Store.AddEncounter）、在就诊下录入诊断或医嘱草稿
// （Store.CreateDraft / Store.UpdateDraft / Store.DeleteDraft），草稿确认后
// 用 Store.ActivateRecord 生效。生效记录不可覆盖或删除，只能用
// Store.CorrectRecord 更正：更正需提供非空原因并指明显式当前版本号，
// 成功后生成新版本，旧内容、版本关系（Version.PrevID）与原因全部保留；
// 指定版本已过期则返回 [ErrConflict] 且状态不变。
//
// 内部使用者用 Store.Chart 查看某患者的全部草稿与完整版本历史；
// 用 Store.Grant 为接收方（[ReceiverActor]）建立按“就诊+类别”限定、
// 带 [开始, 截止) 时间窗的授权，可用 Store.Revoke 提前撤回。
// 接收方用 Store.Read 按自己的身份读取：只返回当前有效授权覆盖记录的
// 当前生效版本，看不到草稿、旧版本或更正原因。
//
// 停用档案（Store.DeactivatePatient）后禁止一切写入与接收方读取，
// 内部使用者仍可查看历史。生效、更正、授权创建与撤回、档案停用都会
// 写入仅供内部使用者按患者查看的审计事件。关闭后用同一目录重新 [Open]，
// 档案、版本、授权状态与审计历史全部保留；授权是否到期始终按本次读取时间判断。
//
// [Ready] 保持基线行为：包可以加载时始终返回 true。
package clinical

// Ready 表示基线可以运行。
func Ready() bool { return true }
